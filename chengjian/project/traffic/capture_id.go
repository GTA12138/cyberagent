package traffic

import (
	"crypto/rand"
	"fmt"
	"time"
)

// A recorder can restart or exceed 10,000 captures in one second. IDs must not
// wrap a sequence or replace an older exchange and silently change its evidence.
// The timestamp preserves chronological ordering; the 128-bit nonce separates
// concurrent recorders and survives clock rollback and process restart.
func captureID(now time.Time) string {
	return fmt.Sprintf("%d-%s", now.UnixNano(), rand.Text())
}
