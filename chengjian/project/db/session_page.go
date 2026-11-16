package db

import (
	"context"
	"fmt"
	"time"
)

// SessionMetadataRecord is the non-secret projection used by agent lists.
// Oversized atomic metadata is omitted before transfer from PostgreSQL rather
// than shortened into a different, potentially actionable value.
type SessionMetadataRecord struct {
	ID              int64
	Kind            string
	HostAssetID     int64
	URL             string
	Lang            string
	Status          string
	CreatedByTask   int64
	CreatedByIntent int64
	LastBeat        time.Time
	MetadataOmitted []string
}

// ListAccessiblePage authorizes rows before applying a stable ID order and
// LIMIT/OFFSET. It never reads or decrypts secret, including for owned rows.
// hasMore is based on one extra authorized row, not an unrelated global count.
// Offsets describe the current live ledger; concurrent grants/deletes can
// change page membership, so this is not an immutable multi-request snapshot.
func (s *SessionStore) ListAccessiblePage(ctx context.Context, taskID int64, status string, limit, offset int) ([]*SessionMetadataRecord, bool, error) {
	if taskID <= 0 || limit < 1 || limit > 50 || offset < 0 {
		return nil, false, fmt.Errorf("task context, limit in [1,50], and nonnegative offset are required")
	}
	if status != "" && status != SessionAlive && status != SessionDead {
		return nil, false, fmt.Errorf("invalid session status filter")
	}
	if s == nil || s.d == nil {
		return nil, false, fmt.Errorf("session authorization store unavailable")
	}
	rows, err := s.d.QueryContext(ctx, `SELECT id,
CASE WHEN octet_length(kind)<=64 THEN kind ELSE '' END,
COALESCE(host_asset_id,0),
CASE WHEN octet_length(url)<=4096 THEN url ELSE '' END,
CASE WHEN octet_length(lang)<=64 THEN lang ELSE '' END,
status,COALESCE(created_by_task,0),COALESCE(created_by_intent,0),last_beat,
octet_length(kind)>64,octet_length(url)>4096,octet_length(lang)>64
FROM sessions
WHERE EXISTS (SELECT 1 FROM tasks caller WHERE caller.id=$1 AND caller.deleted_at IS NULL AND caller.archived_at IS NULL)
AND ($2='' OR status=$2)
AND (created_by_task=$1 OR EXISTS (SELECT 1 FROM session_access_grants g
    WHERE g.session_id=sessions.id AND g.task_id=$1 AND g.revoked_at IS NULL
    AND (g.expires_at IS NULL OR g.expires_at>now())))
ORDER BY id LIMIT $3 OFFSET $4`, taskID, status, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	recs := make([]*SessionMetadataRecord, 0, limit+1)
	for rows.Next() {
		var rec SessionMetadataRecord
		var omittedKind, omittedURL, omittedLang bool
		if err := rows.Scan(&rec.ID, &rec.Kind, &rec.HostAssetID, &rec.URL, &rec.Lang,
			&rec.Status, &rec.CreatedByTask, &rec.CreatedByIntent, &rec.LastBeat,
			&omittedKind, &omittedURL, &omittedLang); err != nil {
			return nil, false, err
		}
		if omittedKind {
			rec.MetadataOmitted = append(rec.MetadataOmitted, "kind")
		}
		if omittedURL {
			// The public agent projection exposes only host, never the raw URL.
			rec.MetadataOmitted = append(rec.MetadataOmitted, "host")
		}
		if omittedLang {
			rec.MetadataOmitted = append(rec.MetadataOmitted, "lang")
		}
		recs = append(recs, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(recs) > limit
	if hasMore {
		recs = recs[:limit]
	}
	return recs, hasMore, nil
}
