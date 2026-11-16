package tool

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

func readBoundedFile(ctx context.Context, path string, offset, limit int, byteOffset *int64, byteLimit int, encoding string, tc *ToolContext) (Result, error) {
	if offset < 0 || limit < 0 || byteLimit < 0 || byteOffset != nil && *byteOffset < 0 {
		return Errorf("offset/limit must be nonnegative"), nil
	}
	if encoding != "" && encoding != "text" && encoding != "base64" {
		return Errorf("encoding must be text/base64"), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Errorf(err.Error()), nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Errorf(err.Error()), nil
	}
	if err := ctx.Err(); err != nil {
		return Errorf(err.Error()), nil
	}
	if byteOffset != nil || byteLimit > 0 || encoding == "base64" {
		start := int64(0)
		if byteOffset != nil {
			start = min(*byteOffset, info.Size())
		}
		n := byteLimit
		if n <= 0 || n > 8192 {
			n = 8192
		}
		// Leave room for base64 expansion and metadata within the model budget.
		n = min(n, max(1, (maxOut(tc)-800)*3/4))
		data := make([]byte, min(int64(n), info.Size()-start))
		read, e := f.ReadAt(data, start)
		if e != nil && e != io.EOF {
			return Errorf(e.Error()), nil
		}
		data = data[:read]
		if encoding == "base64" || isBinary(data) || !utf8.Valid(data) {
			encoding = "base64"
		} else {
			encoding = "text"
		}
		body := string(data)
		if encoding == "base64" {
			body = base64.StdEncoding.EncodeToString(data)
		}
		return Text(fmt.Sprintf("[byte_offset=%d next_byte_offset=%d total_bytes=%d complete=%t encoding=%s]\n%s", start, start+int64(read), info.Size(), start+int64(read) >= info.Size(), encoding, body)), nil
	}
	reader := bufio.NewReaderSize(f, 8192)
	if preview, _ := reader.Peek(int(min(info.Size(), 8000))); isBinary(preview) {
		return Text(fmt.Sprintf("[binary file; total_bytes=%d. Read byte_offset=0, byte_limit=8192, encoding=base64 for lossless content]", info.Size())), nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	if offset <= 0 {
		offset = 1
	}
	var b strings.Builder
	var position int64
	lineNo := 1
	lastErr := error(nil)
	for (lineNo < offset || lineNo-offset < limit) && lastErr != io.EOF {
		if err := ctx.Err(); err != nil {
			return Errorf(err.Error()), nil
		}
		startByte := position
		var line []byte
		fullLen := 0
		for {
			fragment, e := reader.ReadSlice('\n')
			position += int64(len(fragment))
			fullLen += len(fragment)
			if lineNo >= offset && len(line) < maxLineLen {
				line = append(line, fragment[:min(len(fragment), maxLineLen-len(line))]...)
			}
			if e == bufio.ErrBufferFull {
				if err := ctx.Err(); err != nil {
					return Errorf(err.Error()), nil
				}
				continue
			}
			lastErr = e
			if e != nil && e != io.EOF {
				return Errorf(e.Error()), nil
			}
			break
		}
		if lineNo >= offset && fullLen > 0 {
			text := strings.TrimSuffix(string(line), "\n")
			if fullLen > maxLineLen {
				text = fmt.Sprintf("[line truncated; full line starts at byte_offset=%d; use byte paging] ", startByte) + text
			}
			fmt.Fprintf(&b, "%6d\t%s\n", lineNo, text)
		}
		lineNo++
		if b.Len() > maxOut(tc)+readOutputBonus {
			break
		}
	}
	if b.Len() == 0 {
		if info.Size() == 0 {
			return Text("(file is empty)"), nil
		}
		return Text("(no lines in the requested range)"), nil
	}
	out := readCapture(tc, b.String(), offset-1)
	if position < info.Size() && !strings.Contains(out, "Read truncated") {
		out += fmt.Sprintf("\n[more content: Read offset=%d or byte_offset=%d; total_bytes=%d]", lineNo, position, info.Size())
	}
	return Text(out), nil
}
