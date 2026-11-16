package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const defaultMaxOutput = 30000

func maxOut(tc *ToolContext) int {
	if tc != nil && tc.MaxOutputChars > 0 {
		return tc.MaxOutputChars
	}
	return defaultMaxOutput
}

var spillSeq atomic.Int64

// Capture bounds a tool's textual output for the model. When the output exceeds
// the limit (ToolContext.MaxOutputChars, default 30000), the FULL output is
// written to OutputDir, the working directory or a temporary fallback directory.
// A preview plus a file pointer lets the model read byte pages on demand.
// Storage failures explicitly identify incomplete output. Tools that produce
// potentially large output should return Capture(tc, output) rather than the raw
// string.
func Capture(tc *ToolContext, s string) string {
	max := maxOut(tc)
	if len(s) <= max {
		return s
	}
	dir := filepath.Join(os.TempDir(), "chengjian-tool-output")
	if tc != nil {
		if tc.OutputDir != "" {
			dir = tc.OutputDir
		} else if tc.WorkingDir != "" {
			dir = filepath.Join(tc.WorkingDir, "cmd-output")
		}
	}
	{
		if path, err := spillOutput(dir, s); err == nil {
			ref := path
			if tc != nil && tc.WorkingDir != "" {
				if rel, e := filepath.Rel(tc.WorkingDir, path); e == nil && !strings.HasPrefix(rel, "..") {
					ref = rel
				}
			}
			lines := strings.Count(s, "\n") + 1
			return s[:max] + fmt.Sprintf(
				"\n\n... <persisted-output>[Output too large: full %d bytes / %d lines.Full output saved to  %s </persisted-output>",
				len(s), lines, ref)
		} else {
			return truncate(s, max) + "\n[output_preservation_failed: " + err.Error() + "; incomplete=true; reduce page size and retry the source tool]"
		}
	}
}

// alreadyCaptured reports whether s already carries a marker Capture leaves
// behind — a spill pointer or a head+tail truncation notice. It is how a global
// post-tool net recognises output a tool already ran through Capture itself.
//
// A false negative (legitimate text that happens to contain a marker) only means
// the guard fails to re-cap already-large output; it never corrupts. The markers
// are distinctive enough that this is vanishingly rare.
func alreadyCaptured(s string) bool {
	return strings.Contains(s, "<persisted-output>") || strings.Contains(s, "characters truncated]")
}

// CaptureOnce is Capture with an idempotency guard: output a tool already ran
// through Capture (so it carries a spill pointer or truncation notice) is
// returned unchanged. This lets a single post-tool net cap every tool's output
// without re-truncating or re-spilling what built-in / MCP / custom tools
// already handled themselves. Output within the limit is a no-op either way,
// since Capture returns it unchanged.
func CaptureOnce(tc *ToolContext, s string) string {
	if len(s) <= maxOut(tc)+2048 && (alreadyCaptured(s) || strings.Contains(s, "[Read truncated to fit") || strings.Contains(s, "[more content: Read")) {
		return s
	}
	return Capture(tc, s)
}

func spillOutput(dir, s string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("output-%d-%d.txt", time.Now().Unix(), spillSeq.Add(1))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// resolvePath joins a relative path against the working directory.
func resolvePath(tc *ToolContext, p string) string {
	if filepath.IsAbs(p) || tc == nil || tc.WorkingDir == "" {
		return p
	}
	return filepath.Join(tc.WorkingDir, p)
}

// truncate trims s to max characters, keeping head and tail.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + fmt.Sprintf("\n\n... [%d characters truncated] ...\n\n", len(s)-max) + s[len(s)-half:]
}
