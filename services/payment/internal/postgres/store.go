package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/YouEclipse/payments/services/payment/internal/identifier"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound                  = errors.New("payment intent not found")
	ErrIdempotencyConflict       = errors.New("idempotency key reused for a different request")
	ErrMerchantReferenceConflict = errors.New("merchant reference already exists")
	ErrStateConflict             = errors.New("payment intent state conflict")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

type StoredResponse struct {
	Status int
	Body   []byte
}

type IdempotencyLookup struct {
	Found      bool
	InProgress bool
	ResourceID string
	Response   StoredResponse
}

type Aggregate struct {
	Intent    domain.PaymentIntent
	Attempt   domain.PaymentAttempt
	Operation domain.ProviderOperation
}

type CreateIntentCommand struct {
	Intent         domain.PaymentIntent
	IdempotencyKey string
	Operation      string
	RequestHash    []byte
	Response       StoredResponse
}

type CreateIntentResult struct {
	Intent domain.PaymentIntent
	Replay *StoredResponse
}

func (s *Store) CreateIntent(ctx context.Context, command CreateIntentCommand) (CreateIntentResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CreateIntentResult{}, fmt.Errorf("begin create intent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inserted, err := insertIdempotency(ctx, tx, command.Intent.MerchantID, command.IdempotencyKey, command.Operation, command.RequestHash, command.Intent.ID, command.Intent.CreatedAt)
	if err != nil {
		return CreateIntentResult{}, err
	}
	if !inserted {
		lookup, err := lookupIdempotencyTx(ctx, tx, command.Intent.MerchantID, command.IdempotencyKey, command.Operation, command.RequestHash)
		if err != nil {
			return CreateIntentResult{}, err
		}
		if !lookup.InProgress {
			intent, err := getIntentTx(ctx, tx, command.Intent.MerchantID, lookup.ResourceID, false)
			if err != nil {
				return CreateIntentResult{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return CreateIntentResult{}, fmt.Errorf("commit idempotent replay: %w", err)
			}
			return CreateIntentResult{Intent: intent, Replay: &lookup.Response}, nil
		}
		return CreateIntentResult{}, fmt.Errorf("idempotency record unexpectedly in progress")
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO payment_intents (
			id, merchant_id, merchant_reference, amount_minor, currency,
			payment_method_type, payment_method_token, capture_method, status,
			integrity_status, authorized_amount_minor, captured_amount_minor,
			refunded_amount_minor, version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		command.Intent.ID, command.Intent.MerchantID, command.Intent.MerchantReference,
		command.Intent.AmountMinor, command.Intent.Currency, command.Intent.PaymentMethodType,
		command.Intent.PaymentMethodToken, command.Intent.CaptureMethod, command.Intent.Status,
		command.Intent.IntegrityStatus, command.Intent.AuthorizedAmountMinor,
		command.Intent.CapturedAmountMinor, command.Intent.RefundedAmountMinor,
		command.Intent.Version, command.Intent.CreatedAt, command.Intent.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return CreateIntentResult{}, ErrMerchantReferenceConflict
		}
		return CreateIntentResult{}, fmt.Errorf("insert payment intent: %w", err)
	}

	if err := completeIdempotencyTx(ctx, tx, command.Intent.MerchantID, command.IdempotencyKey, command.Response, command.Intent.UpdatedAt); err != nil {
		return CreateIntentResult{}, err
	}
	if err := insertOutbox(ctx, tx, "payment_intent", command.Intent.ID, command.Intent.Version, "payment_intent.created", map[string]any{
		"payment_intent_id": command.Intent.ID,
		"merchant_id":       command.Intent.MerchantID,
		"status":            command.Intent.Status,
	}, command.Intent.CreatedAt); err != nil {
		return CreateIntentResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CreateIntentResult{}, fmt.Errorf("commit create intent: %w", err)
	}
	return CreateIntentResult{Intent: command.Intent}, nil
}

func (s *Store) LookupIdempotency(ctx context.Context, merchantID, key, operation string, requestHash []byte) (IdempotencyLookup, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT operation, request_hash, state, COALESCE(resource_id,''),
		       COALESCE(response_status,0), COALESCE(response_body,''::bytea)
		FROM idempotency_records
		WHERE merchant_id=$1 AND idempotency_key=$2`, merchantID, key)
	return scanIdempotency(row, operation, requestHash)
}

type PrepareConfirmationCommand struct {
	MerchantID     string
	IdempotencyKey string
	OperationName  string
	RequestHash    []byte
	Attempt        domain.PaymentAttempt
	Operation      domain.ProviderOperation
	Now            time.Time
}

type PrepareConfirmationResult struct {
	Aggregate  Aggregate
	Replay     *StoredResponse
	InProgress bool
}

func (s *Store) PrepareConfirmation(ctx context.Context, command PrepareConfirmationCommand) (PrepareConfirmationResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PrepareConfirmationResult{}, fmt.Errorf("begin confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inserted, err := insertIdempotency(ctx, tx, command.MerchantID, command.IdempotencyKey, command.OperationName, command.RequestHash, command.Attempt.PaymentIntentID, command.Now)
	if err != nil {
		return PrepareConfirmationResult{}, err
	}
	if !inserted {
		lookup, err := lookupIdempotencyTx(ctx, tx, command.MerchantID, command.IdempotencyKey, command.OperationName, command.RequestHash)
		if err != nil {
			return PrepareConfirmationResult{}, err
		}
		intent, err := getIntentTx(ctx, tx, command.MerchantID, lookup.ResourceID, false)
		if err != nil {
			return PrepareConfirmationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return PrepareConfirmationResult{}, fmt.Errorf("commit confirmation replay: %w", err)
		}
		result := PrepareConfirmationResult{Aggregate: Aggregate{Intent: intent}, InProgress: lookup.InProgress}
		if !lookup.InProgress {
			result.Replay = &lookup.Response
		}
		return result, nil
	}

	intent, err := getIntentTx(ctx, tx, command.MerchantID, command.Attempt.PaymentIntentID, true)
	if err != nil {
		return PrepareConfirmationResult{}, err
	}
	attempt := command.Attempt
	operation := command.Operation
	if err := domain.PrepareConfirmation(&intent, &attempt, &operation, command.Now); err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			return PrepareConfirmationResult{}, fmt.Errorf("%w: %v", ErrStateConflict, err)
		}
		return PrepareConfirmationResult{}, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO payment_attempts (
			id, payment_intent_id, attempt_number, routing_decision_id,
			provider_connection_id, provider_connection_revision, provider_namespace_id, provider,
			status, outcome_code, reroute_disposition, provider_payment_reference,
			version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,$10,NULL,$11,$12,$13)`,
		attempt.ID, attempt.PaymentIntentID, attempt.AttemptNumber, attempt.RoutingDecisionID,
		attempt.ProviderConnectionID, attempt.ProviderConnectionRevision, attempt.ProviderNamespaceID,
		attempt.Provider, attempt.Status, attempt.RerouteDisposition, attempt.Version,
		attempt.CreatedAt, attempt.UpdatedAt,
	)
	if err != nil {
		return PrepareConfirmationResult{}, fmt.Errorf("insert payment attempt: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO provider_operations (
			id, payment_intent_id, payment_attempt_id, kind, amount_minor, currency,
			provider_connection_id, provider_connection_revision, provider_namespace_id, provider,
			provider_idempotency_key, merchant_reference, request_hash,
			status, side_effect_certainty, retry_mode, retry_count, version,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		operation.ID, operation.PaymentIntentID, operation.PaymentAttemptID, operation.Kind,
		operation.AmountMinor, operation.Currency, operation.ProviderConnectionID,
		operation.ProviderConnectionRevision, operation.ProviderNamespaceID, operation.Provider,
		operation.ProviderIdempotencyKey, operation.MerchantReference, operation.RequestHash,
		operation.Status, operation.SideEffectCertainty, operation.RetryMode,
		operation.RetryCount, operation.Version, operation.CreatedAt, operation.UpdatedAt,
	)
	if err != nil {
		return PrepareConfirmationResult{}, fmt.Errorf("insert provider operation: %w", err)
	}
	if err := updateIntentTx(ctx, tx, intent); err != nil {
		return PrepareConfirmationResult{}, err
	}
	if err := insertOutbox(ctx, tx, "payment_intent", intent.ID, intent.Version, "payment_intent.status_changed", map[string]any{
		"payment_intent_id": intent.ID,
		"attempt_id":        attempt.ID,
		"operation_id":      operation.ID,
		"status":            intent.Status,
		"integrity_status":  intent.IntegrityStatus,
	}, command.Now); err != nil {
		return PrepareConfirmationResult{}, err
	}
	if err := insertOutbox(ctx, tx, "provider_operation", operation.ID, operation.Version, "provider_operation.created", map[string]any{
		"operation_id":                 operation.ID,
		"payment_intent_id":            intent.ID,
		"payment_attempt_id":           attempt.ID,
		"provider_connection_id":       operation.ProviderConnectionID,
		"provider_connection_revision": operation.ProviderConnectionRevision,
		"provider_namespace_id":        operation.ProviderNamespaceID,
		"provider":                     operation.Provider,
	}, command.Now); err != nil {
		return PrepareConfirmationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PrepareConfirmationResult{}, fmt.Errorf("commit confirmation: %w", err)
	}
	return PrepareConfirmationResult{Aggregate: Aggregate{Intent: intent, Attempt: attempt, Operation: operation}}, nil
}

func (s *Store) MarkOperationSending(ctx context.Context, operationID string, now time.Time) (Aggregate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Aggregate{}, fmt.Errorf("begin sending transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	aggregate, err := getAggregateByOperationTx(ctx, tx, operationID, true)
	if err != nil {
		return Aggregate{}, err
	}
	if err := domain.MarkSending(&aggregate.Intent, &aggregate.Attempt, &aggregate.Operation, now); err != nil {
		return Aggregate{}, err
	}
	if err := updateAggregateTx(ctx, tx, aggregate); err != nil {
		return Aggregate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Aggregate{}, fmt.Errorf("commit sending transition: %w", err)
	}
	return aggregate, nil
}

type ApplyOutcomeResult struct {
	Aggregate Aggregate
	Apply     domain.ApplyResult
	Duplicate bool
	Rejected  bool
}

type ResponseBuilder func(domain.PaymentIntent) (StoredResponse, error)

func (s *Store) ApplyOutcome(ctx context.Context, outcome domain.ProviderOutcome, sanitizedPayload []byte, receivedAt time.Time, responseBuilder ResponseBuilder) (ApplyOutcomeResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyOutcomeResult{}, fmt.Errorf("begin provider outcome: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if len(sanitizedPayload) == 0 {
		sanitizedPayload = []byte(`{}`)
	}
	if outcome.EventID != "" {
		var occurredAt any
		if !outcome.OccurredAt.IsZero() {
			occurredAt = outcome.OccurredAt
		}
		commandTag, err := tx.Exec(ctx, `
			INSERT INTO provider_outcome_inbox (
				provider_namespace_id,provider_connection_id,provider_connection_revision,provider,event_id,
				operation_id,provider_idempotency_key,status,
				side_effect_certainty,normalized_code,provider_code,
				provider_payment_reference,provider_operation_reference,
				occurred_at,received_at,processing_status,sanitized_payload
			) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9,NULLIF($10,''),NULLIF($11,''),
			          NULLIF($12,''),NULLIF($13,''),$14,$15,'received',$16)
			ON CONFLICT (provider_namespace_id,event_id) DO NOTHING`,
			outcome.ProviderNamespaceID, outcome.ProviderConnectionID, outcome.ProviderConnectionRevision,
			outcome.Provider, outcome.EventID, outcome.OperationID, outcome.ProviderIdempotencyKey,
			outcome.Status, outcome.SideEffectCertainty, outcome.NormalizedCode, outcome.ProviderCode,
			outcome.ProviderPaymentReference, outcome.ProviderOperationReference, occurredAt, receivedAt, sanitizedPayload,
		)
		if err != nil {
			return ApplyOutcomeResult{}, fmt.Errorf("insert provider outcome inbox: %w", err)
		}
		if commandTag.RowsAffected() == 0 {
			stored, loadErr := getInboxCorrelationTx(ctx, tx, outcome.ProviderNamespaceID, outcome.EventID, sanitizedPayload)
			if loadErr != nil {
				return ApplyOutcomeResult{}, loadErr
			}
			storedOperationID, resolveErr := resolveOperationIDTx(
				ctx, tx, stored.ProviderConnectionID, stored.ProviderConnectionRevision,
				stored.ProviderNamespaceID, stored.OperationID, stored.ProviderIdempotencyKey,
			)
			var aggregate Aggregate
			if resolveErr == nil {
				aggregate, loadErr = getAggregateByOperationTx(ctx, tx, storedOperationID, false)
				if loadErr != nil {
					resolveErr = loadErr
				}
			}
			correlationConflict := resolveErr != nil || !stored.PayloadMatches || correlationMismatch(storedOperationID, aggregate, outcome)
			if correlationConflict {
				reason := "duplicate event_id was delivered with different correlation or payload"
				if err := quarantineOutcomeTx(ctx, tx, outcome, sanitizedPayload, receivedAt, "conflict", reason); err != nil {
					return ApplyOutcomeResult{}, err
				}
				if err := tx.Commit(ctx); err != nil {
					return ApplyOutcomeResult{}, err
				}
				return ApplyOutcomeResult{Aggregate: aggregate, Duplicate: true, Rejected: true}, nil
			}
			if stored.ProcessingStatus == "rejected" || stored.ProcessingStatus == "conflict" {
				if err := tx.Commit(ctx); err != nil {
					return ApplyOutcomeResult{}, err
				}
				return ApplyOutcomeResult{Aggregate: aggregate, Duplicate: true, Rejected: true}, nil
			}
			if err := completeIdempotencyForAggregateTx(ctx, tx, aggregate, responseBuilder, receivedAt); err != nil {
				return ApplyOutcomeResult{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return ApplyOutcomeResult{}, err
			}
			return ApplyOutcomeResult{Aggregate: aggregate, Duplicate: true}, nil
		}
	}

	operationID := outcome.OperationID
	if operationID == "" {
		operationID, err = resolveOperationIDTx(
			ctx, tx, outcome.ProviderConnectionID, outcome.ProviderConnectionRevision,
			outcome.ProviderNamespaceID, "", outcome.ProviderIdempotencyKey,
		)
		if errors.Is(err, ErrNotFound) {
			reason := "provider outcome does not correlate to a known operation"
			if quarantineErr := quarantineOutcomeTx(ctx, tx, outcome, sanitizedPayload, receivedAt, "rejected", reason); quarantineErr != nil {
				return ApplyOutcomeResult{}, quarantineErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return ApplyOutcomeResult{}, commitErr
			}
			return ApplyOutcomeResult{Rejected: true}, nil
		}
		if err != nil {
			return ApplyOutcomeResult{}, err
		}
		outcome.OperationID = operationID
	}
	if outcome.ProviderPaymentReference != "" {
		if err := lockProviderReferenceTx(ctx, tx, outcome.ProviderNamespaceID, outcome.ProviderPaymentReference); err != nil {
			return ApplyOutcomeResult{}, err
		}
	}

	aggregate, err := getAggregateByOperationTx(ctx, tx, operationID, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			reason := "provider outcome names an unknown operation"
			if quarantineErr := quarantineOutcomeTx(ctx, tx, outcome, sanitizedPayload, receivedAt, "rejected", reason); quarantineErr != nil {
				return ApplyOutcomeResult{}, quarantineErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return ApplyOutcomeResult{}, commitErr
			}
			return ApplyOutcomeResult{Rejected: true}, nil
		}
		return ApplyOutcomeResult{}, err
	}
	originalAggregate := aggregate
	if outcome.ProviderConnectionID != aggregate.Operation.ProviderConnectionID ||
		outcome.ProviderConnectionRevision != aggregate.Operation.ProviderConnectionRevision ||
		outcome.ProviderNamespaceID != aggregate.Operation.ProviderNamespaceID ||
		outcome.Provider != aggregate.Operation.Provider ||
		(outcome.ProviderIdempotencyKey != "" && outcome.ProviderIdempotencyKey != aggregate.Operation.ProviderIdempotencyKey) {
		reason := "provider outcome correlation does not match the durable operation"
		if err := quarantineOutcomeTx(ctx, tx, outcome, sanitizedPayload, receivedAt, "rejected", reason); err != nil {
			return ApplyOutcomeResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ApplyOutcomeResult{}, err
		}
		return ApplyOutcomeResult{Aggregate: originalAggregate, Rejected: true}, nil
	}
	if outcome.ProviderPaymentReference != "" {
		existingAttemptID, err := providerReferenceOwnerTx(ctx, tx, aggregate.Operation.ProviderNamespaceID, outcome.ProviderPaymentReference)
		if err != nil {
			return ApplyOutcomeResult{}, err
		}
		if existingAttemptID != "" && existingAttemptID != aggregate.Attempt.ID {
			reason := "provider payment reference is already assigned to another payment attempt"
			apply, reviewErr := requireAggregateReviewTx(ctx, tx, &aggregate, "provider_reference_collision", reason, receivedAt)
			if reviewErr != nil {
				return ApplyOutcomeResult{}, reviewErr
			}
			existingAggregate, loadErr := getAggregateByAttemptTx(ctx, tx, existingAttemptID, true)
			if loadErr != nil {
				return ApplyOutcomeResult{}, loadErr
			}
			if _, reviewErr := requireAggregateReviewTx(ctx, tx, &existingAggregate, "provider_reference_collision", reason, receivedAt); reviewErr != nil {
				return ApplyOutcomeResult{}, reviewErr
			}
			if err := quarantineOutcomeTx(ctx, tx, outcome, sanitizedPayload, receivedAt, "conflict", reason); err != nil {
				return ApplyOutcomeResult{}, err
			}
			if err := completeIdempotencyForAggregateTx(ctx, tx, aggregate, responseBuilder, receivedAt); err != nil {
				return ApplyOutcomeResult{}, err
			}
			if err := completeIdempotencyForAggregateTx(ctx, tx, existingAggregate, responseBuilder, receivedAt); err != nil {
				return ApplyOutcomeResult{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return ApplyOutcomeResult{}, err
			}
			return ApplyOutcomeResult{Aggregate: aggregate, Apply: apply, Rejected: true}, nil
		}
	}
	apply, transitionErr := domain.ApplyProviderOutcome(&aggregate.Intent, &aggregate.Attempt, &aggregate.Operation, outcome, receivedAt)
	if transitionErr != nil && !errors.Is(transitionErr, domain.ErrConflictingOutcome) {
		reason := transitionErr.Error()
		if err := quarantineOutcomeTx(ctx, tx, outcome, sanitizedPayload, receivedAt, "rejected", reason); err != nil {
			return ApplyOutcomeResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ApplyOutcomeResult{}, err
		}
		return ApplyOutcomeResult{Aggregate: originalAggregate, Rejected: true}, nil
	}
	if apply.Changed {
		if err := updateAggregateTx(ctx, tx, aggregate); err != nil {
			return ApplyOutcomeResult{}, err
		}
		if apply.IntentChanged {
			if err := insertOutbox(ctx, tx, "payment_intent", aggregate.Intent.ID, aggregate.Intent.Version, "payment_intent.status_changed", map[string]any{
				"payment_intent_id": aggregate.Intent.ID,
				"attempt_id":        aggregate.Attempt.ID,
				"operation_id":      aggregate.Operation.ID,
				"status":            aggregate.Intent.Status,
				"integrity_status":  aggregate.Intent.IntegrityStatus,
			}, receivedAt); err != nil {
				return ApplyOutcomeResult{}, err
			}
		}
	}
	if err := completeIdempotencyForAggregateTx(ctx, tx, aggregate, responseBuilder, receivedAt); err != nil {
		return ApplyOutcomeResult{}, err
	}
	if outcome.EventID != "" {
		processingStatus := "processed"
		if apply.Conflict {
			processingStatus = "conflict"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE provider_outcome_inbox
			SET processing_status=$3, processed_at=$4
			WHERE provider_namespace_id=$1 AND event_id=$2`, outcome.ProviderNamespaceID, outcome.EventID, processingStatus, receivedAt,
		); err != nil {
			return ApplyOutcomeResult{}, fmt.Errorf("complete provider inbox event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyOutcomeResult{}, fmt.Errorf("commit provider outcome: %w", err)
	}
	result := ApplyOutcomeResult{Aggregate: aggregate, Apply: apply}
	if errors.Is(transitionErr, domain.ErrConflictingOutcome) {
		return result, domain.ErrConflictingOutcome
	}
	return result, nil
}

type inboxCorrelation struct {
	ProviderConnectionID       string
	ProviderConnectionRevision string
	ProviderNamespaceID        string
	OperationID                string
	ProviderIdempotencyKey     string
	ProcessingStatus           string
	PayloadMatches             bool
}

func getInboxCorrelationTx(ctx context.Context, tx pgx.Tx, providerNamespaceID, eventID string, sanitizedPayload []byte) (inboxCorrelation, error) {
	var correlation inboxCorrelation
	err := tx.QueryRow(ctx, `
		SELECT provider_connection_id,provider_connection_revision,provider_namespace_id,
		       COALESCE(operation_id,''),COALESCE(provider_idempotency_key,''),processing_status,
		       sanitized_payload = $3::jsonb
		FROM provider_outcome_inbox
		WHERE provider_namespace_id=$1 AND event_id=$2
		FOR UPDATE`, providerNamespaceID, eventID, sanitizedPayload).Scan(
		&correlation.ProviderConnectionID, &correlation.ProviderConnectionRevision, &correlation.ProviderNamespaceID,
		&correlation.OperationID, &correlation.ProviderIdempotencyKey, &correlation.ProcessingStatus,
		&correlation.PayloadMatches,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return inboxCorrelation{}, ErrNotFound
	}
	if err != nil {
		return inboxCorrelation{}, fmt.Errorf("read existing provider inbox correlation: %w", err)
	}
	return correlation, nil
}

func resolveOperationIDTx(
	ctx context.Context,
	tx pgx.Tx,
	providerConnectionID, providerConnectionRevision, providerNamespaceID, operationID, providerIdempotencyKey string,
) (string, error) {
	if operationID != "" {
		return operationID, nil
	}
	if providerIdempotencyKey == "" {
		return "", ErrNotFound
	}
	err := tx.QueryRow(ctx, `
		SELECT id FROM provider_operations
		WHERE provider_connection_id=$1
		  AND provider_connection_revision=$2
		  AND provider_namespace_id=$3
		  AND provider_idempotency_key=$4`,
		providerConnectionID, providerConnectionRevision, providerNamespaceID, providerIdempotencyKey,
	).Scan(&operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve provider operation: %w", err)
	}
	return operationID, nil
}

func correlationMismatch(storedOperationID string, aggregate Aggregate, incoming domain.ProviderOutcome) bool {
	if incoming.OperationID != "" && storedOperationID != incoming.OperationID {
		return true
	}
	if incoming.ProviderConnectionID != aggregate.Operation.ProviderConnectionID ||
		incoming.ProviderConnectionRevision != aggregate.Operation.ProviderConnectionRevision ||
		incoming.ProviderNamespaceID != aggregate.Operation.ProviderNamespaceID ||
		incoming.Provider != aggregate.Operation.Provider {
		return true
	}
	return incoming.ProviderIdempotencyKey != "" &&
		incoming.ProviderIdempotencyKey != aggregate.Operation.ProviderIdempotencyKey
}

func quarantineOutcomeTx(ctx context.Context, tx pgx.Tx, outcome domain.ProviderOutcome, sanitizedPayload []byte, receivedAt time.Time, status, reason string) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	if _, err := tx.Exec(ctx, `
		UPDATE provider_outcome_inbox
		SET processing_status=$3,processed_at=$4,rejection_reason=$5
		WHERE provider_namespace_id=$1 AND event_id=$2`,
		outcome.ProviderNamespaceID, outcome.EventID, status, receivedAt, reason,
	); err != nil {
		return fmt.Errorf("quarantine provider inbox event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_outcome_quarantine (
			provider_namespace_id,provider_connection_id,provider_connection_revision,provider,event_id,
			operation_id,provider_idempotency_key,reason,sanitized_payload,received_at
		) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9,$10)`,
		outcome.ProviderNamespaceID, outcome.ProviderConnectionID, outcome.ProviderConnectionRevision,
		outcome.Provider, outcome.EventID, outcome.OperationID, outcome.ProviderIdempotencyKey,
		reason, sanitizedPayload, receivedAt,
	); err != nil {
		return fmt.Errorf("insert provider outcome quarantine record: %w", err)
	}
	return nil
}

func completeIdempotencyForAggregateTx(ctx context.Context, tx pgx.Tx, aggregate Aggregate, responseBuilder ResponseBuilder, now time.Time) error {
	if responseBuilder == nil || aggregate.Intent.ID == "" {
		return nil
	}
	response, err := responseBuilder(aggregate.Intent)
	if err != nil {
		return fmt.Errorf("build idempotent provider outcome response: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE idempotency_records
		SET state='completed',response_status=$2,response_body=$3,updated_at=$4
		WHERE resource_id=$1 AND state='in_progress'`,
		aggregate.Intent.ID, response.Status, response.Body, now,
	)
	if err != nil {
		return fmt.Errorf("complete provider outcome idempotency records: %w", err)
	}
	return nil
}

func lockProviderReferenceTx(ctx context.Context, tx pgx.Tx, providerNamespaceID, providerReference string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, providerReferenceLockID(providerNamespaceID, providerReference)); err != nil {
		return fmt.Errorf("lock provider payment reference: %w", err)
	}
	return nil
}

func providerReferenceLockID(providerNamespaceID, providerReference string) int64 {
	material := make([]byte, 16+len(providerNamespaceID)+len(providerReference))
	binary.BigEndian.PutUint64(material[0:8], uint64(len(providerNamespaceID)))
	copy(material[8:8+len(providerNamespaceID)], providerNamespaceID)
	referenceOffset := 8 + len(providerNamespaceID)
	binary.BigEndian.PutUint64(material[referenceOffset:referenceOffset+8], uint64(len(providerReference)))
	copy(material[referenceOffset+8:], providerReference)
	digest := sha256.Sum256(material)
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func providerReferenceOwnerTx(ctx context.Context, tx pgx.Tx, providerNamespaceID, providerReference string) (string, error) {
	var existingAttemptID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM payment_attempts
		WHERE provider_namespace_id=$1 AND provider_payment_reference=$2
		LIMIT 1`, providerNamespaceID, providerReference,
	).Scan(&existingAttemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check provider payment reference ownership: %w", err)
	}
	return existingAttemptID, nil
}

func requireAggregateReviewTx(ctx context.Context, tx pgx.Tx, aggregate *Aggregate, code, reason string, now time.Time) (domain.ApplyResult, error) {
	apply, err := domain.RequireManualReview(
		&aggregate.Intent,
		&aggregate.Attempt,
		&aggregate.Operation,
		code,
		reason,
		now,
	)
	if err != nil {
		return domain.ApplyResult{}, err
	}
	if apply.Changed {
		if err := updateAggregateTx(ctx, tx, *aggregate); err != nil {
			return domain.ApplyResult{}, err
		}
	}
	if apply.IntentChanged {
		if err := insertOutbox(ctx, tx, "payment_intent", aggregate.Intent.ID, aggregate.Intent.Version, "payment_intent.integrity_review_required", map[string]any{
			"payment_intent_id": aggregate.Intent.ID,
			"attempt_id":        aggregate.Attempt.ID,
			"operation_id":      aggregate.Operation.ID,
			"status":            aggregate.Intent.Status,
			"integrity_status":  aggregate.Intent.IntegrityStatus,
			"reason":            code,
		}, now); err != nil {
			return domain.ApplyResult{}, err
		}
	}
	return apply, nil
}

func (s *Store) CompleteIdempotency(ctx context.Context, merchantID, key string, response StoredResponse, now time.Time) error {
	commandTag, err := s.pool.Exec(ctx, `
		UPDATE idempotency_records
		SET state='completed',response_status=$3,response_body=$4,updated_at=$5
		WHERE merchant_id=$1 AND idempotency_key=$2`, merchantID, key, response.Status, response.Body, now)
	if err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetIntent(ctx context.Context, merchantID, intentID string) (domain.PaymentIntent, error) {
	return getIntentTx(ctx, s.pool, merchantID, intentID, false)
}

type RecoverableOperation struct {
	OperationID string
	Status      domain.OperationStatus
	UpdatedAt   time.Time
}

func (s *Store) ClaimRecoverableOperations(ctx context.Context, now time.Time, staleAge, lease time.Duration, limit int) ([]RecoverableOperation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin recoverable operation claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	staleBefore := now.Add(-staleAge)
	rows, err := tx.Query(ctx, `
		SELECT id,status,updated_at
		FROM provider_operations
		WHERE status IN ('created','sending','pending','unknown','retry_scheduled')
		  AND (
		    (next_retry_at IS NULL AND updated_at <= $1)
		    OR next_retry_at <= $2
		  )
		ORDER BY COALESCE(next_retry_at,updated_at) ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $3`, staleBefore, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select recoverable operations for claim: %w", err)
	}
	defer rows.Close()
	var operations []RecoverableOperation
	for rows.Next() {
		var operation RecoverableOperation
		if err := rows.Scan(&operation.OperationID, &operation.Status, &operation.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan recoverable operation: %w", err)
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	leaseUntil := now.Add(lease)
	for _, operation := range operations {
		if _, err := tx.Exec(ctx, `
			UPDATE provider_operations SET next_retry_at=$2 WHERE id=$1`,
			operation.OperationID, leaseUntil,
		); err != nil {
			return nil, fmt.Errorf("lease recoverable operation %s: %w", operation.OperationID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit recoverable operation claim: %w", err)
	}
	return operations, nil
}

func (s *Store) GetAggregateByOperation(ctx context.Context, operationID string) (Aggregate, error) {
	return getAggregateByOperationTx(ctx, s.pool, operationID, false)
}

func insertIdempotency(ctx context.Context, tx pgx.Tx, merchantID, key, operation string, requestHash []byte, resourceID string, now time.Time) (bool, error) {
	commandTag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_records (
			merchant_id,idempotency_key,operation,request_hash,state,resource_id,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'in_progress',$5,$6,$6)
		ON CONFLICT (merchant_id,idempotency_key) DO NOTHING`, merchantID, key, operation, requestHash, resourceID, now)
	if err != nil {
		return false, fmt.Errorf("insert idempotency record: %w", err)
	}
	return commandTag.RowsAffected() == 1, nil
}

func lookupIdempotencyTx(ctx context.Context, tx pgx.Tx, merchantID, key, operation string, requestHash []byte) (IdempotencyLookup, error) {
	row := tx.QueryRow(ctx, `
		SELECT operation,request_hash,state,COALESCE(resource_id,''),
		       COALESCE(response_status,0),COALESCE(response_body,''::bytea)
		FROM idempotency_records
		WHERE merchant_id=$1 AND idempotency_key=$2
		FOR UPDATE`, merchantID, key)
	return scanIdempotency(row, operation, requestHash)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanIdempotency(row rowScanner, operation string, requestHash []byte) (IdempotencyLookup, error) {
	var storedOperation, state, resourceID string
	var storedHash, body []byte
	var status int
	if err := row.Scan(&storedOperation, &storedHash, &state, &resourceID, &status, &body); errors.Is(err, pgx.ErrNoRows) {
		return IdempotencyLookup{}, nil
	} else if err != nil {
		return IdempotencyLookup{}, fmt.Errorf("read idempotency record: %w", err)
	}
	if storedOperation != operation || !idempotency.Equal(storedHash, requestHash) {
		return IdempotencyLookup{}, ErrIdempotencyConflict
	}
	return IdempotencyLookup{
		Found:      true,
		InProgress: state == "in_progress",
		ResourceID: resourceID,
		Response:   StoredResponse{Status: status, Body: body},
	}, nil
}

func completeIdempotencyTx(ctx context.Context, tx pgx.Tx, merchantID, key string, response StoredResponse, now time.Time) error {
	commandTag, err := tx.Exec(ctx, `
		UPDATE idempotency_records
		SET state='completed',response_status=$3,response_body=$4,updated_at=$5
		WHERE merchant_id=$1 AND idempotency_key=$2`, merchantID, key, response.Status, response.Body, now)
	if err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return errors.New("idempotency record disappeared")
	}
	return nil
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getIntentTx(ctx context.Context, query queryer, merchantID, intentID string, lock bool) (domain.PaymentIntent, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	row := query.QueryRow(ctx, `
		SELECT id,merchant_id,merchant_reference,amount_minor,currency,
		       payment_method_type,payment_method_token,capture_method,status,integrity_status,
		       authorized_amount_minor,captured_amount_minor,refunded_amount_minor,
		       COALESCE(active_attempt_id,''),COALESCE(latest_attempt_id,''),
		       COALESCE(winning_attempt_id,''),version,
		       created_at,updated_at
		FROM payment_intents
		WHERE merchant_id=$1 AND id=$2`+suffix, merchantID, intentID)
	intent, err := scanIntent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentIntent{}, ErrNotFound
	}
	return intent, err
}

func getIntentByIDTx(ctx context.Context, query queryer, intentID string, lock bool) (domain.PaymentIntent, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	row := query.QueryRow(ctx, `
		SELECT id,merchant_id,merchant_reference,amount_minor,currency,
		       payment_method_type,payment_method_token,capture_method,status,integrity_status,
		       authorized_amount_minor,captured_amount_minor,refunded_amount_minor,
		       COALESCE(active_attempt_id,''),COALESCE(latest_attempt_id,''),
		       COALESCE(winning_attempt_id,''),version,
		       created_at,updated_at
		FROM payment_intents WHERE id=$1`+suffix, intentID)
	intent, err := scanIntent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentIntent{}, ErrNotFound
	}
	return intent, err
}

func scanIntent(row rowScanner) (domain.PaymentIntent, error) {
	var intent domain.PaymentIntent
	err := row.Scan(
		&intent.ID, &intent.MerchantID, &intent.MerchantReference, &intent.AmountMinor,
		&intent.Currency, &intent.PaymentMethodType, &intent.PaymentMethodToken,
		&intent.CaptureMethod, &intent.Status, &intent.IntegrityStatus,
		&intent.AuthorizedAmountMinor, &intent.CapturedAmountMinor, &intent.RefundedAmountMinor,
		&intent.ActiveAttemptID, &intent.LatestAttemptID, &intent.WinningAttemptID, &intent.Version,
		&intent.CreatedAt, &intent.UpdatedAt,
	)
	if err != nil {
		return domain.PaymentIntent{}, err
	}
	return intent, nil
}

func getAggregateByOperationTx(ctx context.Context, query queryer, operationID string, lock bool) (Aggregate, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	operationRow := query.QueryRow(ctx, `
		SELECT id,payment_intent_id,payment_attempt_id,kind,amount_minor,currency,
		       provider_connection_id,provider_connection_revision,provider_namespace_id,provider,
		       provider_idempotency_key,merchant_reference,request_hash,status,
		       side_effect_certainty,retry_mode,COALESCE(provider_operation_reference,''),
		       COALESCE(normalized_code,''),COALESCE(provider_code,''),retry_count,
		       next_retry_at,COALESCE(last_error,''),version,created_at,updated_at,completed_at
		FROM provider_operations WHERE id=$1`+suffix, operationID)
	operation, err := scanOperation(operationRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return Aggregate{}, ErrNotFound
	}
	if err != nil {
		return Aggregate{}, fmt.Errorf("read provider operation: %w", err)
	}
	attemptRow := query.QueryRow(ctx, `
		SELECT id,payment_intent_id,attempt_number,routing_decision_id,
		       provider_connection_id,provider_connection_revision,provider_namespace_id,provider,status,
		       COALESCE(outcome_code,''),reroute_disposition,
		       COALESCE(provider_payment_reference,''),version,created_at,updated_at
		FROM payment_attempts WHERE id=$1`+suffix, operation.PaymentAttemptID)
	attempt, err := scanAttempt(attemptRow)
	if err != nil {
		return Aggregate{}, fmt.Errorf("read payment attempt: %w", err)
	}
	intent, err := getIntentByIDTx(ctx, query, operation.PaymentIntentID, lock)
	if err != nil {
		return Aggregate{}, err
	}
	return Aggregate{Intent: intent, Attempt: attempt, Operation: operation}, nil
}

func getAggregateByAttemptTx(ctx context.Context, query queryer, attemptID string, lock bool) (Aggregate, error) {
	var operationID string
	err := query.QueryRow(ctx, `SELECT id FROM provider_operations WHERE payment_attempt_id=$1`, attemptID).Scan(&operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Aggregate{}, ErrNotFound
	}
	if err != nil {
		return Aggregate{}, fmt.Errorf("resolve aggregate by payment attempt: %w", err)
	}
	return getAggregateByOperationTx(ctx, query, operationID, lock)
}

func scanAttempt(row rowScanner) (domain.PaymentAttempt, error) {
	var attempt domain.PaymentAttempt
	err := row.Scan(
		&attempt.ID, &attempt.PaymentIntentID, &attempt.AttemptNumber,
		&attempt.RoutingDecisionID, &attempt.ProviderConnectionID, &attempt.ProviderConnectionRevision,
		&attempt.ProviderNamespaceID, &attempt.Provider, &attempt.Status,
		&attempt.OutcomeCode, &attempt.RerouteDisposition, &attempt.ProviderPaymentReference,
		&attempt.Version, &attempt.CreatedAt, &attempt.UpdatedAt,
	)
	return attempt, err
}

func scanOperation(row rowScanner) (domain.ProviderOperation, error) {
	var operation domain.ProviderOperation
	err := row.Scan(
		&operation.ID, &operation.PaymentIntentID, &operation.PaymentAttemptID,
		&operation.Kind, &operation.AmountMinor, &operation.Currency, &operation.ProviderConnectionID,
		&operation.ProviderConnectionRevision, &operation.ProviderNamespaceID, &operation.Provider,
		&operation.ProviderIdempotencyKey, &operation.MerchantReference,
		&operation.RequestHash, &operation.Status, &operation.SideEffectCertainty,
		&operation.RetryMode, &operation.ProviderOperationReference,
		&operation.NormalizedCode, &operation.ProviderCode, &operation.RetryCount,
		&operation.NextRetryAt, &operation.LastError, &operation.Version,
		&operation.CreatedAt, &operation.UpdatedAt, &operation.CompletedAt,
	)
	return operation, err
}

func updateAggregateTx(ctx context.Context, tx pgx.Tx, aggregate Aggregate) error {
	if err := updateIntentTx(ctx, tx, aggregate.Intent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE payment_attempts
		SET status=$2,outcome_code=NULLIF($3,''),reroute_disposition=$4,
		    provider_payment_reference=NULLIF($5,''),version=$6,updated_at=$7
		WHERE id=$1`, aggregate.Attempt.ID, aggregate.Attempt.Status,
		aggregate.Attempt.OutcomeCode, aggregate.Attempt.RerouteDisposition,
		aggregate.Attempt.ProviderPaymentReference, aggregate.Attempt.Version,
		aggregate.Attempt.UpdatedAt,
	); err != nil {
		return fmt.Errorf("update payment attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE provider_operations
		SET status=$2,side_effect_certainty=$3,retry_mode=$4,
		    provider_operation_reference=NULLIF($5,''),normalized_code=NULLIF($6,''),
		    provider_code=NULLIF($7,''),retry_count=$8,next_retry_at=$9,
		    last_error=NULLIF($10,''),version=$11,updated_at=$12,completed_at=$13
		WHERE id=$1`, aggregate.Operation.ID, aggregate.Operation.Status,
		aggregate.Operation.SideEffectCertainty, aggregate.Operation.RetryMode,
		aggregate.Operation.ProviderOperationReference, aggregate.Operation.NormalizedCode,
		aggregate.Operation.ProviderCode, aggregate.Operation.RetryCount,
		aggregate.Operation.NextRetryAt, aggregate.Operation.LastError,
		aggregate.Operation.Version, aggregate.Operation.UpdatedAt, aggregate.Operation.CompletedAt,
	); err != nil {
		return fmt.Errorf("update provider operation: %w", err)
	}
	return nil
}

func updateIntentTx(ctx context.Context, tx pgx.Tx, intent domain.PaymentIntent) error {
	_, err := tx.Exec(ctx, `
		UPDATE payment_intents
		SET status=$2,integrity_status=$3,authorized_amount_minor=$4,
		    captured_amount_minor=$5,refunded_amount_minor=$6,
		    active_attempt_id=NULLIF($7,''),latest_attempt_id=NULLIF($8,''),
		    winning_attempt_id=NULLIF($9,''),version=$10,updated_at=$11
		WHERE id=$1`, intent.ID, intent.Status, intent.IntegrityStatus,
		intent.AuthorizedAmountMinor, intent.CapturedAmountMinor, intent.RefundedAmountMinor,
		intent.ActiveAttemptID, intent.LatestAttemptID, intent.WinningAttemptID,
		intent.Version, intent.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update payment intent: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID string, aggregateVersion int64, eventType string, payload any, now time.Time) error {
	eventID, err := identifier.New("evt_")
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (
			id,aggregate_type,aggregate_id,aggregate_version,event_type,
			payload,created_at,available_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
		ON CONFLICT (aggregate_type,aggregate_id,aggregate_version,event_type) DO NOTHING`,
		eventID, aggregateType, aggregateID, aggregateVersion, eventType, body, now)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
