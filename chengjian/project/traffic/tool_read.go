package traffic

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var rawBlobPointer = regexp.MustCompile(`\n…\[truncated\] @blob sha256:([a-f0-9]{64}) \(len=[0-9]+\)$`)

func trafficReadPages(req, resp, part string, offset, length int) string {
	if length <= 0 || length > 8192 {
		length = 8192
		if part == "" || part == "both" {
			length = 4096
		}
	}
	var out []string
	for _, p := range []struct{ name, text string }{{"request", req}, {"response", resp}} {
		if part != "" && part != "both" && part != p.name {
			continue
		}
		start := min(offset, len(p.text))
		for start < len(p.text) && !utf8.RuneStart(p.text[start]) {
			start++
		}
		end := start + min(length, len(p.text)-start)
		for end > start && end < len(p.text) && !utf8.RuneStart(p.text[end]) {
			end--
		}
		// A tiny requested page must still make progress through a UTF-8 rune.
		if end == start && start < len(p.text) {
			_, n := utf8.DecodeRuneInString(p.text[start:])
			end += n
		}
		head := fmt.Sprintf("=== %s ===\n[part=%s offset=%d next_offset=%d total=%d complete=%t]\n", strings.ToUpper(p.name), p.name, start, end, len(p.text), end == len(p.text))
		if m := rawBlobPointer.FindStringSubmatch(p.text); len(m) > 1 {
			head += "[body_blob_hash=" + m[1] + "; 完整正文用 traffic_blob(hash=该值, source=上方来源, offset=0) 读取]\n"
		}
		out = append(out, head+p.text[start:end])
	}
	return strings.Join(out, "\n\n")
}

func trafficBlobPage(data []byte, offset, total int64, encoding string) string {
	if encoding == "base64" || isBinaryBody("", data) || !utf8.Valid(data) {
		return fmt.Sprintf("[offset=%d next_offset=%d length=%d total=%d complete=%t encoding=base64]\n%s", offset, offset+int64(len(data)), len(data), total, offset+int64(len(data)) >= total, base64.StdEncoding.EncodeToString(data))
	}
	return fmt.Sprintf("[offset=%d next_offset=%d length=%d total=%d complete=%t encoding=text]\n%s", offset, offset+int64(len(data)), len(data), total, offset+int64(len(data)) >= total, string(data))
}
