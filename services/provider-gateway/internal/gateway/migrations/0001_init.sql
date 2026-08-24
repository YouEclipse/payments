CREATE SCHEMA IF NOT EXISTS provider_gateway;

CREATE TABLE IF NOT EXISTS provider_gateway.operations (
    operation_id text PRIMARY KEY,
    attempt_id text NOT NULL,
    provider text NOT NULL,
    provider_idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    status text NOT NULL CHECK (status IN ('created','sending','unknown','succeeded','failed','review_required')),
    side_effect_certainty text NOT NULL CHECK (side_effect_certainty IN ('none','possible','confirmed')),
    provider_reference text,
    failure_code text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE(provider, provider_idempotency_key)
);

CREATE TABLE IF NOT EXISTS provider_gateway.webhook_events (
    provider text NOT NULL,
    event_id text NOT NULL,
    operation_id text NOT NULL,
    sanitized_payload jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(provider, event_id)
);

CREATE TABLE IF NOT EXISTS provider_gateway.outbox (
    id bigserial PRIMARY KEY,
    provider text NOT NULL,
    event_id text NOT NULL,
    operation_id text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    UNIQUE(provider, event_id)
);

CREATE INDEX IF NOT EXISTS provider_gateway_outbox_pending_idx
    ON provider_gateway.outbox(id) WHERE delivered_at IS NULL;
