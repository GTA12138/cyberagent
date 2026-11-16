package main

import (
	"chengjian.local/chengjian/internal/apicontract"
	"log"
	"os"
)

func main() {
	for path, body := range map[string][]byte{"web/src/lib/api-contracts.generated.ts": []byte(apicontract.Types()), "web/src/lib/api-contracts.fixture.json": apicontract.Fixture()} {
		if err := os.WriteFile(path, body, 0644); err != nil {
			log.Fatal(err)
		}
	}
}
