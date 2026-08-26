-- Legacy attempts remain without a snapshot: never invent historical inputs.
-- New v2 decisions are pinned with the Attempt before any provider dispatch.
ALTER TABLE payment_attempts
    ADD COLUMN routing_context_snapshot jsonb,
    ADD COLUMN routing_decision_snapshot jsonb,
    ADD CONSTRAINT payment_attempts_routing_snapshots_together CHECK (
        (routing_context_snapshot IS NULL AND routing_decision_snapshot IS NULL)
        OR (
            jsonb_typeof(routing_context_snapshot) = 'object'
            AND jsonb_typeof(routing_decision_snapshot) = 'object'
            AND routing_context_snapshot->>'context_schema_version' = 'routing_context.v1'
            AND routing_context_snapshot->'base'->>'payment_id' = payment_intent_id
            AND routing_context_snapshot->'base'->>'attempt_id' = id
            AND routing_decision_snapshot->>'decision_id' = routing_decision_id
            AND routing_decision_snapshot->'selected_candidate'->>'candidate_id' = provider_connection_id
            AND routing_decision_snapshot->'selected_candidate'->>'connection_revision' = provider_connection_revision
            AND routing_decision_snapshot->'selected_candidate'->>'provider_namespace_id' = provider_namespace_id
            AND routing_decision_snapshot->'selected_candidate'->>'provider' = provider
        ) IS TRUE
    );

CREATE FUNCTION preserve_payment_attempt_routing_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.routing_context_snapshot IS DISTINCT FROM OLD.routing_context_snapshot
       OR NEW.routing_decision_snapshot IS DISTINCT FROM OLD.routing_decision_snapshot THEN
        RAISE EXCEPTION 'routing evidence is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_attempt_routing_snapshot_immutable
BEFORE UPDATE OF routing_context_snapshot, routing_decision_snapshot ON payment_attempts
FOR EACH ROW EXECUTE FUNCTION preserve_payment_attempt_routing_snapshot();
