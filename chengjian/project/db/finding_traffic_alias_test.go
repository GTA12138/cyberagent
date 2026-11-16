package db

import "testing"

func TestGlobalSnapshotAliasRequiresIdenticalCaptureMetadata(t *testing.T) {
	legacy := TrafficEvidenceSnapshot{SourceTrafficID: "42", CapturedAt: 1, URL: "http://fixture.test/", Method: "GET", Status: 200, RespHash: "body-hash", RespLen: 4}
	legacy.ID = TrafficSnapshotID(legacy)
	qualified := legacy
	qualified.SourceTrafficID = "global/42"
	qualified.ID = TrafficSnapshotID(qualified)
	if globalSnapshotAlias(qualified) != legacy.ID || globalSnapshotAlias(legacy) != qualified.ID {
		t.Fatal("raw/global alias does not preserve complete capture identity")
	}
	changed := qualified
	changed.RespHash = "different-body"
	if globalSnapshotAlias(changed) == legacy.ID {
		t.Fatal("different capture body collapsed into old evidence")
	}
	changed = qualified
	changed.CapturedAt++
	if globalSnapshotAlias(changed) == legacy.ID {
		t.Fatal("reused raw ID at a different time collapsed into old evidence")
	}
	qualified.SourceTrafficID = "task/7/42"
	if globalSnapshotAlias(qualified) != "" {
		t.Fatal("task evidence treated as a global alias")
	}
}
