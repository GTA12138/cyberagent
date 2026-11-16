package server

// Regression tests for the confirmed runtime review defects. All credentials are synthetic.
import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/tunnel"
	"chengjian.local/chengjian/webtest"
)

func TestAuditReplayRejectsWrongBodyBaseline(t *testing.T) {
	base := webtest.Request{Method: "POST", URL: "http://example.invalid/form", Body: []byte("name=alice")}
	got, err := webtest.Replay(base, webtest.Mutation{Kind: "body", Name: "role", From: "user", To: "admin"})
	if err == nil {
		t.Errorf("wrong baseline accepted; unchanged body=%q", got.Body)
	}
}

func TestAuditDifferenceDoesNotProveVulnerability(t *testing.T) {
	cmp := webtest.Compare(webtest.Response{Status: 200, Body: []byte("normal")}, webtest.Response{Status: 403, Body: []byte("request rejected")})
	if cmp.FindingState() == "verified" {
		t.Errorf("ordinary request rejection becomes finding_state=%q, verdict=%q", cmp.FindingState(), cmp.Verdict)
	}
}

func TestAuditHeadersRemainPartOfExperimentalEvidence(t *testing.T) {
	a := webtest.Response{Status: 302, Headers: map[string][]string{"Location": {"/home"}}}
	b := webtest.Response{Status: 302, Headers: map[string][]string{"Location": {"https://example.invalid/elsewhere"}}}
	cmp := webtest.Compare(a, b)
	if cmp.FindingState() == "false_positive" {
		t.Errorf("different Location headers discarded; verdict=%q finding_state=%q", cmp.Verdict, cmp.FindingState())
	}
}

func auditRoutes() *tunnel.RouteTable {
	rt := tunnel.NewRouteTable()
	rt.Sync([]*db.TunnelRecord{
		{ID: 1, State: db.TunnelAlive, Kind: tunnel.KindSocks, Adapter: "chisel", DeployParams: []byte(`{"socks_port":19081}`)},
		{ID: 2, State: db.TunnelAlive, Kind: tunnel.KindSocks, Adapter: "chisel", DeployParams: []byte(`{"socks_port":19082}`)},
	})
	return rt
}

func TestAuditRouteReconsidersStalePositive(t *testing.T) {
	rt := auditRoutes()
	_, first, ok := rt.Select("192.0.2.10")
	if !ok {
		t.Fatal("fixture has no routes")
	}
	rt.ObserveRoute("192.0.2.10", first, nil)
	for i := 0; i < 1; i++ {
		_, id, _ := rt.Select("192.0.2.10")
		rt.ObserveRoute("192.0.2.10", id, errors.New("network is unreachable"))
	}
	_, next, _ := rt.Select("192.0.2.10")
	if next == first {
		t.Errorf("still pinned to tunnel %d after a failure; alternate route never tried", first)
	}
}

func TestAuditRouteAttributesOverlappingRequests(t *testing.T) {
	rt := auditRoutes()
	_, first, _ := rt.Select("192.0.2.10")
	_, second, _ := rt.Select("192.0.2.10")
	if first == second {
		t.Fatal("fixture did not select two different routes")
	}
	// The first request completes while the second is still in flight.
	rt.ObserveRoute("192.0.2.10", first, nil)
	for _, v := range rt.Snapshot() {
		if v.TunnelID == second && len(v.ProvenHosts) > 0 {
			t.Errorf("success from request on route %d credited to route %d", first, second)
		}
	}
}

func TestAuditProxychainsUsesSeparateHostAndPort(t *testing.T) {
	path, cleanup, err := writeProxychainsConf("socks5://127.0.0.1:19081")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "socks5 ") {
			if len(strings.Fields(line)) != 3 {
				t.Errorf("invalid ProxyList record %q; expected TYPE HOST PORT", line)
			}
		}
	}
}

func TestAuditLateralTargetIsSingleCombinedArgument(t *testing.T) {
	c := &db.CredentialRecord{CredType: db.CredPassword, Domain: "LAB", Username: "audit-user", Secret: "synthetic-secret"}
	args, err := lateralAuthArgs(c, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	want := "LAB/audit-user:synthetic-secret@192.0.2.10"
	if args[0] != want {
		t.Errorf("target operand is split from authentication: got synthetic argv=%q", args)
	}
}

func TestAuditSSHHandshakeHonorsOperationTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := ln.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, e := sshRun(context.Background(), &db.CredentialRecord{CredType: db.CredPassword, Username: "audit-user", Secret: "synthetic-secret"}, ln.Addr().String(), "echo synthetic-check", 100*time.Millisecond, "")
		done <- e
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("local fixture not connected")
	}
	defer conn.Close()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("SSH handshake remains blocked 500ms after starting with a 100ms operation deadline")
		conn.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("SSH did not stop after fixture connection closed")
		}
	}
}

func TestAuditRecordedBodyPreservesCRLF(t *testing.T) {
	raw := "POST /upload HTTP/1.1\r\nHost: example.invalid\r\nContent-Type: text/plain\r\n\r\nalpha\r\nbeta"
	got, err := parseRecordedRequest(raw, "http")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != "alpha\r\nbeta" {
		t.Errorf("request body was rewritten: %q", got.Body)
	}
}

func TestAuditTruncatedResponsesRemainInconclusive(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 2<<20) + "important tail"))
	}))
	defer fixture.Close()
	result, err := sendReplayRequest(context.Background(), webtest.Request{Method: "GET", URL: fixture.URL}, "", time.Second)
	if err != nil {
		return
	} // Rejecting an incomplete response is also acceptable.
	cmp := webtest.Compare(result, result)
	if cmp.Verdict != webtest.VerdictInconclusive {
		t.Errorf("truncated evidence accepted as complete: length=%d verdict=%s finding=%s", len(result.Body), cmp.Verdict, cmp.FindingState())
	}
}
