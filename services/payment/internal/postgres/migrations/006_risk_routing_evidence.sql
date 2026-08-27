-- routing_context.v2 is executable only when a fresh ALLOW decision and the
-- selected optimized order are pinned with the Attempt. Risk rejection never
-- reaches this table because it creates no funds path.
ALTER TABLE payment_attempts
    ADD COLUMN routing_executable_until timestamptz;

ALTER TABLE payment_attempts
    DROP CONSTRAINT payment_attempts_routing_snapshots_together,
    ADD CONSTRAINT payment_attempts_routing_snapshots_together CHECK (
        (routing_context_snapshot IS NULL AND routing_decision_snapshot IS NULL)
        OR (
            jsonb_typeof(routing_context_snapshot) = 'object'
            AND jsonb_typeof(routing_decision_snapshot) = 'object'
            AND routing_context_snapshot->>'context_schema_version' IN ('routing_context.v1', 'routing_context.v2')
            AND routing_decision_snapshot->>'context_schema_version' = routing_context_snapshot->>'context_schema_version'
            AND routing_context_snapshot->'base'->>'payment_id' = payment_intent_id
            AND routing_context_snapshot->'base'->>'attempt_id' = id
            AND routing_decision_snapshot->>'decision_id' = routing_decision_id
            AND routing_decision_snapshot->'selected_candidate'->>'candidate_id' = provider_connection_id
            AND routing_decision_snapshot->'selected_candidate'->>'connection_revision' = provider_connection_revision
            AND routing_decision_snapshot->'selected_candidate'->>'provider_namespace_id' = provider_namespace_id
            AND routing_decision_snapshot->'selected_candidate'->>'provider' = provider
            AND routing_decision_snapshot->'candidate_order'->>0 = provider_connection_id
            AND CASE routing_context_snapshot->>'context_schema_version'
                WHEN 'routing_context.v1' THEN
                    NOT (routing_context_snapshot ? 'risk')
                    AND NOT (routing_decision_snapshot ? 'risk')
                    AND NOT (routing_decision_snapshot ? 'optimization')
                WHEN 'routing_context.v2' THEN
                    jsonb_typeof(routing_context_snapshot->'risk') = 'object'
                    AND routing_context_snapshot->'risk'->>'action' = 'allow'
                    AND jsonb_typeof(routing_decision_snapshot->'risk') = 'object'
                    AND routing_decision_snapshot->'risk'->>'decision_id' = routing_context_snapshot->'risk'->>'decision_id'
                    AND routing_decision_snapshot->'risk'->>'version' = routing_context_snapshot->'risk'->>'version'
                    AND routing_decision_snapshot->'risk'->>'action' = 'allow'
                    AND routing_decision_snapshot->'risk'->>'outcome' = 'allowed'
                    AND routing_decision_snapshot->'risk'->'continue' = 'true'::jsonb
                    AND jsonb_typeof(routing_decision_snapshot->'optimization') = 'object'
                    AND routing_decision_snapshot->'optimization'->'scoring'->>'status' IN ('scored', 'static_fallback')
                    AND routing_decision_snapshot->'optimization'->'scoring'->'candidate_order'->>0 = provider_connection_id
                ELSE false
            END
        ) IS TRUE
    );

-- Existing v1 snapshots predate executable_until and remain readable for
-- already-dispatched recovery. The nullable relational column is deliberately
-- not backfilled: a fabricated historical deadline would manufacture routing
-- authority. New writes are enforced by the trigger below; the NOT VALID check
-- keeps legacy rows updatable while proving JSON/column equality whenever a
-- deadline is present.
-- Router truncates (never rounds up) execution authority to PostgreSQL's
-- microsecond precision before computing the immutable Decision ID.
ALTER TABLE payment_attempts
    DROP CONSTRAINT IF EXISTS payment_attempts_new_routing_authority_deadline,
    ADD CONSTRAINT payment_attempts_new_routing_authority_deadline CHECK (
        (
            routing_context_snapshot IS NULL
            AND routing_decision_snapshot IS NULL
            AND routing_executable_until IS NULL
        )
        OR (
            routing_context_snapshot IS NOT NULL
            AND routing_decision_snapshot IS NOT NULL
            AND (
                routing_executable_until IS NULL
                OR CASE
                    WHEN jsonb_typeof(routing_decision_snapshot->'executable_until') = 'string'
                         AND routing_decision_snapshot->>'executable_until' ~
                             '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]{1,6})?Z$'
                    THEN (routing_decision_snapshot->>'executable_until')::timestamptz = routing_executable_until
                    ELSE false
                END
            )
        )
    ) NOT VALID;

DROP TRIGGER payment_attempt_routing_snapshot_immutable ON payment_attempts;

CREATE OR REPLACE FUNCTION preserve_payment_attempt_routing_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.routing_context_snapshot IS DISTINCT FROM OLD.routing_context_snapshot
       OR NEW.routing_decision_snapshot IS DISTINCT FROM OLD.routing_decision_snapshot
       OR NEW.routing_executable_until IS DISTINCT FROM OLD.routing_executable_until THEN
        RAISE EXCEPTION 'routing evidence is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_attempt_routing_snapshot_immutable
BEFORE UPDATE OF routing_context_snapshot, routing_decision_snapshot, routing_executable_until ON payment_attempts
FOR EACH ROW EXECUTE FUNCTION preserve_payment_attempt_routing_snapshot();

CREATE FUNCTION validate_new_payment_attempt_routing_authority() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    parsed_executable_until timestamptz;
BEGIN
    IF NEW.routing_context_snapshot IS NULL AND NEW.routing_decision_snapshot IS NULL THEN
        IF NEW.routing_executable_until IS NOT NULL THEN
            RAISE EXCEPTION 'routing deadline requires routing snapshots' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.routing_context_snapshot IS NULL OR NEW.routing_decision_snapshot IS NULL
       OR NEW.routing_executable_until IS NULL
       OR jsonb_typeof(NEW.routing_decision_snapshot->'executable_until') IS DISTINCT FROM 'string' THEN
        RAISE EXCEPTION 'versioned routing snapshots require executable_until' USING ERRCODE = '23514';
    END IF;

    BEGIN
        parsed_executable_until := (NEW.routing_decision_snapshot->>'executable_until')::timestamptz;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'routing decision executable_until is not a timestamp' USING ERRCODE = '23514';
    END;
    IF parsed_executable_until IS DISTINCT FROM NEW.routing_executable_until THEN
        RAISE EXCEPTION 'routing deadline differs from decision snapshot' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_attempt_new_routing_authority
BEFORE INSERT ON payment_attempts
FOR EACH ROW EXECUTE FUNCTION validate_new_payment_attempt_routing_authority();
