CREATE TABLE tool_execution_attempts (
 id BIGSERIAL PRIMARY KEY,
 task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 intent_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
 session_key TEXT NOT NULL,
 tool_use_id TEXT NOT NULL,
 tool TEXT NOT NULL,
 input_hash TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('started','completed','unknown','not_executed')),
 result JSONB,
 assessment TEXT NOT NULL DEFAULT '',
 started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 completed_at TIMESTAMPTZ,
 UNIQUE(task_id,session_key,tool_use_id)
);
CREATE INDEX idx_tool_attempts_unresolved ON tool_execution_attempts(task_id,intent_id) WHERE state IN ('started','unknown');
