CREATE TABLE trigger_deliveries (
 id BIGSERIAL PRIMARY KEY,
 agent_key TEXT NOT NULL,
 event_key TEXT UNIQUE,
 payload JSONB NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','completed','failed')),
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 last_error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_trigger_deliveries_pending ON trigger_deliveries(agent_key,id) WHERE state='pending';
