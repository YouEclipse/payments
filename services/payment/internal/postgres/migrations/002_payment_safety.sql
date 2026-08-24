ALTER TABLE provider_outcome_inbox
    ADD COLUMN rejection_reason text;

ALTER TABLE payment_attempts
    ADD CONSTRAINT payment_attempts_id_intent_unique
    UNIQUE (id, payment_intent_id),
    ADD CONSTRAINT payment_attempts_captured_reference_check
    CHECK (status <> 'captured' OR provider_payment_reference IS NOT NULL);

ALTER TABLE provider_operations
    ADD CONSTRAINT provider_operations_attempt_intent_fk
    FOREIGN KEY (payment_attempt_id, payment_intent_id)
    REFERENCES payment_attempts (id, payment_intent_id),
    ADD CONSTRAINT provider_operations_succeeded_reference_check
    CHECK (status <> 'succeeded' OR provider_operation_reference IS NOT NULL);

ALTER TABLE payment_intents
    ADD CONSTRAINT payment_intents_active_attempt_owner_fk
    FOREIGN KEY (active_attempt_id, id)
    REFERENCES payment_attempts (id, payment_intent_id),
    ADD CONSTRAINT payment_intents_latest_attempt_owner_fk
    FOREIGN KEY (latest_attempt_id, id)
    REFERENCES payment_attempts (id, payment_intent_id),
    ADD CONSTRAINT payment_intents_winning_attempt_owner_fk
    FOREIGN KEY (winning_attempt_id, id)
    REFERENCES payment_attempts (id, payment_intent_id),
    ADD CONSTRAINT payment_intents_succeeded_amounts_check
    CHECK (
        status <> 'succeeded'
        OR (
            authorized_amount_minor = amount_minor
            AND captured_amount_minor = amount_minor
            AND winning_attempt_id IS NOT NULL
            AND active_attempt_id IS NULL
        )
    );

CREATE TABLE provider_outcome_quarantine (
    id bigserial PRIMARY KEY,
    provider text NOT NULL,
    event_id text NOT NULL,
    operation_id text,
    provider_idempotency_key text,
    reason text NOT NULL,
    sanitized_payload jsonb NOT NULL,
    received_at timestamptz NOT NULL
);

CREATE INDEX provider_outcome_quarantine_event_idx
    ON provider_outcome_quarantine (provider, event_id, received_at);

CREATE INDEX idempotency_records_resource_in_progress_idx
    ON idempotency_records (resource_id)
    WHERE state = 'in_progress';
