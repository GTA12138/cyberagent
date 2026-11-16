-- The business row, immutable event and each recipient are committed together.
-- There is deliberately no max(id) consumer watermark: sequences are allocated
-- before commit, and neither task IDs nor event IDs imply commit order.
CREATE TABLE trigger_events (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL,
    task_id BIGINT,
    source_id BIGINT,
    snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE trigger_deliveries ADD COLUMN event_id BIGINT REFERENCES trigger_events(id);
ALTER TABLE trigger_deliveries ADD COLUMN trigger_id BIGINT;
ALTER TABLE trigger_deliveries ADD COLUMN lease_until TIMESTAMPTZ;
ALTER TABLE trigger_deliveries ADD COLUMN execution_started_at TIMESTAMPTZ;
ALTER TABLE trigger_deliveries ADD COLUMN conversation_id BIGINT;
-- Legacy running jobs may already have performed external actions.
UPDATE trigger_deliveries SET execution_started_at=created_at WHERE state='running';
CREATE UNIQUE INDEX trigger_delivery_recipient ON trigger_deliveries(event_id,trigger_id);
CREATE INDEX trigger_delivery_lease ON trigger_deliveries(lease_until) WHERE state='running';

-- Legacy tool_names is editable text. A malformed filter must not abort the
-- business write that emits an unrelated event.
CREATE FUNCTION artex_trigger_tool_matches(raw_names TEXT, tool_name TEXT)
RETURNS BOOLEAN LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE names JSONB;
BEGIN
    names := COALESCE(NULLIF(raw_names,''),'[]')::JSONB;
    IF jsonb_typeof(names) IS DISTINCT FROM 'array' THEN RETURN false; END IF;
    RETURN EXISTS(SELECT 1 FROM jsonb_array_elements_text(names) name WHERE lower(name)=lower(tool_name));
EXCEPTION WHEN invalid_text_representation THEN RETURN false;
END $$;

CREATE FUNCTION artex_emit_trigger_event(event_kind TEXT, snap JSONB, only_trigger BIGINT DEFAULT NULL)
RETURNS BIGINT LANGUAGE plpgsql AS $$
DECLARE eid BIGINT;
BEGIN
    INSERT INTO trigger_events(kind,task_id,source_id,snapshot)
    VALUES(event_kind,(snap->>'task_id')::BIGINT,(snap->>'node_id')::BIGINT,snap)
    RETURNING id INTO eid;
    INSERT INTO trigger_deliveries(agent_key,event_key,event_id,trigger_id,payload)
    SELECT tr.agent_key,'event:' || eid || ':trigger:' || tr.id,eid,tr.id,
           jsonb_build_object('agent_key',tr.agent_key,'task_id',COALESCE((snap->>'task_id')::BIGINT,0),
             'mergeable',event_kind<>'interval','source_kind',event_kind,'source_event',snap,
             'trigger',to_jsonb(tr) - 'tool_names' - 'created_at' - 'updated_at')
    FROM agent_triggers tr
    WHERE tr.enabled AND (only_trigger IS NULL OR tr.id=only_trigger) AND
      ((event_kind='created' AND tr.on_task_create) OR
       (event_kind='timeout' AND tr.on_task_timeout) OR
       (event_kind='finding' AND tr.on_finding) OR
       (event_kind='goal' AND tr.on_goal_met) OR
       (event_kind='interval' AND tr.interval_sec>0) OR
       (event_kind='tool' AND tr.on_tool_call AND artex_trigger_tool_matches(tr.tool_names,snap->>'tool')));
    RETURN eid;
END $$;

CREATE FUNCTION artex_capture_task_trigger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE event_kind TEXT;
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN RETURN NEW; END IF;
    IF TG_OP='INSERT' THEN event_kind := 'created';
    ELSIF NEW.status='timeout' AND OLD.status IS DISTINCT FROM NEW.status THEN event_kind := 'timeout';
    ELSE RETURN NEW;
    END IF;
    PERFORM artex_emit_trigger_event(event_kind,jsonb_build_object(
        'node_id',NEW.id,'task_id',NEW.id,'task_description',NEW.description,'task_goal',NEW.goal));
    RETURN NEW;
END $$;
CREATE TRIGGER artex_task_trigger AFTER INSERT OR UPDATE OF status ON tasks
FOR EACH ROW EXECUTE FUNCTION artex_capture_task_trigger();

CREATE FUNCTION artex_capture_node_trigger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE t tasks%ROWTYPE; event_kind TEXT;
BEGIN
    IF NEW.kind='finding' AND TG_OP='INSERT' THEN event_kind := 'finding';
    ELSIF NEW.kind='goal' AND NEW.state='met' THEN
        IF TG_OP='UPDATE' AND OLD.state='met' THEN RETURN NEW; END IF;
        event_kind := 'goal';
    ELSE RETURN NEW;
    END IF;
    SELECT * INTO t FROM tasks WHERE exploration_id=NEW.exploration_id AND deleted_at IS NULL;
    IF NOT FOUND THEN RETURN NEW; END IF;
    PERFORM artex_emit_trigger_event(event_kind,jsonb_build_object(
        'node_id',NEW.id,'task_id',t.id,'task_description',t.description,'task_goal',t.goal,
        'summary',COALESCE(NEW.payload->>'text',NEW.payload->>'summary',''),
        'vulnclass',COALESCE(NEW.payload->>'vulnclass',''),'severity',COALESCE(NEW.payload->>'severity','')));
    RETURN NEW;
END $$;
CREATE TRIGGER artex_node_trigger AFTER INSERT OR UPDATE OF state ON exploration_nodes
FOR EACH ROW EXECUTE FUNCTION artex_capture_node_trigger();

CREATE FUNCTION artex_capture_tool_trigger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE t tasks%ROWTYPE; tool_input TEXT;
BEGIN
    IF NEW.kind IS DISTINCT FROM 'tool_result' OR COALESCE(NEW.tool,'')='' OR
       (NEW.tool='report_finding' AND NEW.is_error) THEN RETURN NEW; END IF;
    SELECT * INTO t FROM tasks WHERE exploration_id=NEW.exploration_id AND deleted_at IS NULL;
    IF NOT FOUND THEN RETURN NEW; END IF;
    SELECT COALESCE(detail,'') INTO tool_input FROM activity
      WHERE exploration_id=NEW.exploration_id AND node_id IS NOT DISTINCT FROM NEW.node_id
        AND tool_use_id=NEW.tool_use_id AND kind='tool_use' ORDER BY id DESC LIMIT 1;
    PERFORM artex_emit_trigger_event('tool',jsonb_build_object(
        'node_id',NEW.id,'task_id',t.id,'task_description',t.description,'task_goal',t.goal,
        'tool',NEW.tool,'tool_input',left(COALESCE(tool_input,''),1500),
        'tool_output',left(COALESCE(NEW.detail,''),1500),'tool_is_err',NEW.is_error));
    RETURN NEW;
END $$;
CREATE TRIGGER artex_tool_trigger AFTER INSERT ON activity
FOR EACH ROW EXECUTE FUNCTION artex_capture_tool_trigger();
