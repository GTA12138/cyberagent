package server

import (
	"strings"
	"testing"
)

func TestProxyDiagnosticsHideMalformedCredentialsAndTokens(t *testing.T) {
	for _, raw := range []string{
		"http://user:private-password@127.0.0.1:8080?token=private-token#private-fragment",
		"http://user:private-password@127.0.0.1:bad%port",
	} {
		if got := redactURL(raw); strings.Contains(got, "private-") {
			t.Fatal("proxy diagnostic leaked a credential or token")
		}
	}
}
