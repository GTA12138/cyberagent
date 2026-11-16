package tunnel

import (
	"errors"
	"sync"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
)

func TestRouteFeedbackKeepsRequestAttribution(t *testing.T) {
	rt := NewRouteTable()
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1), routeRec(2, 20802, 1)})
	up1, done1, _ := rt.SelectFeedback("192.0.2.10")
	up2, done2, _ := rt.SelectFeedback("192.0.2.10")
	if up1 == up2 {
		t.Fatal("fixture did not select distinct routes")
	}
	done2(errors.New("network is unreachable"))
	done1(nil)
	views := rt.Snapshot()
	if len(views[0].ProvenHosts) != 1 || len(views[1].ProvenHosts) != 0 {
		t.Fatalf("cross-request attribution: %+v", views)
	}
	rt.Sync(nil)
	rt.Sync([]*db.TunnelRecord{routeRec(1, 20801, 1)})
	done1(nil)
	if len(rt.Snapshot()[0].ProvenHosts) != 0 {
		t.Fatal("old callback credited a recreated route")
	}
}

func TestRouteEvidenceExpiresAndFailureOverridesVerifiedRange(t *testing.T) {
	rt := NewRouteTable()
	r1 := verifiedRec(1, 20801, "192.0.2.0/24")
	rt.Sync([]*db.TunnelRecord{r1, routeRec(2, 20802, 1)})
	_, done, _ := rt.SelectFeedback("192.0.2.10")
	done(nil)
	done(errors.New("network is unreachable"))
	if _, id, _ := rt.Select("192.0.2.10"); id != 2 {
		t.Fatal("negative feedback did not override verified range")
	}
	old := time.Now().Add(-2 * routeEvidenceTTL)
	r1.ReachVerifiedAt = &old
	rt.Sync([]*db.TunnelRecord{r1, routeRec(2, 20802, 1)})
	rt.routes[1].ev["192.0.2.20"] = &hostEvidence{tries: 4, ok: 1, lastOK: old}
	if _, id, _ := rt.Select("192.0.2.20"); id != 2 {
		t.Fatal("expired host/range evidence still pins route")
	}
}

func TestRouteConcurrentSyncAndFeedback(t *testing.T) {
	rt := NewRouteTable()
	recs := []*db.TunnelRecord{routeRec(1, 20801, 1), routeRec(2, 20802, 1)}
	rt.Sync(recs)
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				rt.Sync(recs)
				_, done, _ := rt.SelectFeedback("192.0.2.10")
				done(nil)
				rt.Snapshot()
			}
		}()
	}
	wg.Wait()
}
