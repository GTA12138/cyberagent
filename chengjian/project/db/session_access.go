package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	SessionActionExec         = "exec"
	SessionActionRead         = "read"
	SessionActionWrite        = "write"
	SessionActionRecon        = "recon"
	SessionActionTunnelProbe  = "tunnel_probe"
	SessionActionTunnelDeploy = "tunnel_deploy"
)

// SessionActions returns a fresh allowlist. Granting is a management operation,
// never an agent tool or a consequence of inheriting task background.
func SessionActions() []string {
	return []string{SessionActionExec, SessionActionRead, SessionActionWrite,
		SessionActionRecon, SessionActionTunnelProbe, SessionActionTunnelDeploy}
}

func normalizeSessionActions(actions []string) ([]string, error) {
	if len(actions) == 0 || len(actions) > len(SessionActions()) {
		return nil, fmt.Errorf("one or more explicit session actions are required")
	}
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		if !slices.Contains(SessionActions(), action) || slices.Contains(out, action) {
			return nil, fmt.Errorf("invalid or duplicate session action %q", action)
		}
		out = append(out, action)
	}
	slices.Sort(out)
	return out, nil
}

type SessionAccessGrant struct {
	SessionID int64      `json:"session_id"`
	TaskID    int64      `json:"task_id"`
	Actions   []string   `json:"actions"`
	Source    string     `json:"source"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (s *SessionStore) TaskActive(ctx context.Context, taskID int64) (bool, error) {
	if taskID <= 0 {
		return false, nil
	}
	if s == nil || s.d == nil {
		return false, fmt.Errorf("session authorization store unavailable")
	}
	var active bool
	err := s.d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&active)
	return active, err
}

// CanAccess reads authoritative ownership/grants without decrypting any secret.
// A missing caller, unavailable DB, archived/deleted task, revoked or expired
// grant cannot authorize an operation. Already running operations are not leases
// and are not remotely cancelled by revocation.
func (s *SessionStore) CanAccess(ctx context.Context, sessionID, taskID int64, action string) (bool, error) {
	if sessionID <= 0 || taskID <= 0 || !slices.Contains(SessionActions(), action) {
		return false, nil
	}
	if s == nil || s.d == nil {
		return false, fmt.Errorf("session authorization store unavailable")
	}
	var allowed bool
	err := s.d.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM sessions
JOIN tasks caller ON caller.id=$2 AND caller.deleted_at IS NULL AND caller.archived_at IS NULL
WHERE sessions.id=$1 AND (sessions.created_by_task=$2 OR EXISTS (
    SELECT 1 FROM session_access_grants g WHERE g.session_id=sessions.id AND g.task_id=$2
    AND $3=ANY(g.actions) AND g.revoked_at IS NULL
    AND (g.expires_at IS NULL OR g.expires_at>now())
)))`, sessionID, taskID, action).Scan(&allowed)
	return allowed, err
}

// ListAccessible applies the authorization predicate before reading/decrypting
// rows, so a corrupt unrelated secret cannot break this task's session list.
func (s *SessionStore) ListAccessible(ctx context.Context, taskID int64, status string) ([]*SessionRecord, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("task context is required")
	}
	if s == nil || s.d == nil {
		return nil, fmt.Errorf("session authorization store unavailable")
	}
	rows, err := s.d.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions
WHERE EXISTS (SELECT 1 FROM tasks caller WHERE caller.id=$1 AND caller.deleted_at IS NULL AND caller.archived_at IS NULL)
AND ($2='' OR status=$2)
AND (created_by_task=$1 OR EXISTS (SELECT 1 FROM session_access_grants g
    WHERE g.session_id=sessions.id AND g.task_id=$1 AND g.revoked_at IS NULL
    AND (g.expires_at IS NULL OR g.expires_at>now()))) ORDER BY id`, taskID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SessionRecord{}
	for rows.Next() {
		rec, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GrantAccess replaces the exact action set atomically. It is for authenticated
// management callers only; an explicit regrant is required to clear revocation.
func (s *SessionStore) GrantAccess(ctx context.Context, sessionID, taskID int64, actions []string, expiresAt *time.Time, source string) error {
	actions, err := normalizeSessionActions(actions)
	if err != nil {
		return err
	}
	if sessionID <= 0 || taskID <= 0 || strings.TrimSpace(source) == "" || len(source) > 256 {
		return fmt.Errorf("valid session, task and grant source are required")
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return fmt.Errorf("grant expiry must be in the future")
	}
	tx, err := s.d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=$1 FOR SHARE`, sessionID).Scan(&id); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR SHARE`, taskID).Scan(&id); err != nil {
		return err
	}
	if err := upsertSessionGrant(ctx, tx, sessionID, taskID, actions, expiresAt, source); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertSessionGrant(ctx context.Context, tx *sql.Tx, sessionID, taskID int64, actions []string, expiresAt *time.Time, source string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_access_grants(session_id,task_id,actions,expires_at,source)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (session_id,task_id) DO UPDATE SET
actions=EXCLUDED.actions,expires_at=EXCLUDED.expires_at,source=EXCLUDED.source,revoked_at=NULL,updated_at=now()`, sessionID, taskID, actions, expiresAt, source)
	return err
}

func (s *SessionStore) RevokeAccess(ctx context.Context, sessionID, taskID int64) error {
	_, err := s.d.ExecContext(ctx, `UPDATE session_access_grants SET revoked_at=now(),updated_at=now() WHERE session_id=$1 AND task_id=$2 AND revoked_at IS NULL`, sessionID, taskID)
	return err
}

func (s *SessionStore) AccessGrants(ctx context.Context, sessionID int64) ([]SessionAccessGrant, error) {
	rows, err := s.d.QueryContext(ctx, `SELECT session_id,task_id,to_json(actions),source,expires_at,revoked_at,updated_at FROM session_access_grants WHERE session_id=$1 ORDER BY task_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionAccessGrant{}
	for rows.Next() {
		var g SessionAccessGrant
		var actionsJSON []byte
		if err := rows.Scan(&g.SessionID, &g.TaskID, &actionsJSON, &g.Source, &g.ExpiresAt, &g.RevokedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(actionsJSON, &g.Actions); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GrantsForTask returns the still-valid action set this task holds on sessions
// it does not own, keyed by session id. It exists so agent-facing lists can say
// what the caller may *do* with a granted foothold, not merely that it exists:
// a session shown as usable but actually read-only invites intents that can
// only fail at execution time.
//
// One query per list, not per row. Expired and revoked grants are excluded with
// the same predicate CanAccess uses, so the projection can never be more
// permissive than enforcement.
func (s *SessionStore) GrantsForTask(ctx context.Context, taskID int64) (map[int64][]string, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("task context is required")
	}
	if s == nil || s.d == nil {
		return nil, fmt.Errorf("session authorization store unavailable")
	}
	rows, err := s.d.QueryContext(ctx, `SELECT session_id, to_json(actions) FROM session_access_grants
WHERE task_id=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var actionsJSON []byte
		if err := rows.Scan(&id, &actionsJSON); err != nil {
			return nil, err
		}
		var actions []string
		if err := json.Unmarshal(actionsJSON, &actions); err != nil {
			return nil, err
		}
		out[id] = actions
	}
	return out, rows.Err()
}

type SessionHandoffOptions struct {
	SessionID    int64
	SourceTaskID int64
	HostIP       string
}

func insertSessionHandoff(tx *sql.Tx, d *DB, taskID int64, sources []int64, handoff SessionHandoffOptions) (string, error) {
	if handoff.SessionID <= 0 || handoff.SourceTaskID <= 0 || net.ParseIP(handoff.HostIP) == nil || !slices.Contains(sources, handoff.SourceTaskID) {
		return "", fmt.Errorf("invalid session handoff")
	}
	var owner int64
	if err := tx.QueryRow(`SELECT COALESCE(created_by_task,0) FROM sessions WHERE id=$1 FOR SHARE`, handoff.SessionID).Scan(&owner); err != nil {
		return "", err
	}
	if owner != handoff.SourceTaskID {
		return "", fmt.Errorf("handoff session owner changed")
	}
	var source int64
	if err := tx.QueryRow(`SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR SHARE`, owner).Scan(&source); err != nil {
		return "", err
	}
	parent := strconv.FormatInt(source, 10)
	if _, err := tx.Exec(`UPDATE tasks SET parent_ref=$2 WHERE id=$1`, taskID, parent); err != nil {
		return "", err
	}
	scoped := &AssetStore{db: d, tx: tx}
	if _, err := scoped.AddAgentScope(taskID, "ip", handoff.HostIP, fmt.Sprintf("handoff session #%d", handoff.SessionID), "manual"); err != nil {
		return "", err
	}
	if err := upsertSessionGrant(context.Background(), tx, handoff.SessionID, taskID, SessionActions(), nil, "manual_handoff"); err != nil {
		return "", err
	}
	return parent, nil
}
