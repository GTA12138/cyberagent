package db

import (
	"context"
	"database/sql"
	"time"
)

// SessionHealth is a non-secret observation token for a probe. A probe may
// update only the exact status/heartbeat it started from.
type SessionHealth struct {
	Status   string
	LastBeat time.Time
}

func (s *SessionStore) Health(ctx context.Context, id int64) (*SessionHealth, error) {
	var health SessionHealth
	err := s.d.QueryRowContext(ctx, `SELECT status, last_beat FROM sessions WHERE id=$1`, id).
		Scan(&health.Status, &health.LastBeat)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &health, nil
}

// ApplyProbe atomically records a conclusive observation. A changed status,
// successful operation heartbeat, or deletion invalidates the old token.
// Repeated failures with identical status/heartbeat are not ordered by this
// token; this is not a complete observation revision ledger.
// Inconclusive/canceled probes must never call this method.
func (s *SessionStore) ApplyProbe(ctx context.Context, id int64, expected SessionHealth, alive bool) (bool, error) {
	status := SessionDead
	if alive {
		status = SessionAlive
	}
	result, err := s.d.ExecContext(ctx, `UPDATE sessions
SET status=$4, last_beat=CASE WHEN $5 THEN clock_timestamp() ELSE last_beat END
WHERE id=$1 AND status=$2 AND last_beat=$3`, id, expected.Status, expected.LastBeat, status, alive)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}
