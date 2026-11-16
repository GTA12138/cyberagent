package db

import (
	"fmt"
	"testing"
)

func TestTraceCursorAndFinalOutputBeyondFirstThousand(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("information-recovery", "offline regression")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	intent, err := s.AddIntent(map[string]any{"summary": "trace paging"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	// One SQL fixture inserts enough rows to exercise the old fixed-prefix bug.
	_, err = d.Exec(`INSERT INTO activity(exploration_id,node_id,worker,kind,summary,detail)
SELECT $1,$2,'worker','text','step-'||g,'detail-'||g FROM generate_series(1,1005) g`, exp, intent)
	if err != nil {
		t.Fatal(err)
	}
	resultID, err := s.AppendActivity(Activity{NodeID: &intent, Worker: "worker", Kind: "result", Summary: "FINAL_AFTER_1000", Detail: "complete final conclusion"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.LatestWorkerOutputWithSources(intent)
	if err != nil || final == nil || final.ID != resultID || final.Detail != "complete final conclusion" {
		t.Fatalf("latest conclusion missing: %+v %v", final, err)
	}
	seen := 0
	var cursor int64
	for {
		page, more, err := s.ActivityTracePageWithSources(intent, "", cursor, 97)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page {
			if row.ID <= cursor {
				t.Fatal("cursor repeated a row")
			}
			cursor = row.ID
			seen++
		}
		if !more {
			break
		}
		if len(page) == 0 {
			t.Fatal("empty advancing page")
		}
	}
	if seen != 1006 || cursor != resultID {
		t.Fatalf("trace omitted rows: %d/%d", seen, cursor)
	}
	hits, _, err := s.ActivityTracePageWithSources(intent, "FINAL_AFTER_1000", 0, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("late keyword missing: %s", fmt.Sprint(err))
	}
}
