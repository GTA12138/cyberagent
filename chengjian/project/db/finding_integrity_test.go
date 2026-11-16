package db

import (
	"database/sql"
	"fmt"
	"testing"
)

func integrityFinding(t *testing.T) (*DB, int64) {
	t.Helper()
	d, fid := retestDB(t)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM conversations WHERE id IN (SELECT conversation_id FROM finding_checks WHERE finding_id=$1)`, fid)
	})
	return d, fid
}

func startIntegrityCheck(t *testing.T, d *DB, fid int64, verdict string) *FindingCheck {
	t.Helper()
	c, conv, created, err := d.CreateFindingCheck(t.Context(), fid, FindingCheckKindVerify)
	if err != nil || !created {
		t.Fatalf("create check: created=%v err=%v", created, err)
	}
	if started, err := d.StartFindingCheck(t.Context(), c.ID); err != nil || !started {
		t.Fatalf("start check: %v %v", started, err)
	}
	if verdict != "" {
		if err := d.RecordFindingCheckResult(t.Context(), conv.ID, verdict, "fixture result", "fixture evidence"); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func mergeIntegrityFinding(t *testing.T, d *DB, fid int64) {
	t.Helper()
	f, err := d.GetFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	// The standalone fixture has no graph node. Use a dedicated task/node so the
	// production merge can update both projections in the same transaction.
	tk, err := d.CreateTask("integrity fixture", "local data only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteFinding(fid); _ = d.DeleteTask(tk.ID) })
	node, err := d.Exploration(tk.ExplorationID).AddNode(KindFinding, map[string]any{"severity": f.Severity, "evidence": map[string]any{}}, 1, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE findings SET task_id=$2,node_id=$3 WHERE id=$1`, fid, tk.ID, node); err != nil {
		t.Fatal(err)
	}
	f.NodeID = &node
	err = d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(tx, fid, nil); err != nil {
			return err
		}
		_, err := mergeFindingTx(tx, f, RecordFindingInput{TaskID: tk.ID, ExplorationID: tk.ExplorationID,
			VulnClass: f.VulnClass, Severity: "critical", Summary: "new fixture material", Evidence: "new evidence"}, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertIntegrityStatus(t *testing.T, d *DB, fid int64, want string) {
	t.Helper()
	f, err := d.GetFinding(fid)
	if err != nil || f == nil || f.Status != want {
		t.Fatalf("finding status want %s: %+v %v", want, f, err)
	}
}

func TestFindingCheckEvidenceRevision(t *testing.T) {
	t.Run("old staged verdict cannot confirm merged evidence", func(t *testing.T) {
		d, fid := integrityFinding(t)
		c := startIntegrityCheck(t, d, fid, "verified")
		mergeIntegrityFinding(t, d, fid)
		if err := d.FinishFindingCheck(c.ID, "completed", ""); err != nil {
			t.Fatal(err)
		}
		assertIntegrityStatus(t, d, fid, FindingPending)
	})
	t.Run("confirmed merge is pending and new check can reject it", func(t *testing.T) {
		d, fid := integrityFinding(t)
		if _, err := d.SetFindingStatus(fid, FindingConfirmed); err != nil {
			t.Fatal(err)
		}
		mergeIntegrityFinding(t, d, fid)
		assertIntegrityStatus(t, d, fid, FindingPending)
		c := startIntegrityCheck(t, d, fid, "false_positive")
		if err := d.FinishFindingCheck(c.ID, "completed", ""); err != nil {
			t.Fatal(err)
		}
		assertIntegrityStatus(t, d, fid, FindingFalsePositive)
	})
	t.Run("new revision replaces active old check", func(t *testing.T) {
		d, fid := integrityFinding(t)
		old := startIntegrityCheck(t, d, fid, "verified")
		mergeIntegrityFinding(t, d, fid)
		fresh, _, created, err := d.CreateFindingCheck(t.Context(), fid, FindingCheckKindVerify)
		if err != nil || !created || fresh.ID == old.ID {
			t.Fatalf("replacement: %+v %v %v", fresh, created, err)
		}
		rows, err := d.ListFindingChecks(fid)
		if err != nil || len(rows) != 2 || rows[1].Status != "stopped" {
			t.Fatalf("old check not stopped: %+v %v", rows, err)
		}
		if err := d.FinishFindingCheck(old.ID, "completed", ""); err != nil {
			t.Fatal(err)
		}
		assertIntegrityStatus(t, d, fid, FindingPending)
		dup, _, created, err := d.CreateFindingCheck(t.Context(), fid, FindingCheckKindVerify)
		if err != nil || created || dup.ID != fresh.ID {
			t.Fatalf("same revision not deduplicated: %+v %v %v", dup, created, err)
		}
	})
	for _, status := range []string{FindingFixed, FindingFalsePositive, FindingConfirmed} {
		t.Run("manual "+status+" survives", func(t *testing.T) {
			d, fid := integrityFinding(t)
			verdict := "verified"
			if status == FindingConfirmed {
				verdict = "false_positive"
			}
			c := startIntegrityCheck(t, d, fid, verdict)
			if _, err := d.SetFindingStatus(fid, status); err != nil {
				t.Fatal(err)
			}
			if err := d.FinishFindingCheck(c.ID, "completed", ""); err != nil {
				t.Fatal(err)
			}
			assertIntegrityStatus(t, d, fid, status)
		})
	}
}

func TestFindingRetestCannotFixNewEvidenceRevision(t *testing.T) {
	d, fid := integrityFinding(t)
	r, conv, _, err := d.CreateFindingRetest(t.Context(), fid, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.StartFindingRetest(t.Context(), r.ID); err != nil || !ok {
		t.Fatal(err)
	}
	if err := d.RecordFindingRetestResult(t.Context(), conv.ID, "fixed", "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	mergeIntegrityFinding(t, d, fid)
	if err := d.FinishFindingRetest(r.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	assertIntegrityStatus(t, d, fid, FindingPending)
}

func TestFindingRetestPreservesConcurrentTriage(t *testing.T) {
	d, fid := integrityFinding(t)
	r, conv, _, err := d.CreateFindingRetest(t.Context(), fid, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.StartFindingRetest(t.Context(), r.ID); err != nil || !ok {
		t.Fatal(err)
	}
	if err := d.RecordFindingRetestResult(t.Context(), conv.ID, "fixed", "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetFindingStatus(fid, FindingFalsePositive); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(r.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	assertIntegrityStatus(t, d, fid, FindingFalsePositive)
}

func TestFindingMetadataRollbackOnProjectionFailure(t *testing.T) {
	d, fid := integrityFinding(t)
	mergeIntegrityFinding(t, d, fid)
	before, err := d.GetFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("fixture_metadata_%d", fid)
	_, err = d.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture projection failure'; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON exploration_nodes`)
		_, _ = d.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
	})
	_, err = d.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON exploration_nodes FOR EACH ROW WHEN (OLD.id=%d) EXECUTE FUNCTION %s()`, name, *before.NodeID, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetFindingSeverity(fid, "low"); err == nil {
		t.Fatal("projection failure was ignored")
	}
	after, err := d.GetFinding(fid)
	if err != nil || after.Severity != before.Severity || after.EvidenceVersion != before.EvidenceVersion {
		t.Fatalf("partial metadata update: %+v %v", after, err)
	}
}
