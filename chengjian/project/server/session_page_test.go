package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/session"
)

type pageMetadataSession struct {
	id     int64
	driver string
}

func (s *pageMetadataSession) ID() int64                { return s.id }
func (*pageMetadataSession) Kind() string               { panic("metadata list called Kind") }
func (*pageMetadataSession) Test(context.Context) error { panic("metadata list called Test") }
func (*pageMetadataSession) Exec(context.Context, string, time.Duration) (string, string, error) {
	panic("metadata list called Exec")
}
func (*pageMetadataSession) ReadFile(context.Context, string) ([]byte, error) {
	panic("metadata list called ReadFile")
}
func (*pageMetadataSession) WriteFile(context.Context, string, []byte) error {
	panic("metadata list called WriteFile")
}
func (*pageMetadataSession) Close(context.Context) error { return nil }
func (s *pageMetadataSession) Capabilities() session.Capabilities {
	c := session.UnknownCapabilities()
	c.Driver = s.driver
	return c
}

func TestSessionListRejectsInvalidPageWithoutDatabase(t *testing.T) {
	s := &Server{}
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: 1})
	for _, input := range []string{`null`, `[]`, `{`, `{"limit":0}`, `{"limit":51}`, `{"limit":1.5}`, `{"limit":null}`, `{"offset":-1}`, `{"offset":"0"}`, `{"offset":null}`, `{"offset":9223372036854775808}`} {
		result, err := s.sessionListTool().Call(ctx, json.RawMessage(input), nil)
		if err != nil || !result.IsError {
			t.Fatalf("accepted invalid page %q: result=%+v err=%v", input, result, err)
		}
	}
}

func sessionPageFixture(t *testing.T) (*Server, *db.SessionStore, int64) {
	t.Helper()
	pg := openIntranetTestDB(t)
	owner, err := pg.CreateTask("bounded session page", "local metadata fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(owner.ID) })
	store := db.NewSessionStore(pg, []byte("session-page-fixture-key"))
	return &Server{sessReg: session.NewRegistry(), sessStore: store}, store, owner.ID
}

func callSessionPage(t *testing.T, s *Server, taskID int64, input string) (map[string]any, string) {
	t.Helper()
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: taskID})
	result, err := s.sessionListTool().Call(ctx, json.RawMessage(input), nil)
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("page failed: result=%+v err=%v", result, err)
	}
	text := result.Content[0].Text
	if len(text) > 64*1024 {
		t.Fatalf("JSON output exceeded 64KiB: %d", len(text))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out, text
}

func TestSessionListDefaultsAndBudgetedContinuation(t *testing.T) {
	s, store, taskID := sessionPageFixture(t)
	ctx := context.Background()
	const total = 60
	ids := make([]int64, 0, total)
	for i := 0; i < total; i++ {
		id, err := store.Create(ctx, &db.SessionRecord{Kind: strings.Repeat("<", 64), Lang: strings.Repeat("<", 64), URL: "https://local.invalid/", CreatedByTask: taskID})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		t.Cleanup(func() { _ = store.Delete(ctx, id) })
	}
	first, _ := callSessionPage(t, s, taskID, `{}`)
	if first["count"] != float64(25) || first["alive"] != float64(25) || first["has_more"] != true || first["next_offset"] != float64(25) {
		t.Fatalf("default page changed: %+v", first)
	}
	// Encode-heavy provider metadata is valid and individually bounded, but a
	// full 50-row page would exceed the shared context budget.
	for _, id := range ids {
		s.sessReg.Add(&pageMetadataSession{id: id, driver: strings.Repeat("<", 200)})
	}
	seen := make(map[int64]bool)
	offset, budgetReduced := 0, false
	for page := 0; page < total; page++ {
		out, _ := callSessionPage(t, s, taskID, fmt.Sprintf(`{"limit":50,"offset":%d}`, offset))
		items := out["items"].([]any)
		if len(items) < 50 && out["has_more"] == true {
			budgetReduced = true
		}
		for _, raw := range items {
			id := int64(raw.(map[string]any)["id"].(float64))
			if seen[id] {
				t.Fatalf("duplicate session at offset %d: %d", offset, id)
			}
			seen[id] = true
		}
		next := int(out["next_offset"].(float64))
		if next != offset+len(items) {
			t.Fatalf("cursor skipped budget-trimmed rows: offset=%d count=%d next=%d", offset, len(items), next)
		}
		if out["has_more"] == false {
			break
		}
		if next <= offset {
			t.Fatal("pagination made no progress")
		}
		offset = next
	}
	if !budgetReduced || len(seen) != total {
		t.Fatalf("budget reduction=%v visited=%d want=%d", budgetReduced, len(seen), total)
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("session skipped: %d", id)
		}
	}
}

func TestSessionListOmitsAtomicMetadataAndHugeCapabilities(t *testing.T) {
	s, store, taskID := sessionPageFixture(t)
	ctx := context.Background()
	id, err := store.Create(ctx, &db.SessionRecord{Kind: strings.Repeat("k", 10000), Lang: strings.Repeat("l", 10000), URL: "https://" + strings.Repeat("h", 10000), CreatedByTask: taskID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, id) })
	s.sessReg.Add(&pageMetadataSession{id: id, driver: strings.Repeat("sensitive-long-provider-value", 10000)})
	longHostID, err := store.Create(ctx, &db.SessionRecord{Kind: "fixture", URL: "https://" + strings.Repeat("h", 300) + "/", CreatedByTask: taskID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, longHostID) })
	out, text := callSessionPage(t, s, taskID, `{}`)
	item := out["items"].([]any)[0].(map[string]any)
	if item["kind"] != nil || item["host"] != nil || item["lang"] != nil {
		t.Fatalf("atomic fields were partially emitted: %+v", item)
	}
	if len(item["metadata_omitted"].([]any)) != 4 || strings.Contains(text, "sensitive-long-provider-value") {
		t.Fatalf("missing omission flags or leaked large provider: %s", text)
	}
	if item["capabilities"].(map[string]any)["driver"] != "unknown" {
		t.Fatalf("oversized capability snapshot was not omitted: %s", text)
	}
	longHost := out["items"].([]any)[1].(map[string]any)
	if longHost["host"] != nil || len(longHost["metadata_omitted"].([]any)) != 1 || longHost["metadata_omitted"].([]any)[0] != "host" {
		t.Fatalf("oversized host was shortened or unmarked: %+v", longHost)
	}
}
