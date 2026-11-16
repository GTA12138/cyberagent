package apicontract

import (
	"os"
	"testing"
)

func TestBrowserContractMatchesGo(t *testing.T) {
	for path, want := range map[string]string{"../../web/src/lib/api-contracts.generated.ts": Types(), "../../web/src/lib/api-contracts.fixture.json": string(Fixture())} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s is stale; run go run ./cmd/api-types", path)
		}
	}
}
