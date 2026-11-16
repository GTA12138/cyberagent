// evidence_slice.go 证据切片锚定（P4-1）。
//
// 问题：一条流量可能 100KB，真正支撑漏洞结论的往往只有几十字节（一个报错、
// 一段泄露数据、一个反射串）。此前 TrafficRef 只记录「关联到哪条流量」，
// 于是出现两类问题：
//  1. 复核者必须重新通读全文才能判断证据是否成立；
//  2. 「模型声称的那句话是否真的在响应里」无法机械校验——模型可以凭印象
//     复述一段并不存在的输出，而 finding 照样成立。
//
// 解法（借鉴 PentestGPT 的「回执锚定」）：要求证据引用带上它在响应体里的
// 精确字节区间 [start,end)，并对该区间做 hash。校验时重新取该区间、比对 hash，
// 就能把「模型复述」和「真实字节」区分开。
//
// 向后兼容：SliceStart<0 表示未锚定，既有 finding 不受影响；锚定是渐进增强，
// 不是新的必填门槛（否则会一次性让所有历史证据失效）。
package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// MaxEvidenceSliceBytes 是单个证据切片的长度上限。
// 超过这个长度的「切片」已经不是证据而是全文引用，应改引整条流量。
const MaxEvidenceSliceBytes = 8 << 10 // 8 KiB

// SliceDigest 计算切片内容的稳定摘要（sha256 前 8 字节 hex，16 字符）。
// 用短摘要而不是全长：它只需检测「内容是否变了」，不是密码学承诺。
func SliceDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// AnchorSlice 在响应体 body 中定位 snippet 并返回锚定后的引用。
// 找不到 snippet 时返回错误——这是关键：调用方必须知道「我声称的证据
// 并不在响应里」，而不是静默存一条没有锚点的引用。
//
// 多次出现时取第一次出现的位置（确定性）。调用方若需要指定某次出现，
// 可用 AnchorSliceAt。
func AnchorSlice(ref TrafficRef, body []byte, snippet string) (TrafficRef, error) {
	snippet = strings.TrimSpace(snippet)
	if snippet == "" {
		return ref, fmt.Errorf("证据片段为空：无法锚定")
	}
	idx := strings.Index(string(body), snippet)
	if idx < 0 {
		return ref, fmt.Errorf("证据片段未在响应体中找到（片段前 80 字符：%q）——"+
			"请确认你引用的是实际收到的字节，而不是对响应的复述", firstRunes(snippet, 80))
	}
	return anchorAt(ref, body, idx, len(snippet))
}

// AnchorSliceAt 在指定偏移处锚定（调用方已知位置时用）。
func AnchorSliceAt(ref TrafficRef, body []byte, start, length int) (TrafficRef, error) {
	if start < 0 || length <= 0 || start+length > len(body) {
		return ref, fmt.Errorf("切片区间非法：[%d,%d) 超出响应体长度 %d", start, start+length, len(body))
	}
	return anchorAt(ref, body, start, length)
}

func anchorAt(ref TrafficRef, body []byte, start, length int) (TrafficRef, error) {
	if length > MaxEvidenceSliceBytes {
		return ref, fmt.Errorf("证据切片 %d 字节超过上限 %d：这已不是「证据片段」而是全文，"+
			"请只锚定支撑结论的那一小段（如报错行、泄露值、反射串）", length, MaxEvidenceSliceBytes)
	}
	slice := body[start : start+length]
	ref.SliceStart = start
	ref.SliceEnd = start + length
	ref.SliceHash = SliceDigest(slice)
	return ref, nil
}

// VerifySlice 校验一个已锚定的引用：重新取切片并比对 hash。
// 未锚定的引用返回 (false, nil)——「无法验证」不等于「验证失败」。
//
// 返回 (true, nil) 表示切片存在且内容未变；(false, err) 表示不一致。
func VerifySlice(ref TrafficRef, body []byte) (bool, error) {
	if !ref.Anchored() {
		return false, nil
	}
	if ref.SliceEnd > len(body) {
		return false, fmt.Errorf("切片 [%d,%d) 超出当前响应体长度 %d：流量内容已变（被截断或替换）",
			ref.SliceStart, ref.SliceEnd, len(body))
	}
	got := SliceDigest(body[ref.SliceStart:ref.SliceEnd])
	if got != ref.SliceHash {
		return false, fmt.Errorf("切片 hash 不匹配（记录 %s，实际 %s）：响应内容已变，"+
			"原证据不再成立，需要重新验证", ref.SliceHash, got)
	}
	return true, nil
}

// SliceText 取出锚定的切片文本（未锚定返回空）。
func SliceText(ref TrafficRef, body []byte) string {
	if !ref.Anchored() || ref.SliceEnd > len(body) {
		return ""
	}
	return string(body[ref.SliceStart:ref.SliceEnd])
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
