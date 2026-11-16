package llm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// sseScanner reads Server-Sent Events from a response body, yielding the
// (event, data) pair of each message. Anthropic uses named events; OpenAI uses
// bare data lines. Both are handled.
type sseScanner struct {
	r    *bufio.Reader
	read int64
}

const maxSSEEventBytes = 8 << 20
const maxLLMResponseBytes = 32 << 20
const maxSSEStreamBytes = 64 << 20

var ErrIncompleteStream = errors.New("llm stream ended before a terminal event")
var ErrResponseLimit = errors.New("llm response exceeds memory limit")

// readResponse bounds non-streaming replies, including malformed gateway bodies.
// Never return a prefix as a successful response.
func readResponse(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxLLMResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxLLMResponseBytes {
		return nil, ErrResponseLimit
	}
	return b, nil
}

func (s *sseScanner) line() (string, error) {
	var b bytes.Buffer
	for {
		part, err := s.r.ReadSlice('\n')
		s.read += int64(len(part))
		if b.Len()+len(part) > maxSSEEventBytes || s.read > maxSSEStreamBytes {
			return "", ErrResponseLimit
		}
		b.Write(part)
		if err == bufio.ErrBufferFull {
			continue
		}
		return b.String(), err
	}
}

func newSSEScanner(body io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReaderSize(body, 64*1024)}
}

// next returns the next event's (type, data). event is "" for bare-data streams.
// Returns io.EOF at end of stream.
func (s *sseScanner) next() (event, data string, err error) {
	var buf bytes.Buffer
	for {
		line, rerr := s.line()
		if rerr != nil && rerr != io.EOF {
			return "", "", rerr
		}
		if rerr == io.EOF && line == "" {
			if buf.Len() > 0 || event != "" {
				return event, strings.TrimRight(buf.String(), "\n"), nil
			}
			return "", "", io.EOF
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if buf.Len() > 0 || event != "" {
				return event, strings.TrimRight(buf.String(), "\n"), nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := line[len("data:"):]
			d = strings.TrimPrefix(d, " ")
			buf.WriteString(d)
			buf.WriteByte('\n')
		default:
			// comment / unknown field — ignore
		}
		if buf.Len()+len(event) > maxSSEEventBytes {
			return "", "", ErrResponseLimit
		}
		if rerr == io.EOF {
			if buf.Len() > 0 || event != "" {
				return event, strings.TrimRight(buf.String(), "\n"), nil
			}
			return "", "", io.EOF
		}
	}
}
