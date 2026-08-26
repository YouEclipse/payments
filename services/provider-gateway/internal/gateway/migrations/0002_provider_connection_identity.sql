-- Historical rows do not contain their endpoint or credential configuration.
-- Leave this registry empty during migration; coordinated first startup must
-- bind the explicitly supplied original configuration, never an inferred PSP.
CREATE TABLE provider_gateway.provider_connection_revisions (
    provider_connection_id text NOT NULL,
    provider_connection_revision text NOT NULL,
    provider_namespace_id text NOT NULL,
    provider text NOT NULL,
    fingerprint_version smallint NOT NULL CHECK (fingerprint_version > 0),
    config_fingerprint text NOT NULL CHECK (config_fingerprint ~ '^[a-f0-9]{64}$'),
    registered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_connection_id, provider_connection_revision)
);

ALTER TABLE provider_gateway.operations
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text,
    ADD COLUMN request_hash_version smallint;

UPDATE provider_gateway.operations
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider,
    request_hash_version = 1
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL
   OR request_hash_version IS NULL;

ALTER TABLE provider_gateway.operations
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL,
    ALTER COLUMN request_hash_version SET NOT NULL,
    DROP CONSTRAINT operations_provider_provider_idempotency_key_key,
    ADD CONSTRAINT operations_namespace_idempotency_key
        UNIQUE (provider_namespace_id, provider_idempotency_key),
    ADD CONSTRAINT operations_request_hash_version_check
        CHECK (request_hash_version IN (1, 2));

CREATE INDEX provider_gateway_operations_nonterminal_connection_idx
    ON provider_gateway.operations (
        provider_connection_id,
        provider_connection_revision,
        provider_namespace_id,
        provider
    )
    WHERE status IN ('created', 'sending', 'unknown');

CREATE TABLE provider_gateway.provider_reference_claims (
    provider_namespace_id text NOT NULL,
    provider_reference text NOT NULL,
    operation_id text NOT NULL REFERENCES provider_gateway.operations(operation_id),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_namespace_id, provider_reference)
);

CREATE TABLE provider_gateway.provider_reference_observations (
    provider_namespace_id text NOT NULL,
    provider_reference text NOT NULL,
    operation_id text NOT NULL REFERENCES provider_gateway.operations(operation_id),
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_namespace_id, provider_reference, operation_id)
);

INSERT INTO provider_gateway.provider_reference_claims (
    provider_namespace_id,
    provider_reference,
    operation_id,
    claimed_at
)
SELECT DISTINCT ON (provider_namespace_id, provider_reference)
       provider_namespace_id,
       provider_reference,
       operation_id,
       created_at
FROM provider_gateway.operations
WHERE provider_reference IS NOT NULL
  AND provider_reference <> ''
ORDER BY provider_namespace_id, provider_reference, created_at, operation_id;

INSERT INTO provider_gateway.provider_reference_observations (
    provider_namespace_id,
    provider_reference,
    operation_id,
    observed_at
)
SELECT provider_namespace_id,
       provider_reference,
       operation_id,
       updated_at
FROM provider_gateway.operations
WHERE provider_reference IS NOT NULL
  AND provider_reference <> '';

WITH collisions AS (
    SELECT provider_namespace_id, provider_reference
    FROM provider_gateway.operations
    WHERE provider_reference IS NOT NULL
      AND provider_reference <> ''
    GROUP BY provider_namespace_id, provider_reference
    HAVING count(*) > 1
)
UPDATE provider_gateway.operations AS operation
SET status = 'review_required',
    side_effect_certainty = 'possible',
    failure_code = CASE
        WHEN operation.status = 'review_required' AND NULLIF(operation.failure_code, '') IS NOT NULL
        THEN operation.failure_code
        ELSE 'provider_reference_collision'
    END,
    updated_at = now()
FROM collisions
WHERE operation.provider_namespace_id = collisions.provider_namespace_id
  AND operation.provider_reference = collisions.provider_reference;

ALTER TABLE provider_gateway.webhook_events
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text;

UPDATE provider_gateway.webhook_events
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL;

UPDATE provider_gateway.webhook_events
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
    );

ALTER TABLE provider_gateway.webhook_events
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL,
    DROP CONSTRAINT webhook_events_pkey,
    ADD PRIMARY KEY (provider_namespace_id, event_id);

CREATE TABLE provider_gateway.webhook_event_conflicts (
    id bigserial PRIMARY KEY,
    provider_namespace_id text NOT NULL,
    event_id text NOT NULL,
    operation_id text NOT NULL,
    path_connection_id text NOT NULL,
    path_connection_revision text NOT NULL,
    payload_hash text NOT NULL,
    sanitized_payload jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_namespace_id, event_id, payload_hash)
);

ALTER TABLE provider_gateway.outbox
    ADD COLUMN provider_connection_id text,
    ADD COLUMN provider_connection_revision text,
    ADD COLUMN provider_namespace_id text;

UPDATE provider_gateway.outbox
SET provider_connection_id = provider,
    provider_connection_revision = 'legacy_v1',
    provider_namespace_id = provider
WHERE provider_connection_id IS NULL
   OR provider_connection_revision IS NULL
   OR provider_namespace_id IS NULL;

UPDATE provider_gateway.outbox
SET payload = jsonb_set(
        jsonb_set(
            jsonb_set(
                payload,
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
    );

ALTER TABLE provider_gateway.outbox
    ALTER COLUMN provider_connection_id SET NOT NULL,
    ALTER COLUMN provider_connection_revision SET NOT NULL,
    ALTER COLUMN provider_namespace_id SET NOT NULL,
    DROP CONSTRAINT outbox_provider_event_id_key,
    ADD CONSTRAINT outbox_namespace_event_id_key
        UNIQUE (provider_namespace_id, event_id);

WITH collision_operations AS (
    SELECT operation.*
    FROM provider_gateway.operations AS operation
    JOIN (
        SELECT provider_namespace_id, provider_reference
        FROM provider_gateway.operations
        WHERE provider_reference IS NOT NULL
          AND provider_reference <> ''
        GROUP BY provider_namespace_id, provider_reference
        HAVING count(*) > 1
    ) AS collision
      ON collision.provider_namespace_id = operation.provider_namespace_id
     AND collision.provider_reference = operation.provider_reference
)
INSERT INTO provider_gateway.outbox (
    provider_connection_id,
    provider_connection_revision,
    provider_namespace_id,
    provider,
    event_id,
    operation_id,
    payload
)
SELECT provider_connection_id,
       provider_connection_revision,
       provider_namespace_id,
       provider,
       'migration_provider_reference_collision_' || md5(
           jsonb_build_array(provider_namespace_id, provider_reference, operation_id)::text
       ),
       operation_id,
       jsonb_build_object(
           'event_id', 'migration_provider_reference_collision_' || md5(
               jsonb_build_array(provider_namespace_id, provider_reference, operation_id)::text
           ),
           'operation_id', operation_id,
           'provider_connection_id', provider_connection_id,
           'provider_connection_revision', provider_connection_revision,
           'provider_namespace_id', provider_namespace_id,
           'provider', provider,
           'status', 'review_required',
           'side_effect_certainty', 'possible',
           'provider_reference', provider_reference,
           'failure_code', 'provider_reference_collision',
           'occurred_at', updated_at
       )
FROM collision_operations
ON CONFLICT (provider_namespace_id, event_id) DO NOTHING;
