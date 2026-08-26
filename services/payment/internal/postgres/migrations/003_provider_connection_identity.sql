-- This is a coordinated contract migration. Stop old binaries before applying;
-- they do not write the execution identity columns introduced here.

ALTER TABLE payment_attempts
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text;

UPDATE payment_attempts
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL;

ALTER TABLE payment_attempts
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL,
    ADD CONSTRAINT payment_attempts_execution_identity_unique
        UNIQUE (
            id,
            payment_intent_id,
            provider_connection_id,
            provider_connection_revision,
            provider_namespace_id,
            provider
        );

DROP INDEX IF EXISTS payment_attempts_provider_reference_unique;

CREATE UNIQUE INDEX payment_attempts_namespace_reference_unique
    ON payment_attempts (provider_namespace_id, provider_payment_reference)
    WHERE provider_payment_reference IS NOT NULL;

ALTER TABLE provider_operations
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text;

UPDATE provider_operations
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL;

ALTER TABLE provider_operations
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL,
    DROP CONSTRAINT provider_operations_provider_provider_idempotency_key_key,
    DROP CONSTRAINT provider_operations_provider_merchant_reference_key,
    DROP CONSTRAINT provider_operations_attempt_intent_fk,
    ADD CONSTRAINT provider_operations_namespace_idempotency_key
        UNIQUE (provider_namespace_id, provider_idempotency_key),
    ADD CONSTRAINT provider_operations_namespace_merchant_reference
        UNIQUE (provider_namespace_id, merchant_reference),
    ADD CONSTRAINT provider_operations_attempt_execution_fk
        FOREIGN KEY (
            payment_attempt_id,
            payment_intent_id,
            provider_connection_id,
            provider_connection_revision,
            provider_namespace_id,
            provider
        )
        REFERENCES payment_attempts (
            id,
            payment_intent_id,
            provider_connection_id,
            provider_connection_revision,
            provider_namespace_id,
            provider
        );

ALTER TABLE provider_outcome_inbox
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text;

UPDATE provider_outcome_inbox
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL;

UPDATE provider_outcome_inbox
SET sanitized_payload = jsonb_set(
    jsonb_set(
        jsonb_set(
            sanitized_payload,
            '{provider_connection_id}',
            to_jsonb(provider_connection_id),
            true
        ),
        '{provider_connection_revision}',
        to_jsonb(provider_connection_revision),
        true
    ),
    '{provider_namespace_id}',
    to_jsonb(provider_namespace_id),
    true
)
WHERE sanitized_payload ->> 'provider_connection_id' IS DISTINCT FROM provider_connection_id
   OR sanitized_payload ->> 'provider_connection_revision' IS DISTINCT FROM provider_connection_revision
   OR sanitized_payload ->> 'provider_namespace_id' IS DISTINCT FROM provider_namespace_id;

ALTER TABLE provider_outcome_inbox
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL,
    DROP CONSTRAINT provider_outcome_inbox_pkey,
    ADD PRIMARY KEY (provider_namespace_id, event_id);

ALTER TABLE provider_outcome_quarantine
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text;

UPDATE provider_outcome_quarantine
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL;

UPDATE provider_outcome_quarantine
SET sanitized_payload = jsonb_set(
    jsonb_set(
        jsonb_set(
            sanitized_payload,
            '{provider_connection_id}',
            to_jsonb(provider_connection_id),
            true
        ),
        '{provider_connection_revision}',
        to_jsonb(provider_connection_revision),
        true
    ),
    '{provider_namespace_id}',
    to_jsonb(provider_namespace_id),
    true
)
WHERE sanitized_payload ->> 'provider_connection_id' IS DISTINCT FROM provider_connection_id
   OR sanitized_payload ->> 'provider_connection_revision' IS DISTINCT FROM provider_connection_revision
   OR sanitized_payload ->> 'provider_namespace_id' IS DISTINCT FROM provider_namespace_id;

ALTER TABLE provider_outcome_quarantine
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL;

DROP INDEX IF EXISTS provider_outcome_quarantine_event_idx;

CREATE INDEX provider_outcome_quarantine_event_idx
    ON provider_outcome_quarantine (provider_namespace_id, event_id, received_at);
