package traffic

import (
	"strings"
	"testing"
)

func TestProxyValidationErrorsDoNotEchoSecrets(t *testing.T) {
	for _, raw := range []string{
		"http://user:private-password@127.0.0.1:bad%port",
		"http:user:private-password@",
	} {
		if _, err := ValidateProxyURL(raw); err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatal("invalid proxy must fail without echoing credentials")
		}
	}
}
