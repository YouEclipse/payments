-- Coordinated upgrade: stop old Payment binaries before applying this migration.
-- This is the local SALE recovery policy, NOT a production PSP retention promise.
-- Legacy first-dispatch times were not recorded: preserve NULL, never invent them.
ALTER TABLE provider_operations
    ADD COLUMN first_dispatched_at timestamptz,
    ADD COLUMN recovery_deadline_at timestamptz,
    ADD COLUMN recovery_budget_remaining integer,
    ADD COLUMN recovery_policy_version text,
    ADD COLUMN recovery_lease_until timestamptz,
    ADD COLUMN recovery_claim_generation bigint NOT NULL DEFAULT 0,
    ADD COLUMN recovery_claim_basis text NOT NULL DEFAULT 'none',
    ADD COLUMN recovery_action text NOT NULL DEFAULT 'wait',
    ADD COLUMN recovery_stop_reason text,
    ADD COLUMN recovery_evidence jsonb;

UPDATE provider_operations
SET recovery_deadline_at = created_at + interval '24 hours',
    recovery_budget_remaining = greatest(0, 32 - greatest(0, retry_count)),
    recovery_policy_version = 'local_sale_recovery.v1';

ALTER TABLE provider_operations
    ALTER COLUMN recovery_deadline_at SET NOT NULL,
    ALTER COLUMN recovery_budget_remaining SET NOT NULL,
    ALTER COLUMN recovery_policy_version SET NOT NULL,
    ADD CONSTRAINT provider_operations_recovery_bounds CHECK (
        recovery_deadline_at > created_at
        AND recovery_budget_remaining BETWEEN 0 AND 32
        AND recovery_claim_generation >= 0
        AND recovery_policy_version = 'local_sale_recovery.v1'
        AND recovery_action IN ('query', 'wait', 'same_key_resubmit', 'stop')
        AND recovery_claim_basis IN ('none', 'query', 'first_gateway_io', 'provider_recovery')
        AND CASE recovery_action
            WHEN 'query' THEN recovery_claim_basis = 'query'
            WHEN 'same_key_resubmit' THEN recovery_claim_basis IN ('first_gateway_io', 'provider_recovery')
            ELSE recovery_claim_basis = 'none'
        END
        AND (recovery_lease_until IS NULL OR (
            recovery_action IN ('query', 'same_key_resubmit')
            AND recovery_lease_until <= recovery_deadline_at
        ))
    ),
    ADD CONSTRAINT provider_operations_recovery_evidence_identity CHECK (
        recovery_evidence IS NULL OR (
            jsonb_typeof(recovery_evidence) = 'object'
            AND recovery_evidence->>'kind' IN (
                'gateway_operation_absent', 'gateway_never_dispatched', 'provider_conclusive_absent'
            )
            AND recovery_evidence->'identity'->>'operation_id' = id
            AND recovery_evidence->'identity'->>'attempt_id' = payment_attempt_id
            AND recovery_evidence->'identity'->>'provider_connection_id' = provider_connection_id
            AND recovery_evidence->'identity'->>'provider_connection_revision' = provider_connection_revision
            AND recovery_evidence->'identity'->>'provider_namespace_id' = provider_namespace_id
            AND recovery_evidence->'identity'->>'provider' = provider
            AND recovery_evidence->'identity'->>'provider_idempotency_key' = provider_idempotency_key
            AND recovery_evidence->'identity'->>'payload_hash' = encode(request_hash, 'hex')
            AND recovery_evidence->>'observed_at' IS NOT NULL
            AND recovery_evidence->>'valid_until' IS NOT NULL
        ) IS TRUE
    );

CREATE FUNCTION preserve_payment_operation_recovery_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.payment_intent_id IS DISTINCT FROM OLD.payment_intent_id
       OR NEW.payment_attempt_id IS DISTINCT FROM OLD.payment_attempt_id
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.amount_minor IS DISTINCT FROM OLD.amount_minor
       OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.provider_connection_id IS DISTINCT FROM OLD.provider_connection_id
       OR NEW.provider_connection_revision IS DISTINCT FROM OLD.provider_connection_revision
       OR NEW.provider_namespace_id IS DISTINCT FROM OLD.provider_namespace_id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.provider_idempotency_key IS DISTINCT FROM OLD.provider_idempotency_key
       OR NEW.merchant_reference IS DISTINCT FROM OLD.merchant_reference
       OR NEW.request_hash IS DISTINCT FROM OLD.request_hash
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'payment operation execution identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.recovery_deadline_at IS DISTINCT FROM OLD.recovery_deadline_at
       OR NEW.recovery_policy_version IS DISTINCT FROM OLD.recovery_policy_version
       OR NEW.recovery_budget_remaining > OLD.recovery_budget_remaining
       OR NEW.recovery_claim_generation < OLD.recovery_claim_generation
       OR (OLD.first_dispatched_at IS NOT NULL AND NEW.first_dispatched_at IS DISTINCT FROM OLD.first_dispatched_at) THEN
        RAISE EXCEPTION 'recovery deadline, budget and dispatch history cannot be reset' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_operation_recovery_identity_immutable
BEFORE UPDATE ON provider_operations
FOR EACH ROW EXECUTE FUNCTION preserve_payment_operation_recovery_identity();

CREATE INDEX provider_operations_recovery_due_idx
    ON provider_operations (COALESCE(next_retry_at, updated_at), recovery_lease_until)
    WHERE status IN ('created', 'sending', 'retry_scheduled', 'pending', 'unknown');

-- The bytes and merchant-global key scope do not change. This labels the
-- existing typed-command SHA-256 format so a future format cannot silently
-- reinterpret a durable key. There is intentionally no TTL deletion here.
ALTER TABLE idempotency_records
    ADD COLUMN fingerprint_version text NOT NULL DEFAULT 'command_sha256.v1',
    ADD CONSTRAINT idempotency_fingerprint_version_supported
        CHECK (fingerprint_version = 'command_sha256.v1');

CREATE FUNCTION preserve_payment_idempotency_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.operation IS DISTINCT FROM OLD.operation
       OR NEW.request_hash IS DISTINCT FROM OLD.request_hash
       OR NEW.fingerprint_version IS DISTINCT FROM OLD.fingerprint_version
       OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'payment command idempotency identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'completed' AND (
        NEW.state IS DISTINCT FROM OLD.state
        OR NEW.response_status IS DISTINCT FROM OLD.response_status
        OR NEW.response_body IS DISTINCT FROM OLD.response_body
    ) THEN
        RAISE EXCEPTION 'completed payment command response is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payment_idempotency_identity_immutable
BEFORE UPDATE ON idempotency_records
FOR EACH ROW EXECUTE FUNCTION preserve_payment_idempotency_identity();
