-- Existing webhook rows contain only the normalized payload. Leave their raw
-- hash NULL rather than inventing byte-level provenance. New rows persist a
-- hash of the sanitized Provider payload so a reused event ID with different
-- raw semantics is quarantined even when both values normalize alike.
ALTER TABLE provider_gateway.webhook_events
    ADD COLUMN provider_payload_hash bytea,
    ADD CONSTRAINT webhook_events_provider_payload_hash_length
        CHECK (provider_payload_hash IS NULL OR octet_length(provider_payload_hash) = 32);

-- Provider responses are evidence, not mutable attributes of the latest
-- operation row. Keep every accepted observation so that reducer decisions can
-- be reconstructed without claiming that a Provider delivered it exactly once.
CREATE TABLE provider_gateway.provider_operation_observations (
    id bigserial PRIMARY KEY,
    operation_id text NOT NULL REFERENCES provider_gateway.operations(operation_id),
    attempt_id text NOT NULL,
    provider_connection_id text NOT NULL,
    provider_connection_revision text NOT NULL,
    provider_namespace_id text NOT NULL,
    provider text NOT NULL,
    source text NOT NULL CHECK (source IN ('sale', 'query', 'webhook', 'legacy_seed')),
    raw_provider_status text NOT NULL CHECK (raw_provider_status <> ''),
    normalized_status text NOT NULL
        CHECK (normalized_status IN ('created','sending','unknown','succeeded','failed','review_required')),
    normalized_certainty text NOT NULL
        CHECK (normalized_certainty IN ('none','possible','confirmed')),
    normalized_provider_reference text,
    normalized_failure_code text,
    provider_event_id text,
    payload_hash bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    sanitized_payload jsonb NOT NULL CHECK (jsonb_typeof(sanitized_payload) = 'object'),
    observed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT provider_operation_observations_payload_hash_matches CHECK (
        payload_hash = sha256(convert_to(sanitized_payload::text, 'UTF8'))
    ),
    CONSTRAINT provider_operation_observations_runtime_status CHECK (
        source = 'legacy_seed'
        OR normalized_status IN ('unknown','succeeded','failed','review_required')
    )
);

CREATE INDEX provider_operation_observations_operation_time_idx
    ON provider_gateway.provider_operation_observations(operation_id, observed_at, id);

CREATE INDEX provider_operation_observations_provider_event_idx
    ON provider_gateway.provider_operation_observations(provider_namespace_id, provider_event_id)
    WHERE provider_event_id IS NOT NULL;

-- The migration has only the durable reduced snapshot for historical rows.
-- Mark that provenance explicitly instead of inventing a raw Provider payload.
WITH legacy AS (
    SELECT operation.*,
           jsonb_strip_nulls(jsonb_build_object(
               'kind', 'legacy_reduced_operation_snapshot',
               'operation_id', operation.operation_id,
               'status', operation.status,
               'side_effect_certainty', operation.side_effect_certainty,
               'provider_reference', operation.provider_reference,
               'failure_code', operation.failure_code,
               'snapshot_updated_at', operation.updated_at
           )) AS payload
    FROM provider_gateway.operations AS operation
)
INSERT INTO provider_gateway.provider_operation_observations (
    operation_id,
    attempt_id,
    provider_connection_id,
    provider_connection_revision,
    provider_namespace_id,
    provider,
    source,
    raw_provider_status,
    normalized_status,
    normalized_certainty,
    normalized_provider_reference,
    normalized_failure_code,
    provider_event_id,
    payload_hash,
    sanitized_payload,
    observed_at
)
SELECT operation_id,
       attempt_id,
       provider_connection_id,
       provider_connection_revision,
       provider_namespace_id,
       provider,
       'legacy_seed',
       status,
       status,
       side_effect_certainty,
       provider_reference,
       failure_code,
       NULL,
       sha256(convert_to(payload::text, 'UTF8')),
       payload,
       updated_at
FROM legacy;

CREATE FUNCTION provider_gateway.reject_provider_operation_observation_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'provider operation observations are append-only' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER provider_operation_observations_append_only
BEFORE UPDATE OR DELETE ON provider_gateway.provider_operation_observations
FOR EACH ROW EXECUTE FUNCTION provider_gateway.reject_provider_operation_observation_mutation();
