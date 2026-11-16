package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigBOMAndInvalidJSONDiagnostics(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CHENGJIAN_CONFIG", p)
	t.Setenv("CHENGJIAN_PG_DSN", "")
	if err := os.WriteFile(p, append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"database":{"dbname":"sample"}}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PostgresDSN(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"database":{"password":"private-do-not-log"},bad}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := PostgresDSN()
	if err == nil || !strings.Contains(err.Error(), "JSON") || strings.Contains(err.Error(), "private-do-not-log") {
		t.Fatalf("unsafe/missing diagnostic: %v", err)
	}
}
