// Package authstate 维护「以什么身份发请求」这件事的唯一真值（W1 / T-16）。
//
// 问题：此前「身份」散落在三处——凭据表里的一行、worker 手写在 curl 里的
// Cookie、以及它脑子里「我现在是 admin」。三者没有关联，于是出现两类错误：
//
//  1. **越权结论不可复现。** worker 说「用低权账号访问到了 admin 接口」，但没人
//     知道那条 Cookie 属于哪个身份、是什么时候拿到的、现在还失效没有。
//  2. **鉴权变更后旧结论仍被引用。** 目标重新登录导致会话轮换，之前所有基于旧
//     会话的结论事实上已经不可复现——但系统里没有任何标记，它们看起来一样新。
//
// 本包只做两件事：给身份一个稳定的名字（Principal）和一个版本号（AuthVersion）。
// 版本号是这里唯一"聪明"的地方：**每次凭据/会话变化就 +1**，于是任何引用
// 「身份@版本」的结论都能自动判断是否还成立——不需要事后逐个复核。
//
// 刻意不做：不存凭据明文、不发请求、不做权限推断。它是账本，不是执行器。
package authstate

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Identity 是一个可复现的请求身份。
type Identity struct {
	// Principal 是身份名（如 "admin@corp" / "user:lowpriv"）。稳定、可读、唯一。
	Principal string `json:"principal"`
	// Version 是鉴权版本。凭据或会话变化时递增；引用旧版本的结论视为不可复现。
	Version int `json:"version"`
	// Headers 是发请求时要带的自定义头（如 X-API-Key）。不含 Cookie——
	// Cookie 放 Cookies，因为它的失效语义与普通头不同。
	Headers map[string]string `json:"headers,omitempty"`
	// Cookies 是 Cookie 名→值。
	Cookies map[string]string `json:"cookies,omitempty"`
	// Roles 是已知的角色/权限标签（来自目标响应或人工标注），用于越权判断。
	Roles []string `json:"roles,omitempty"`
	// Source 说明这份身份从哪来（"credential #12" / "manual" / "register"）。
	Source string `json:"source,omitempty"`
	// UpdatedAt 是最后一次变更时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// Clone 深拷贝（调用方拿到的永远是可改的副本）。
func (i Identity) Clone() Identity {
	out := i
	out.Headers = cloneMap(i.Headers)
	out.Cookies = cloneMap(i.Cookies)
	out.Roles = append([]string(nil), i.Roles...)
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Ref 是「身份@版本」的短引用，用于结论溯源。
func (i Identity) Ref() string {
	if i.Principal == "" {
		return ""
	}
	return fmt.Sprintf("%s@%d", i.Principal, i.Version)
}

// HasRole 判断是否具备某个角色（大小写不敏感）。
func (i Identity) HasRole(role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	for _, r := range i.Roles {
		if strings.ToLower(strings.TrimSpace(r)) == role {
			return true
		}
	}
	return false
}

// Store 是身份的并发安全账本。
//
// 为什么是内存：身份是「当前这次测试上下文」的一部分，不跨进程、不需要持久化。
// 需要留痕的是**结论**（它引用 Ref），而不是身份本身——把身份也落库会引入
// 「库里的身份和实际发出去的请求不一致」这种最难查的问题。
type Store struct {
	mu   sync.RWMutex
	byID map[string]Identity
}

// NewStore 构造空账本。
func NewStore() *Store {
	return &Store{byID: map[string]Identity{}}
}

// Put 写入（或替换）一个身份。首次写入版本为 1；替换时版本取「旧版本+1」，
// 除非显式给了更大的 Version。
//
// 为什么替换必须递增：同一 principal 的凭据变了（口令改了、会话轮换了），
// 基于旧凭据的结论就不再可复现。递增版本让这件事自动可见。
func (s *Store) Put(id Identity) (Identity, error) {
	if strings.TrimSpace(id.Principal) == "" {
		return Identity{}, fmt.Errorf("principal 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.byID[id.Principal]
	if !existed {
		if id.Version <= 0 {
			id.Version = 1
		}
	} else {
		next := prev.Version + 1
		if id.Version > next {
			next = id.Version
		}
		id.Version = next
	}
	id.UpdatedAt = time.Now()
	s.byID[id.Principal] = id.Clone()
	return id.Clone(), nil
}

// Get 读取身份副本；不存在返回 (零值, false)。
func (s *Store) Get(principal string) (Identity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byID[principal]
	if !ok {
		return Identity{}, false
	}
	return id.Clone(), true
}

// Bump 让某个身份失效并递增版本（会话轮换、口令变更、被登出时调用）。
// 返回新版本；身份不存在返回 0。
func (s *Store) Bump(principal string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byID[principal]
	if !ok {
		return 0
	}
	id.Version++
	id.UpdatedAt = time.Now()
	s.byID[principal] = id
	return id.Version
}

// Stale 判断一个「身份@版本」引用是否已过期。
//
// 三种情况都算过期（stale）：
//   - 身份不存在了；
//   - 版本比当前低（凭据变过）；
//   - 引用为空（没有溯源的结论，无法确认可复现）。
//
// 返回的第二个值是原因，直接进报告——「为什么这条结论不可复现」比「它过期了」
// 有用得多。
func (s *Store) Stale(ref string) (bool, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true, "结论没有引用任何身份，无法确认可复现"
	}
	principal, verStr := ref, ""
	if i := strings.LastIndex(ref, "@"); i > 0 {
		principal, verStr = ref[:i], ref[i+1:]
	}
	id, ok := s.Get(principal)
	if !ok {
		return true, fmt.Sprintf("身份 %q 已不在账本中（可能已失效）", principal)
	}
	if verStr == "" {
		return false, "" // 只引用 principal：可接受，按当前版本算
	}
	ver := 0
	if _, err := fmt.Sscanf(verStr, "%d", &ver); err != nil {
		return true, fmt.Sprintf("身份引用 %q 的版本号无法解析", ref)
	}
	if ver < id.Version {
		return true, fmt.Sprintf("身份 %s 的凭据已变更（引用 v%d，当前 v%d）：基于旧凭据的结论需要重验", principal, ver, id.Version)
	}
	return false, ""
}

// List 返回全部身份（按 principal 升序，保证确定性）。
func (s *Store) List() []Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Identity, 0, len(s.byID))
	for _, id := range s.byID {
		out = append(out, id.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Principal < out[j].Principal })
	return out
}

// HeaderPairs 返回可直接用于发请求的头列表（含 Cookie 合并）。
//
// 单独提供它是因为「有身份」和「发请求」之间最容易出错：忘了带 Cookie、
// 或把 Cookie 拼成两个头。这里统一拼一次。
func (i Identity) HeaderPairs() [][2]string {
	out := make([][2]string, 0, len(i.Headers)+1)
	keys := make([]string, 0, len(i.Headers))
	for k := range i.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, [2]string{k, i.Headers[k]})
	}
	if len(i.Cookies) > 0 {
		names := make([]string, 0, len(i.Cookies))
		for k := range i.Cookies {
			names = append(names, k)
		}
		sort.Strings(names) // 固定顺序：Cookie 顺序变了会让服务端缓存/指纹变化
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, n+"="+i.Cookies[n])
		}
		out = append(out, [2]string{"Cookie", strings.Join(parts, "; ")})
	}
	return out
}
