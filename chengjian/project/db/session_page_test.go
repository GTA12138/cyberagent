package db

import (
	"context"
	"strings"
	"testing"
)

func TestSessionPageRejectsInvalidBoundsWithoutDatabase(t *testing.T) {
	var store *SessionStore
	for _, tc := range []struct {
		taskID        int64
		status        string
		limit, offset int
	}{
		{0, "", 25, 0}, {1, "", 0, 0}, {1, "", 51, 0}, {1, "", 25, -1}, {1, "other", 25, 0},
	} {
		if _, _, err := store.ListAccessiblePage(context.Background(), tc.taskID, tc.status, tc.limit, tc.offset); err == nil {
			t.Fatalf("accepted invalid page: %+v", tc)
		}
	}
}

func TestSessionPageFiltersBeforePagingWithoutSecrets(t *testing.T) {
	d, store, owner, ownedID := sessionAccessFixture(t)
	ctx := context.Background()
	reader, err := d.CreateTask("session page reader", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(reader.ID) })
	if err := store.GrantAccess(ctx, ownedID, reader.ID, []string{SessionActionRead}, nil, "page fixture"); err != nil {
		t.Fatal(err)
	}
	ids := []int64{ownedID}
	for i := 0; i < 5; i++ {
		id, err := store.Create(ctx, &SessionRecord{Kind: "fixture", CreatedByTask: owner.ID})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, id) })
		if i%2 == 0 {
			if err := store.GrantAccess(ctx, id, reader.ID, []string{SessionActionRead}, nil, "page fixture"); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
	}
	// A readable row with a corrupt secret must still be listable with no key.
	if _, err := d.Exec(`UPDATE sessions SET secret='invalid encrypted value' WHERE id=$1`, ownedID); err != nil {
		t.Fatal(err)
	}
	metadataOnly := NewSessionStore(d, nil)
	for offset := 0; offset < len(ids); offset += 2 {
		rows, more, err := metadataOnly.ListAccessiblePage(ctx, reader.ID, "", 2, offset)
		if err != nil {
			t.Fatal(err)
		}
		end := offset + 2
		if end > len(ids) {
			end = len(ids)
		}
		if len(rows) != end-offset || more != (end < len(ids)) {
			t.Fatalf("page offset=%d rows=%d more=%v", offset, len(rows), more)
		}
		for i, row := range rows {
			if row.ID != ids[offset+i] {
				t.Fatalf("page offset=%d row=%d want=%d", offset, row.ID, ids[offset+i])
			}
		}
	}
	if err := store.RevokeAccess(ctx, ownedID, reader.ID); err != nil {
		t.Fatal(err)
	}
	rows, _, err := metadataOnly.ListAccessiblePage(ctx, reader.ID, "", 50, 0)
	if err != nil || len(rows) != len(ids)-1 {
		t.Fatalf("revocation not reflected: rows=%d err=%v", len(rows), err)
	}
}

func TestSessionPageOmitsOversizedAtomicMetadata(t *testing.T) {
	d, store, owner, id := sessionAccessFixture(t)
	ctx := context.Background()
	if _, err := d.Exec(`UPDATE sessions SET kind=$2,lang=$3,url=$4 WHERE id=$1`, id,
		strings.Repeat("k", 10000), strings.Repeat("l", 10000), "https://"+strings.Repeat("h", 10000)); err != nil {
		t.Fatal(err)
	}
	rows, more, err := NewSessionStore(d, nil).ListAccessiblePage(ctx, owner.ID, "", 1, 0)
	if err != nil || len(rows) != 1 || more {
		t.Fatalf("page rows=%d more=%v err=%v", len(rows), more, err)
	}
	row := rows[0]
	if row.ID != id || row.Kind != "" || row.URL != "" || row.Lang != "" || len(row.MetadataOmitted) != 3 {
		t.Fatalf("oversized metadata was not wholly omitted: %+v", row)
	}
	if err := store.UpdateStatus(ctx, id, SessionDead); err != nil {
		t.Fatal(err)
	}
	rows, more, err = store.ListAccessiblePage(ctx, owner.ID, SessionAlive, 1, 0)
	if err != nil || len(rows) != 0 || more {
		t.Fatalf("status filter applied after paging: rows=%d more=%v err=%v", len(rows), more, err)
	}
}
