package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/traffic"
)

func TestPrepareCanonicalizesReferencesWithoutLosingBindingMetadata(t *testing.T) {
	s := New(nil, nil, t.TempDir())
	s.ReadTrafficEvidence = func(ctx context.Context, taskID int64, id string, consume func(traffic.EvidenceExchange) error) error {
		if taskID != 7 {
			t.Fatalf("lost authoritative task: %d", taskID)
		}
		source, raw, err := traffic.ParseReference(id, "global")
		if err != nil {
			return err
		}
		return consume(traffic.EvidenceExchange{ID: source + "/" + raw, Request: strings.NewReader(""), Response: strings.NewReader("body"), RespLen: 4})
	}
	prepared, err := s.prepare(context.Background(), 7, []db.TrafficRef{
		{TrafficID: "42", Role: "proof", Note: "keep first"},
		{TrafficID: "global/42", Role: "baseline", Note: "duplicate alias"},
		{TrafficID: "task/7/42", Role: "verification", Note: "separate task"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 || prepared[0].Ref.TrafficID != "global/42" || prepared[0].Ref.Role != "proof" || prepared[0].Ref.Note != "keep first" || prepared[1].Ref.TrafficID != "task/7/42" {
		t.Fatalf("wrong prepared refs: %+v", prepared)
	}
	if prepared[0].Snapshot.ID == prepared[1].Snapshot.ID {
		t.Fatal("source namespaces collapsed into the same snapshot")
	}
}

func TestPrepareDoesNotReturnPartialEvidenceAfterReaderFailure(t *testing.T) {
	s := New(nil, nil, t.TempDir())
	readErr := errors.New("capture unavailable")
	s.ReadTrafficEvidence = func(_ context.Context, _ int64, id string, consume func(traffic.EvidenceExchange) error) error {
		if id == "bad" {
			return readErr
		}
		return consume(traffic.EvidenceExchange{ID: "global/" + id, Request: strings.NewReader(""), Response: strings.NewReader("")})
	}
	if prepared, err := s.prepare(context.Background(), 7, []db.TrafficRef{{TrafficID: "42"}, {TrafficID: "bad"}}); !errors.Is(err, readErr) || prepared != nil {
		t.Fatalf("partial evidence: %+v, %v", prepared, err)
	}
}
