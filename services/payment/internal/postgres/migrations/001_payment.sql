CREATE TABLE payment_intents (
    id text PRIMARY KEY,
    merchant_id text NOT NULL,
    merchant_reference text NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency character(3) NOT NULL CHECK (currency = upper(currency)),
    payment_method_type text NOT NULL CHECK (payment_method_type = 'card'),
    payment_method_token text NOT NULL,
    capture_method text NOT NULL CHECK (capture_method = 'automatic'),
    status text NOT NULL CHECK (status IN (
        'requires_confirmation', 'processing', 'requires_action',
        'succeeded', 'failed', 'canceled'
    )),
    integrity_status text NOT NULL DEFAULT 'healthy'
        CHECK (integrity_status IN ('healthy', 'review_required')),
    authorized_amount_minor bigint NOT NULL DEFAULT 0,
    captured_amount_minor bigint NOT NULL DEFAULT 0,
    refunded_amount_minor bigint NOT NULL DEFAULT 0,
    active_attempt_id text,
    latest_attempt_id text,
    winning_attempt_id text,
    version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (merchant_id, merchant_reference),
    CHECK (
        refunded_amount_minor >= 0
        AND refunded_amount_minor <= captured_amount_minor
        AND captured_amount_minor <= authorized_amount_minor
        AND authorized_amount_minor <= amount_minor
    )
);

CREATE TABLE payment_attempts (
    id text PRIMARY KEY,
    payment_intent_id text NOT NULL REFERENCES payment_intents(id),
    attempt_number integer NOT NULL CHECK (attempt_number > 0),
    routing_decision_id text NOT NULL UNIQUE,
    provider text NOT NULL,
    status text NOT NULL CHECK (status IN (
        'created', 'processing', 'unknown', 'captured', 'declined', 'failed'
    )),
    outcome_code text,
    reroute_disposition text NOT NULL DEFAULT 'blocked'
        CHECK (reroute_disposition IN ('blocked', 'allowed')),
    provider_payment_reference text,
    version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (payment_intent_id, attempt_number)
);

CREATE UNIQUE INDEX payment_attempts_provider_reference_unique
    ON payment_attempts (provider, provider_payment_reference)
    WHERE provider_payment_reference IS NOT NULL;

CREATE TABLE provider_operations (
    id text PRIMARY KEY,
    payment_intent_id text NOT NULL REFERENCES payment_intents(id),
    payment_attempt_id text NOT NULL REFERENCES payment_attempts(id),
    kind text NOT NULL CHECK (kind = 'sale'),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency character(3) NOT NULL,
    provider text NOT NULL,
    provider_idempotency_key text NOT NULL,
    merchant_reference text NOT NULL,
    request_hash bytea NOT NULL,
    status text NOT NULL CHECK (status IN (
        'created', 'sending', 'retry_scheduled', 'pending',
        'unknown', 'review_required', 'succeeded', 'failed'
    )),
    side_effect_certainty text NOT NULL
        CHECK (side_effect_certainty IN ('none', 'possible', 'confirmed')),
    retry_mode text NOT NULL CHECK (retry_mode IN ('same_key', 'query_or_wait', 'none')),
    provider_operation_reference text,
    normalized_code text,
    provider_code text,
    retry_count integer NOT NULL DEFAULT 0,
    next_retry_at timestamptz,
    last_error text,
    sanitized_result jsonb,
    version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (provider, provider_idempotency_key),
    UNIQUE (provider, merchant_reference),
    UNIQUE (payment_attempt_id)
);

ALTER TABLE payment_intents
    ADD CONSTRAINT payment_intents_active_attempt_fk
    FOREIGN KEY (active_attempt_id) REFERENCES payment_attempts(id),
    ADD CONSTRAINT payment_intents_latest_attempt_fk
    FOREIGN KEY (latest_attempt_id) REFERENCES payment_attempts(id),
    ADD CONSTRAINT payment_intents_winning_attempt_fk
    FOREIGN KEY (winning_attempt_id) REFERENCES payment_attempts(id);

CREATE INDEX provider_operations_recovery_idx
    ON provider_operations (status, updated_at)
    WHERE status IN ('created', 'sending', 'retry_scheduled', 'pending', 'unknown');

CREATE TABLE idempotency_records (
    merchant_id text NOT NULL,
    idempotency_key text NOT NULL,
    operation text NOT NULL,
    request_hash bytea NOT NULL,
    state text NOT NULL CHECK (state IN ('in_progress', 'completed')),
    resource_id text,
    response_status integer,
    response_body bytea,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, idempotency_key)
);

CREATE TABLE provider_outcome_inbox (
    provider text NOT NULL,
    event_id text NOT NULL,
    operation_id text,
    provider_idempotency_key text,
    status text NOT NULL,
    side_effect_certainty text NOT NULL,
    normalized_code text,
    provider_code text,
    provider_payment_reference text,
    provider_operation_reference text,
    occurred_at timestamptz,
    received_at timestamptz NOT NULL,
    processed_at timestamptz,
    processing_status text NOT NULL CHECK (processing_status IN ('received', 'processed', 'conflict', 'rejected')),
    sanitized_payload jsonb NOT NULL,
    PRIMARY KEY (provider, event_id)
);

CREATE TABLE outbox_events (
    id text PRIMARY KEY,
    aggregate_type text NOT NULL,
    aggregate_id text NOT NULL,
    aggregate_version bigint NOT NULL,
    event_type text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    available_at timestamptz NOT NULL,
    published_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    last_error text,
    UNIQUE (aggregate_type, aggregate_id, aggregate_version, event_type)
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (available_at, created_at)
    WHERE published_at IS NULL;
