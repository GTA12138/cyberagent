// encode.go 变异值的编码管线（W1 二期：编码预处理）。
//
// 为什么在变异之后、发送之前做编码：手写绕过时最常见的错误不是 payload 错，
// 而是「编码错了一层」——该双重 URL 编码的只编了一层、base64 里带了换行、
// %u 编码写成了逐字符拼接。把它收进纯函数，模型只声明「用什么编码」，
// 字节由代码保证。
//
// 纯函数、无 IO：与 Replay/Compare 同一纪律，可单测锁语义。
package webtest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// EncodeKind 是支持的编码类别。
type EncodeKind string

const (
	EncNone    EncodeKind = ""         // 不编码（默认）
	EncURL     EncodeKind = "url"      // 百分号编码一次（QueryEscape 语义）
	EncURLTwice EncodeKind = "url2"    // 百分号编码两次（双层解码的目标）
	EncBase64  EncodeKind = "base64"   // 标准 base64（无换行）
	EncHex     EncodeKind = "hex"      // 十六进制全文
	EncUnicode EncodeKind = "unicode"  // %uXXXX（IIS/ASP 类解析器的宽字符形态）
)

// Encode 对变异值做一次编码。未知类别返回错误而不是静默原文——「以为编了
// 码其实没编」会让差分结论归因到错误的层。
func Encode(kind, value string) (string, error) {
	switch EncodeKind(strings.ToLower(strings.TrimSpace(kind))) {
	case EncNone:
		return value, nil
	case EncURL:
		return url.QueryEscape(value), nil
	case EncURLTwice:
		return url.QueryEscape(url.QueryEscape(value)), nil
	case EncBase64:
		return base64.StdEncoding.EncodeToString([]byte(value)), nil
	case EncHex:
		return hex.EncodeToString([]byte(value)), nil
	case EncUnicode:
		return unicodeEscape(value), nil
	default:
		return "", fmt.Errorf("未知编码类别 %q（可用：url / url2 / base64 / hex / unicode，留空不编码）", kind)
	}
}

// unicodeEscape 把每个字节编成 %u00XX（大写十六进制），与经典宽字符绕过形态
// 一致。按字节而不是 rune 编：payload 的多字节 UTF-8 在 %u 形态下本就按字节落。
func unicodeEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%u00%02X", s[i])
	}
	return b.String()
}
