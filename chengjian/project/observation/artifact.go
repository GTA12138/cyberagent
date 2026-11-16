package observation

// artifact.go 是观测原文的留存层。
//
// 设计约束来自 I3 验收的第一句：「Observation 保存 … 原始 artifact_ref + hash；
// **hash 不能替代可读取原文**」。所以这里有两件事必须同时成立：
//
//   - 原文要能读回（Read），且读回时必须重新校验 hash。校验失败要报错，
//     不能把「内容已经和当时不一样」的文件当成当时的证据返回——那会让
//     一次静默的文件损坏变成一条看起来可信的假证据。
//   - ref 必须是**相对路径**而不是绝对路径。绝对路径会让数据目录一搬就全失效，
//     而失效的证据比没有证据更糟（它看起来还在）。
//
// 内容寻址（路径由 sha256 决定）带来一个附带好处：同一条命令重跑两次、输出
// 完全相同时只占一份空间，且「这两次观测的证据是同一段文字」这件事在路径上
// 就能看出来。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxArtifactBytes 是单份观测原文的上限。
//
// 侦察命令输出通常在几十 KB 量级；4 MiB 足够覆盖被截断前的完整输出，
// 同时保证一次异常命令（cat 了一个大文件）不会把数据目录写满。
const MaxArtifactBytes = 4 << 20

// ArtifactStore 是内容寻址的观测原文库。
type ArtifactStore struct {
	root string
	mu   sync.Mutex // 只保护目录创建，不保护读（内容寻址天然幂等）
}

// NewArtifactStore 在 root 下建立（或复用）原文库。
func NewArtifactStore(root string) *ArtifactStore { return &ArtifactStore{root: root} }

// Root 返回库根目录，供备份/迁移使用。
func (s *ArtifactStore) Root() string { return s.root }

// Put 写入一段原文，返回相对 ref、sha256 与字节数。
//
// 空内容也会被写入：一条「命令跑了但没有任何输出」的观测本身就是证据
// （它支撑「这个目标没有 ARP 表」这类否定结论），不能因为空就当成没采集。
func (s *ArtifactStore) Put(content []byte) (ref, hash string, size int64, err error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return "", "", 0, errors.New("observation: 原文库未配置")
	}
	if int64(len(content)) > MaxArtifactBytes {
		return "", "", 0, fmt.Errorf("observation: 原文超限 %d > %d 字节", len(content), MaxArtifactBytes)
	}
	sum := sha256.Sum256(content)
	hash = hex.EncodeToString(sum[:])
	ref = filepath.ToSlash(filepath.Join("obs", hash[:2], hash+".txt"))
	path := filepath.Join(s.root, filepath.FromSlash(ref))

	if st, statErr := os.Stat(path); statErr == nil && st.Size() == int64(len(content)) {
		// 已经存在且长度一致 → 内容寻址保证内容也一致，直接复用。
		return ref, hash, st.Size(), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	stage := filepath.Join(s.root, ".staging")
	if err = os.MkdirAll(stage, 0o700); err != nil {
		return "", "", 0, fmt.Errorf("observation: 建立暂存目录失败: %w", err)
	}
	f, err := os.CreateTemp(stage, "obs-")
	if err != nil {
		return "", "", 0, fmt.Errorf("observation: 创建暂存文件失败: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err = f.Write(content); err != nil {
		_ = f.Close()
		return "", "", 0, fmt.Errorf("observation: 写入原文失败: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return "", "", 0, fmt.Errorf("observation: 落盘原文失败: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", "", 0, fmt.Errorf("observation: 关闭原文失败: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", 0, fmt.Errorf("observation: 建立原文目录失败: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return "", "", 0, fmt.Errorf("observation: 安装原文失败: %w", err)
	}
	return ref, hash, int64(len(content)), nil
}

// Read 读回原文并校验 hash。
//
// expectedHash 为空表示调用方没记 hash——此时仍返回内容，但**不做校验**，
// 并把这件事说清楚（返回的 verified=false）。让「没法校验」显式可见，
// 比让它看起来像校验过了要好。
func (s *ArtifactStore) Read(ref, expectedHash string) (content []byte, verified bool, err error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return nil, false, errors.New("observation: 原文库未配置")
	}
	clean, err := safeRelRef(ref)
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(s.root, filepath.FromSlash(clean))
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 原文不在 ≠ 内容为空。这条必须能区分：把缺失读成空会让
			// 「输出被清理掉了」变成「目标什么都没有」。
			return nil, false, fmt.Errorf("observation: 原文不存在（%s），不能当成空输出: %w", ref, err)
		}
		return nil, false, err
	}
	defer f.Close()
	limited := io.LimitReader(f, MaxArtifactBytes+1)
	content, err = io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > MaxArtifactBytes {
		return nil, false, fmt.Errorf("observation: 原文超过上限，拒绝整份读入")
	}
	if strings.TrimSpace(expectedHash) == "" {
		return content, false, nil
	}
	sum := sha256.Sum256(content)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(expectedHash)) {
		return nil, false, fmt.Errorf("observation: 原文校验失败（内容与记录的 hash 不一致），拒绝作为证据返回")
	}
	return content, true, nil
}

// safeRelRef 拒绝绝对路径与 .. 穿越。
//
// 这一层不是为了防「内网里的攻击者」，是为了防「上层拼错一个 ref」把
// 数据目录外的文件读进证据链——那会让证据系统变成任意文件读取。
func safeRelRef(ref string) (string, error) {
	r := strings.TrimSpace(ref)
	if r == "" {
		return "", errors.New("observation: 空 ref")
	}
	r = filepath.ToSlash(r)
	if strings.HasPrefix(r, "/") || strings.Contains(r, "..") {
		return "", fmt.Errorf("observation: 非法 ref %q", ref)
	}
	return r, nil
}
