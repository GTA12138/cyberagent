package traffic

import (
	"strings"
	"testing"
	"time"
)

func TestCaptureIDDoesNotWrapWithinFrozenClock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	seen := map[string]bool{}
	for range 20_001 {
		id := captureID(now)
		if seen[id] || strings.ContainsAny(id, "/\\") {
			t.Fatalf("invalid or colliding ID: %s", id)
		}
		seen[id] = true
	}
}

func TestCaptureIDPreservesNanosecondOrdering(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if captureID(now) >= captureID(now.Add(time.Nanosecond)) {
		t.Fatal("capture IDs lost timestamp ordering")
	}
}
