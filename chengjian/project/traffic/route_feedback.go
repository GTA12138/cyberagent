package traffic

import (
	"context"
	"net/http"
	"net/url"
	"sync"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

type routeChoiceKey struct{}

// A CONNECT owns one upstream connection; all exchanges on it share its choice.
// Plain HTTP requests each own a choice. Selection happens only when dialing,
// never from the response hook or a shared per-host lookup.
type routeChoice struct {
	mu       sync.Mutex
	selected bool
	upstream *url.URL
	err      error
	feedback func(error)
}

func (t *Traffic) SetRouteSelector(selectFeedback func(string) (string, func(error), error)) {
	if selectFeedback == nil {
		t.routes.Store(nil)
		return
	}
	t.routes.Store(&routeHooks{selectFeedback: selectFeedback})
}

func (s *sink) Requestheaders(f *mproxy.Flow) {
	if hooks := s.t.routes.Load(); hooks == nil || hooks.selectFeedback == nil {
		return
	}
	if f.Request == nil || f.Request.Raw() == nil {
		return
	}
	raw := f.Request.Raw()
	choice := &routeChoice{}
	if f.ConnContext != nil && f.ConnContext.ClientConn != nil {
		conn := f.ConnContext.ClientConn
		if raw.Method == http.MethodConnect {
			s.t.connectionRoutes.Store(conn, choice)
		} else if raw.URL != nil && raw.URL.Scheme == "https" {
			if stored, ok := s.t.connectionRoutes.Load(conn); ok {
				choice = stored.(*routeChoice)
			}
		}
	}
	*raw = *raw.WithContext(context.WithValue(raw.Context(), routeChoiceKey{}, choice))
}

func (s *sink) ClientDisconnected(conn *mproxy.ClientConn) {
	s.t.connectionRoutes.Delete(conn)
}

func (t *Traffic) resolveRequestUpstream(req *http.Request) (*url.URL, error) {
	h := t.routes.Load()
	if h == nil || h.selectFeedback == nil {
		return t.resolveUpstreamFor(hostOnly(req.Host))
	}
	choice, ok := req.Context().Value(routeChoiceKey{}).(*routeChoice)
	if !ok {
		choice = &routeChoice{}
		*req = *req.WithContext(context.WithValue(req.Context(), routeChoiceKey{}, choice))
	}
	choice.mu.Lock()
	defer choice.mu.Unlock()
	if !choice.selected {
		choice.selected = true
		raw, feedback, err := h.selectFeedback(hostOnly(req.Host))
		choice.err = err
		if err == nil {
			if raw == "" {
				choice.upstream = t.upstream.Load()
			} else {
				choice.upstream, choice.err = ValidateProxyURL(raw)
				if choice.err == nil {
					choice.feedback = feedback
				}
			}
		}
	}
	return choice.upstream, choice.err
}

func (choice *routeChoice) report(err error) {
	choice.mu.Lock()
	feedback := choice.feedback
	choice.mu.Unlock()
	if feedback != nil {
		feedback(err)
	}
}
