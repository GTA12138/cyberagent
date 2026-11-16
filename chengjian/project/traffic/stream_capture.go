package traffic

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// Streaming preserves a bounded prefix on disk without buffering the wire body
// in memory or changing bytes delivered to the client. Overflow is evidence
// metadata, never a synthetic HTTP header/body attributed to the target.
const maxStreamCapture = 16 << 20

var streamFiles = make(chan struct{}, 16)
var streamFinalizers = make(chan struct{}, 1)

type bodyCaptureState struct {
	Received int64  `json:"received_bytes"`
	Retained int64  `json:"retained_bytes"`
	Complete bool   `json:"complete"`
	Error    string `json:"error,omitempty"`
}
type captureState struct {
	Request  bodyCaptureState `json:"request"`
	Response bodyCaptureState `json:"response"`
}
type streamBody struct {
	mu    sync.Mutex
	file  *os.File
	state bodyCaptureState
}

func newStreamBody() *streamBody {
	b := &streamBody{}
	select {
	case streamFiles <- struct{}{}:
		f, err := os.CreateTemp("", "chengjian-capture-*.bin")
		if err != nil {
			<-streamFiles
			b.state.Error = "capture_storage_unavailable"
		} else {
			b.file = f
		}
	default:
		b.state.Error = "capture_capacity_exceeded"
	}
	return b
}

func (b *streamBody) capture(p []byte, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.Received += int64(len(p))
	if b.file != nil {
		n := int(min(int64(len(p)), max(0, int64(maxStreamCapture)-b.state.Retained)))
		if n > 0 {
			written, e := b.file.Write(p[:n])
			b.state.Retained += int64(written)
			if e != nil {
				b.state.Error = "capture_storage_write_failed"
			}
		}
		if b.state.Received > maxStreamCapture {
			b.state.Error = "capture_limit_exceeded"
		}
	}
	if err == io.EOF && b.state.Error == "" {
		b.state.Complete = true
	}
	if err != nil && err != io.EOF {
		b.state.Error = "stream_read_failed"
	}
}

func (b *streamBody) finish() ([]byte, bodyCaptureState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil {
		return nil, b.state
	}
	f := b.file
	b.file = nil
	defer func() { f.Close(); os.Remove(f.Name()); <-streamFiles }()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		b.state.Error, b.state.Complete = "capture_storage_read_failed", false
		return nil, b.state
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStreamCapture))
	if err != nil {
		b.state.Error, b.state.Complete = "capture_storage_read_failed", false
	}
	b.state.Retained = int64(len(data))
	return data, b.state
}

type streamExchange struct {
	mu                sync.Mutex
	once              sync.Once
	request, response *streamBody
}
type captureReader struct {
	io.Reader
	body *streamBody
	end  func()
}

func (r *captureReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.body.capture(p[:n], err)
	if err != nil && r.end != nil {
		r.end()
	}
	return n, err
}

func (s *sink) stream(f *mproxy.Flow) *streamExchange {
	value, loaded := s.streams.LoadOrStore(f, &streamExchange{})
	x := value.(*streamExchange)
	if !loaded && f.Done() != nil {
		go func() { <-f.Done(); s.finishStream(f, x) }()
	}
	return x
}
func (s *sink) StreamRequestModifier(f *mproxy.Flow, reader io.Reader) io.Reader {
	if !f.Stream {
		return reader
	}
	x := s.stream(f)
	x.mu.Lock()
	x.request = newStreamBody()
	b := x.request
	x.mu.Unlock()
	return &captureReader{Reader: reader, body: b}
}
func (s *sink) StreamResponseModifier(f *mproxy.Flow, reader io.Reader) io.Reader {
	if !f.Stream {
		return reader
	}
	x := s.stream(f)
	x.mu.Lock()
	x.response = newStreamBody()
	b := x.response
	x.mu.Unlock()
	return &captureReader{Reader: reader, body: b, end: func() { s.finishStream(f, x) }}
}
func (s *sink) finishStream(f *mproxy.Flow, x *streamExchange) {
	x.once.Do(func() {
		defer s.streams.Delete(f)
		streamFinalizers <- struct{}{}
		defer func() { <-streamFinalizers }()
		x.mu.Lock()
		defer x.mu.Unlock()
		var req, resp []byte
		var state captureState
		if f.Request != nil {
			req = f.Request.Body
			state.Request = completeBody(len(req))
		}
		if f.Response != nil {
			resp = f.Response.Body
			state.Response = completeBody(len(resp))
		}
		if x.request != nil {
			req, state.Request = x.request.finish()
		}
		if x.response != nil {
			resp, state.Response = x.response.finish()
		}
		if f.Request == nil || f.Response == nil {
			return
		}
		copyFlow := *f
		copyReq, copyResp := *f.Request, *f.Response
		copyReq.Body, copyResp.Body = req, resp
		copyFlow.Request, copyFlow.Response = &copyReq, &copyResp
		s.t.observeRoute(f, nil)
		s.t.recordWithState(&copyFlow, &state)
	})
}
func completeBody(size int) bodyCaptureState {
	return bodyCaptureState{Received: int64(size), Retained: int64(size), Complete: true}
}

func (t *Traffic) captureMetadata(id string) string {
	var raw string
	if err := t.db.QueryRow(`SELECT metadata FROM capture_state WHERE id=?`, id).Scan(&raw); err != nil {
		return "[capture_state=unknown/legacy; 历史记录没有采集完整性元数据，不能据此保证原始资源完整]\n"
	}
	var state captureState
	if json.Unmarshal([]byte(raw), &state) != nil {
		return "[capture_state=unreadable; 信息完整性未知]\n"
	}
	hint := ""
	if !state.Request.Complete || !state.Response.Complete {
		hint = "\n[采集不完整；本地续读只能恢复已保留部分。需要更多原始字节时须重新获取；大响应可用 Range 分段请求，验证 206/Content-Range 与 ETag，禁止把 200 当续页拼接。]"
	}
	return fmt.Sprintf("[capture_state=%s; text_index_body_limit=%d]%s\n", raw, maxIndexBody, hint)
}
