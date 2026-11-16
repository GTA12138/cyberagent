package agentcore

import (
	"context"
	"errors"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/transcript"
	"os"
	"path/filepath"
	"testing"
)

func TestUnwritableTranscriptStopsBeforeModel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file-not-directory")
	if err := os.WriteFile(root, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewSession(Options{Transcript: transcript.NewStore(root), DisableBackgroundTasks: true})
	defer s.Close()
	var term *harness.Terminal
	for ev, err := range s.Prompt(context.Background(), "fixture") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Terminal != nil {
			term = ev.Terminal
		}
	}
	var persistenceErr *harness.PersistenceError
	if term == nil || !errors.As(term.Err, &persistenceErr) {
		t.Fatalf("unwritable transcript did not stop execution: %+v", term)
	}
}
