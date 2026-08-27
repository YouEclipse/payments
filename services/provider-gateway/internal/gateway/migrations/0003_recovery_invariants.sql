-- Existing non-created operations predate a durable dispatch marker. Preserve
-- their NULL history rather than inventing a Provider call time. The trigger
-- below requires every future Created -> non-Created transition to set it.
ALTER TABLE provider_gateway.operations
    ADD COLUMN first_dispatched_at timestamptz,
    ADD COLUMN dispatch_lease_until timestamptz,
    ADD CONSTRAINT operations_created_dispatch_history CHECK (
        (status <> 'created' OR first_dispatched_at IS NULL)
        AND (first_dispatched_at IS NULL OR first_dispatched_at >= created_at)
        AND (dispatch_lease_until IS NULL OR (status = 'sending' AND first_dispatched_at IS NOT NULL))
    );

-- This capability belongs to a concrete connection revision, not a Provider
-- brand. Rows bound before 0003 adopt one explicit value on first new-binary
-- startup; after that, the trigger makes the value immutable.
ALTER TABLE provider_gateway.provider_connection_revisions
    ADD COLUMN conclusive_absence boolean;

CREATE FUNCTION provider_gateway.preserve_connection_revision_binding() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.provider_connection_id IS DISTINCT FROM OLD.provider_connection_id
       OR NEW.provider_connection_revision IS DISTINCT FROM OLD.provider_connection_revision
       OR NEW.provider_namespace_id IS DISTINCT FROM OLD.provider_namespace_id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.fingerprint_version IS DISTINCT FROM OLD.fingerprint_version
       OR NEW.config_fingerprint IS DISTINCT FROM OLD.config_fingerprint
       OR NEW.registered_at IS DISTINCT FROM OLD.registered_at
       OR (OLD.conclusive_absence IS NOT NULL
           AND NEW.conclusive_absence IS DISTINCT FROM OLD.conclusive_absence) THEN
        RAISE EXCEPTION 'provider connection revision binding is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER provider_connection_revision_binding_immutable
BEFORE UPDATE ON provider_gateway.provider_connection_revisions
FOR EACH ROW EXECUTE FUNCTION provider_gateway.preserve_connection_revision_binding();

CREATE FUNCTION provider_gateway.preserve_operation_execution_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (NEW.status = 'created' AND NEW.first_dispatched_at IS NOT NULL)
           OR (NEW.status NOT IN ('created', 'review_required') AND NEW.first_dispatched_at IS NULL) THEN
            RAISE EXCEPTION 'new provider operation dispatch history is inconsistent' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id
       OR NEW.provider_connection_id IS DISTINCT FROM OLD.provider_connection_id
       OR NEW.provider_connection_revision IS DISTINCT FROM OLD.provider_connection_revision
       OR NEW.provider_namespace_id IS DISTINCT FROM OLD.provider_namespace_id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.provider_idempotency_key IS DISTINCT FROM OLD.provider_idempotency_key
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'provider operation execution identity is immutable' USING ERRCODE = '23514';
    END IF;

    IF NEW.request_hash IS DISTINCT FROM OLD.request_hash
       OR NEW.request_hash_version IS DISTINCT FROM OLD.request_hash_version THEN
        IF NOT (
            OLD.request_hash_version = 1
            AND NEW.request_hash_version = 2
            AND OLD.provider_connection_revision = 'legacy_v1'
            AND OLD.provider_connection_id = OLD.provider
            AND OLD.provider_namespace_id = OLD.provider
            AND NEW.request_hash IS DISTINCT FROM OLD.request_hash
        ) THEN
            RAISE EXCEPTION 'provider operation request fingerprint is immutable' USING ERRCODE = '23514';
        END IF;
    END IF;

    IF OLD.status <> 'created' AND NEW.status = 'created' THEN
        RAISE EXCEPTION 'provider operation cannot return to created' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'created' AND NEW.status NOT IN ('created', 'review_required') AND NEW.first_dispatched_at IS NULL THEN
        RAISE EXCEPTION 'first provider dispatch must be recorded atomically' USING ERRCODE = '23514';
    END IF;
    IF OLD.first_dispatched_at IS NOT NULL
       AND NEW.first_dispatched_at IS DISTINCT FROM OLD.first_dispatched_at THEN
        RAISE EXCEPTION 'provider operation dispatch history is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER provider_operation_execution_identity_immutable
BEFORE INSERT OR UPDATE ON provider_gateway.operations
FOR EACH ROW EXECUTE FUNCTION provider_gateway.preserve_operation_execution_identity();
