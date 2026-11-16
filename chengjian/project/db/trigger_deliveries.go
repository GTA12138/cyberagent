package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const TriggerLease = 90 * time.Second

var ErrTriggerLeaseLost = errors.New("trigger delivery lease lost")

type TriggerDelivery struct {
	ID       int64           `json:"id"`
	AgentKey string          `json:"agent_key"`
	Payload  json.RawMessage `json:"payload"`
	Attempts int             `json:"attempts"`
}

func (d *DB) EnqueueTrigger(ctx context.Context, agent, eventKey string, payload json.RawMessage) error {
	_, err := d.ExecContext(ctx, `INSERT INTO trigger_deliveries(agent_key,event_key,payload) VALUES ($1,NULLIF($2,''),$3) ON CONFLICT(event_key) DO NOTHING`, agent, eventKey, payload)
	return err
}
func (d *DB) PendingTriggerAgents(ctx context.Context) ([]string, error) {
	if err := d.expireTriggerLeases(ctx); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT agent_key FROM trigger_deliveries WHERE state='pending' AND next_attempt_at<=now() GROUP BY agent_key ORDER BY min(id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
func (d *DB) ClaimTriggers(ctx context.Context, key, merge string, limit int) ([]TriggerDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	limit = min(limit, 500)
	if merge == "none" {
		limit = 1
	}
	if err := d.expireTriggerLeases(ctx); err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,agent_key,payload,attempts FROM trigger_deliveries WHERE agent_key=$1 AND state='pending' AND next_attempt_at<=now() ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED`, key, limit)
	if err != nil {
		return nil, err
	}
	all := []TriggerDelivery{}
	for rows.Next() {
		var j TriggerDelivery
		if err = rows.Scan(&j.ID, &j.AgentKey, &j.Payload, &j.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := all
	if len(all) > 0 && merge == "by_task" {
		var head struct {
			TaskID    int64 `json:"task_id"`
			Mergeable bool  `json:"mergeable"`
		}
		_ = json.Unmarshal(all[0].Payload, &head)
		out = all[:1:1]
		if head.Mergeable {
			for _, j := range all[1:] {
				var next struct {
					TaskID    int64 `json:"task_id"`
					Mergeable bool  `json:"mergeable"`
				}
				_ = json.Unmarshal(j.Payload, &next)
				if next.Mergeable && next.TaskID == head.TaskID {
					out = append(out, j)
				}
			}
		}
	}
	for i := range out {
		if _, err = tx.ExecContext(ctx, `UPDATE trigger_deliveries SET state='running',attempts=attempts+1,execution_started_at=NULL,lease_until=now()+make_interval(secs=>$2) WHERE id=$1`, out[i].ID, TriggerLease.Seconds()); err != nil {
			return nil, err
		}
		out[i].Attempts++
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Attempts is the fencing token. A stale runner cannot renew or settle a new claim.
func (d *DB) FinishTrigger(ctx context.Context, id int64, attempt int, state, reason string) error {
	if state != "pending" && state != "completed" && state != "failed" {
		return fmt.Errorf("invalid trigger state %q", state)
	}
	delay := min(300, 5*(1<<min(max(attempt-1, 0), 6)))
	result, err := d.ExecContext(ctx, `UPDATE trigger_deliveries SET state=$3,last_error=$4,
 next_attempt_at=now()+make_interval(secs=>$5),lease_until=NULL,
 execution_started_at=CASE WHEN $3='pending' THEN NULL ELSE execution_started_at END
 WHERE id=$1 AND attempts=$2 AND state='running' AND lease_until>now()`, id, attempt, state, reason, delay)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTriggerLeaseLost
	}
	return nil
}

func (d *DB) changeTriggerLease(ctx context.Context, jobs []TriggerDelivery, start bool) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, job := range jobs {
		result, err := tx.ExecContext(ctx, `UPDATE trigger_deliveries
   SET lease_until=now()+make_interval(secs=>$3),
       execution_started_at=CASE WHEN $4 THEN COALESCE(execution_started_at,now()) ELSE execution_started_at END
   WHERE id=$1 AND attempts=$2 AND state='running' AND lease_until>now()`, job.ID, job.Attempts, TriggerLease.Seconds(), start)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrTriggerLeaseLost
		}
	}
	return tx.Commit()
}
func (d *DB) StartTriggerExecution(ctx context.Context, jobs []TriggerDelivery) error {
	return d.changeTriggerLease(ctx, jobs, true)
}
func (d *DB) RenewTriggerLease(ctx context.Context, jobs []TriggerDelivery) error {
	return d.changeTriggerLease(ctx, jobs, false)
}

// Retain the conversation identity even if its execution or settlement fails.
func (d *DB) LinkTriggerConversation(ctx context.Context, jobs []TriggerDelivery, conversationID int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, job := range jobs {
		result, err := tx.ExecContext(ctx, `UPDATE trigger_deliveries SET conversation_id=$3
   WHERE id=$1 AND attempts=$2 AND state='running' AND lease_until>now()`, job.ID, job.Attempts, conversationID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrTriggerLeaseLost
		}
	}
	return tx.Commit()
}
func (d *DB) expireTriggerLeases(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `UPDATE trigger_deliveries SET
 state=CASE WHEN execution_started_at IS NULL AND attempts<3 THEN 'pending' ELSE 'failed' END,
 last_error=CASE WHEN execution_started_at IS NULL THEN '执行前租约过期' ELSE '执行租约过期，结果未知；请核对会话' END,
 next_attempt_at=now(),lease_until=NULL
 WHERE state='running' AND lease_until<=now()`)
	return err
}

// The exclusive runtime owner calls this once on startup. Only claims that never
// entered the runner are safe to replay; started conversations require inspection.
func (d *DB) RecoverTriggerDeliveries(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `UPDATE trigger_deliveries SET
 state=CASE WHEN execution_started_at IS NULL AND attempts<3 THEN 'pending' ELSE 'failed' END,
 last_error=CASE WHEN execution_started_at IS NULL THEN '进程中断，执行前重新排队' ELSE '进程中断，执行结果未知；请核对已创建的会话' END,
 next_attempt_at=now(),lease_until=NULL WHERE state='running'`)
	return err
}

// EnqueueDueTriggerIntervals atomically creates the event/recipients and advances
// last_fire. A rollback leaves the interval due; a committed slot cannot duplicate.
func (d *DB) EnqueueDueTriggerIntervals(ctx context.Context) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM agent_triggers WHERE enabled AND interval_sec>0
 AND (last_fire IS NULL OR last_fire<=now()-make_interval(secs=>interval_sec))
 ORDER BY id FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `SELECT chengjian_emit_trigger_event('interval',jsonb_build_object('fired_at',now()),$1)`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_triggers SET last_fire=now() WHERE id=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
