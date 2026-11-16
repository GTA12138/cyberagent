package traffic

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordingTogglePreservesRoutingAndSkipsBodies(t *testing.T) {
	tr, dir := openTraffic(t)
	if err := tr.SetUpstreamProxy("http://127.0.0.1:9000"); err != nil {
		t.Fatal(err)
	}
	tr.record(newFlow("record.test", "GET", "/before", nil, []byte("baseline")))
	tr.SetRecordEnabled(false)
	// A large body would create a blob if the recording barrier were too late.
	tr.record(newFlow("record.test", "POST", "/off", bytes.Repeat([]byte("s"), maxInlineBody+1), nil))
	var count int
	if err := tr.DB().QueryRow(`SELECT count(*) FROM exchanges`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("capture off wrote an exchange: count=%d err=%v", count, err)
	}
	if tr.UpstreamProxy() != "http://127.0.0.1:9000" || tr.CACertPath() == "" {
		t.Fatal("recording toggle changed routing or TLS trust")
	}
	if err := filepath.WalkDir(filepath.Join(dir, "_blobs"), func(_ string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err == nil && !entry.IsDir() {
			t.Error("capture off spilled a body to disk")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tr.SetRecordEnabled(true)
	tr.record(newFlow("record.test", "GET", "/after", nil, []byte("resumed")))
	if err := tr.DB().QueryRow(`SELECT count(*) FROM exchanges`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("recording did not resume: count=%d err=%v", count, err)
	}
}
