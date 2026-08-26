//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplyOutcomeCompletesConfirmationIdempotencyAtomically(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	aggregate, merchantID, idempotencyKey := prepareIntegrationAggregate(t, ctx, store, "atomic", true)

	result, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    "evt_atomic_" + aggregate.Operation.ID,
		ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
		Provider:                   aggregate.Operation.Provider,
		OperationID:                aggregate.Operation.ID,
		ProviderIdempotencyKey:     aggregate.Operation.ProviderIdempotencyKey,
		Status:                     domain.OutcomeSucceeded,
		SideEffectCertainty:        domain.SideEffectConfirmed,
		ProviderPaymentReference:   "psp_payment_" + aggregate.Operation.ID,
		ProviderOperationReference: "psp_operation_" + aggregate.Operation.ID,
	}, []byte(`{"event":"success"}`), time.Now().UTC(), integrationResponseBuilder)
	if err != nil || result.Rejected || result.Aggregate.Intent.Status != domain.IntentSucceeded {
		t.Fatalf("apply success: result=%+v err=%v", result, err)
	}

	var state string
	var responseStatus int
	if err := pool.QueryRow(ctx, `
		SELECT state,response_status FROM idempotency_records
		WHERE merchant_id=$1 AND idempotency_key=$2`, merchantID, idempotencyKey,
	).Scan(&state, &responseStatus); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || responseStatus != 200 {
		t.Fatalf("idempotency completion was not committed with funds state: state=%s status=%d", state, responseStatus)
	}
}

func TestDuplicateInboxNamespaceCorrelationCannotCompleteIncomingPayment(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	first, _, _ := prepareIntegrationAggregate(t, ctx, store, "duplicate_a", true)
	second, secondMerchant, secondKey := prepareIntegrationAggregateWithConnection(t, ctx, store, "duplicate_b", true, "provider_integration_eu_backup")
	eventID := "evt_shared_" + first.Operation.ID

	if _, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    eventID,
		ProviderConnectionID:       first.Operation.ProviderConnectionID,
		ProviderConnectionRevision: first.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        first.Operation.ProviderNamespaceID,
		Provider:                   first.Operation.Provider,
		OperationID:                first.Operation.ID,
		ProviderIdempotencyKey:     first.Operation.ProviderIdempotencyKey,
		Status:                     domain.OutcomeSucceeded,
		SideEffectCertainty:        domain.SideEffectConfirmed,
		ProviderPaymentReference:   "psp_payment_" + first.Operation.ID,
		ProviderOperationReference: "psp_operation_" + first.Operation.ID,
	}, []byte(`{"event":"first"}`), time.Now().UTC(), integrationResponseBuilder); err != nil {
		t.Fatal(err)
	}

	duplicate, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    eventID,
		ProviderConnectionID:       second.Operation.ProviderConnectionID,
		ProviderConnectionRevision: second.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        second.Operation.ProviderNamespaceID,
		Provider:                   second.Operation.Provider,
		OperationID:                second.Operation.ID,
		ProviderIdempotencyKey:     second.Operation.ProviderIdempotencyKey,
		Status:                     domain.OutcomeSucceeded,
		SideEffectCertainty:        domain.SideEffectConfirmed,
		ProviderPaymentReference:   "psp_payment_" + second.Operation.ID,
	}, []byte(`{"event":"collision"}`), time.Now().UTC(), integrationResponseBuilder)
	if err != nil || !duplicate.Duplicate || !duplicate.Rejected {
		t.Fatalf("correlation collision: result=%+v err=%v", duplicate, err)
	}
	if duplicate.Aggregate.Intent.ID != first.Intent.ID {
		t.Fatalf("collision returned incoming aggregate %q, want original %q", duplicate.Aggregate.Intent.ID, first.Intent.ID)
	}

	var secondState string
	if err := pool.QueryRow(ctx, `
		SELECT state FROM idempotency_records
		WHERE merchant_id=$1 AND idempotency_key=$2`, secondMerchant, secondKey,
	).Scan(&secondState); err != nil {
		t.Fatal(err)
	}
	if secondState != "in_progress" {
		t.Fatalf("collision completed incoming payment idempotency: %s", secondState)
	}
	var quarantined int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_outcome_quarantine
		WHERE provider_namespace_id=$1 AND event_id=$2`, first.Operation.ProviderNamespaceID, eventID,
	).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined != 1 {
		t.Fatalf("correlation collision quarantine rows=%d, want 1", quarantined)
	}
}

func TestDuplicateInboxEventRejectsChangedPayloadWithSameCorrelation(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "duplicate_payload", true)
	eventID := "evt_payload_" + aggregate.Operation.ID
	outcome := domain.ProviderOutcome{
		EventID: eventID, ProviderConnectionID: aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision, ProviderNamespaceID: aggregate.Operation.ProviderNamespaceID,
		Provider: aggregate.Operation.Provider, OperationID: aggregate.Operation.ID,
		ProviderIdempotencyKey: aggregate.Operation.ProviderIdempotencyKey,
		Status:                 domain.OutcomeUnknown, SideEffectCertainty: domain.SideEffectPossible,
	}
	if _, err := store.ApplyOutcome(ctx, outcome, []byte(`{"event":"original"}`), time.Now().UTC(), integrationResponseBuilder); err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.ApplyOutcome(ctx, outcome, []byte(`{"event":"mutated"}`), time.Now().UTC().Add(time.Second), integrationResponseBuilder)
	if err != nil || !duplicate.Duplicate || !duplicate.Rejected {
		t.Fatalf("mutated duplicate: result=%+v err=%v", duplicate, err)
	}
	var quarantined int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_outcome_quarantine
		WHERE provider_namespace_id=$1 AND event_id=$2`, aggregate.Operation.ProviderNamespaceID, eventID,
	).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined != 1 {
		t.Fatalf("mutated duplicate quarantine rows=%d, want 1", quarantined)
	}
}

func TestRejectedOutcomeIsDurablyQuarantinedWithoutCompletingCommand(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	aggregate, merchantID, idempotencyKey := prepareIntegrationAggregate(t, ctx, store, "rejected", true)
	eventID := "evt_rejected_" + aggregate.Operation.ID

	result, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    eventID,
		ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
		Provider:                   "wrong_provider",
		OperationID:                aggregate.Operation.ID,
		ProviderIdempotencyKey:     aggregate.Operation.ProviderIdempotencyKey,
		Status:                     domain.OutcomeSucceeded,
		SideEffectCertainty:        domain.SideEffectConfirmed,
		ProviderPaymentReference:   "psp_wrong_" + aggregate.Operation.ID,
	}, []byte(`{"event":"wrong-provider"}`), time.Now().UTC(), integrationResponseBuilder)
	if err != nil || !result.Rejected || result.Duplicate {
		t.Fatalf("reject outcome: result=%+v err=%v", result, err)
	}

	var processingStatus, rejectionReason string
	if err := pool.QueryRow(ctx, `
		SELECT processing_status,rejection_reason FROM provider_outcome_inbox
		WHERE provider_namespace_id=$1 AND event_id=$2`, aggregate.Operation.ProviderNamespaceID, eventID,
	).Scan(&processingStatus, &rejectionReason); err != nil {
		t.Fatal(err)
	}
	if processingStatus != "rejected" || rejectionReason == "" {
		t.Fatalf("rejected event was not durable: status=%s reason=%q", processingStatus, rejectionReason)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM idempotency_records WHERE merchant_id=$1 AND idempotency_key=$2`, merchantID, idempotencyKey).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "in_progress" {
		t.Fatalf("rejected event completed payment command: %s", state)
	}
}

func TestProviderReferenceCollisionIsQuarantinedInsteadOfAbortingInbox(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	first, _, _ := prepareIntegrationAggregate(t, ctx, store, "reference_a", true)
	second, secondMerchant, secondKey := prepareIntegrationAggregate(t, ctx, store, "reference_b", true)
	providerReference := "psp_shared_" + first.Operation.ID

	if _, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    "evt_reference_a_" + first.Operation.ID,
		ProviderConnectionID:       first.Operation.ProviderConnectionID,
		ProviderConnectionRevision: first.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        first.Operation.ProviderNamespaceID,
		Provider:                   first.Operation.Provider,
		OperationID:                first.Operation.ID,
		Status:                     domain.OutcomeSucceeded,
		SideEffectCertainty:        domain.SideEffectConfirmed,
		ProviderPaymentReference:   providerReference,
		ProviderOperationReference: providerReference,
	}, []byte(`{"event":"reference-a"}`), time.Now().UTC(), integrationResponseBuilder); err != nil {
		t.Fatal(err)
	}
	eventID := "evt_reference_b_" + second.Operation.ID
	collision, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    eventID,
		ProviderConnectionID:       second.Operation.ProviderConnectionID,
		ProviderConnectionRevision: second.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        second.Operation.ProviderNamespaceID,
		Provider:                   second.Operation.Provider,
		OperationID:                second.Operation.ID,
		Status:                     domain.OutcomeSucceeded,
		SideEffectCertainty:        domain.SideEffectConfirmed,
		ProviderPaymentReference:   providerReference,
		ProviderOperationReference: providerReference,
	}, []byte(`{"event":"reference-b"}`), time.Now().UTC(), integrationResponseBuilder)
	if err != nil || !collision.Rejected {
		t.Fatalf("provider reference collision: result=%+v err=%v", collision, err)
	}
	var processingStatus, commandState string
	if err := pool.QueryRow(ctx, `
		SELECT processing_status FROM provider_outcome_inbox
		WHERE provider_namespace_id=$1 AND event_id=$2`, second.Operation.ProviderNamespaceID, eventID,
	).Scan(&processingStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT state FROM idempotency_records
		WHERE merchant_id=$1 AND idempotency_key=$2`, secondMerchant, secondKey,
	).Scan(&commandState); err != nil {
		t.Fatal(err)
	}
	if processingStatus != "conflict" || commandState != "completed" {
		t.Fatalf("collision was not safely quarantined: inbox=%s idempotency=%s", processingStatus, commandState)
	}
	if collision.Aggregate.Intent.IntegrityStatus != domain.IntegrityReviewRequired ||
		collision.Aggregate.Operation.Status != domain.OperationReviewRequired {
		t.Fatalf("collision did not stop automatic recovery for review: %+v", collision.Aggregate)
	}
	firstOwner, err := store.GetAggregateByOperation(ctx, first.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstOwner.Intent.IntegrityStatus != domain.IntegrityReviewRequired ||
		firstOwner.Intent.Status != domain.IntentSucceeded ||
		firstOwner.Operation.Status != domain.OperationSucceeded {
		t.Fatalf("existing reference owner was not reviewed while preserving funds facts: %+v", firstOwner)
	}
	recoverable, err := store.ClaimRecoverableOperations(ctx, time.Now().UTC().Add(24*time.Hour), time.Second, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if containsRecoverable(recoverable, second.Operation.ID) {
		t.Fatalf("manual-review operation remained automatically recoverable: %+v", recoverable)
	}
	var reviewEvents int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id=$1 AND event_type='payment_intent.integrity_review_required'`,
		second.Intent.ID,
	).Scan(&reviewEvents); err != nil {
		t.Fatal(err)
	}
	if reviewEvents != 1 {
		t.Fatalf("manual-review outbox events=%d, want 1", reviewEvents)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id=$1 AND event_type='payment_intent.integrity_review_required'`,
		first.Intent.ID,
	).Scan(&reviewEvents); err != nil {
		t.Fatal(err)
	}
	if reviewEvents != 1 {
		t.Fatalf("existing-owner review outbox events=%d, want 1", reviewEvents)
	}
}

func TestClaimRecoverableOperationsUsesDurableLease(t *testing.T) {
	store, _, ctx := integrationStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "lease", false)
	now := time.Now().UTC().Add(10 * time.Second)

	first, err := store.ClaimRecoverableOperations(ctx, now, time.Second, 30*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !containsRecoverable(first, aggregate.Operation.ID) {
		t.Fatalf("first claim did not include operation %s: %+v", aggregate.Operation.ID, first)
	}
	second, err := store.ClaimRecoverableOperations(ctx, now.Add(time.Second), time.Second, 30*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if containsRecoverable(second, aggregate.Operation.ID) {
		t.Fatalf("operation was claimed again before lease expiry: %+v", second)
	}
	afterLease, err := store.ClaimRecoverableOperations(ctx, now.Add(31*time.Second), time.Second, 30*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !containsRecoverable(afterLease, aggregate.Operation.ID) {
		t.Fatalf("operation was not recoverable after lease expiry: %+v", afterLease)
	}
}

func TestRepeatedUnknownDoesNotEmitFalsePaymentStatusEvents(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "unknown_outbox", true)
	var before int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_type='payment_intent' AND aggregate_id=$1 AND event_type='payment_intent.status_changed'`,
		aggregate.Intent.ID,
	).Scan(&before); err != nil {
		t.Fatal(err)
	}
	first, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID: "evt_unknown_first_" + aggregate.Operation.ID, ProviderConnectionID: aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision, ProviderNamespaceID: aggregate.Operation.ProviderNamespaceID,
		Provider:    aggregate.Operation.Provider,
		OperationID: aggregate.Operation.ID, Status: domain.OutcomeUnknown,
		SideEffectCertainty: domain.SideEffectPossible,
	}, []byte(`{"event":"unknown-first"}`), time.Now().UTC(), integrationResponseBuilder)
	if err != nil || first.Apply.IntentChanged {
		t.Fatalf("first UNKNOWN: result=%+v err=%v", first, err)
	}
	second, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID: "evt_unknown_second_" + aggregate.Operation.ID, ProviderConnectionID: aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision, ProviderNamespaceID: aggregate.Operation.ProviderNamespaceID,
		Provider:    aggregate.Operation.Provider,
		OperationID: aggregate.Operation.ID, Status: domain.OutcomeUnknown,
		SideEffectCertainty: domain.SideEffectPossible,
	}, []byte(`{"event":"unknown-second"}`), time.Now().UTC().Add(time.Second), integrationResponseBuilder)
	if err != nil || second.Apply.IntentChanged {
		t.Fatalf("second UNKNOWN: result=%+v err=%v", second, err)
	}
	var after int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_type='payment_intent' AND aggregate_id=$1 AND event_type='payment_intent.status_changed'`,
		aggregate.Intent.ID,
	).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("same-state UNKNOWN emitted payment status events: before=%d after=%d", before, after)
	}
	if second.Aggregate.Operation.NextRetryAt == nil || first.Aggregate.Operation.NextRetryAt == nil ||
		!second.Aggregate.Operation.NextRetryAt.After(*first.Aggregate.Operation.NextRetryAt) {
		t.Fatalf("UNKNOWN recovery backoff did not advance: first=%v second=%v", first.Aggregate.Operation.NextRetryAt, second.Aggregate.Operation.NextRetryAt)
	}
}

func TestProviderReferenceCollisionSpansAliasesInTheSameNamespace(t *testing.T) {
	store, _, ctx := integrationStore(t)
	first, _, _ := prepareIntegrationAggregateWithConnection(t, ctx, store, "connection_scope_a", true, "provider_integration_us_primary")
	second, _, _ := prepareIntegrationAggregateWithConnection(t, ctx, store, "connection_scope_b", true, "provider_integration_eu_backup")
	sharedReference := "provider_namespace_reference_" + first.Operation.ID

	for index, aggregate := range []Aggregate{first, second} {
		result, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
			EventID:                    fmt.Sprintf("evt_connection_scope_%d_%s", index, aggregate.Operation.ID),
			ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
			ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
			ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
			Provider:                   aggregate.Operation.Provider,
			OperationID:                aggregate.Operation.ID,
			ProviderIdempotencyKey:     aggregate.Operation.ProviderIdempotencyKey,
			Status:                     domain.OutcomeSucceeded,
			SideEffectCertainty:        domain.SideEffectConfirmed,
			ProviderPaymentReference:   sharedReference,
			ProviderOperationReference: sharedReference,
		}, []byte(fmt.Sprintf(`{"connection":%d}`, index)), time.Now().UTC(), integrationResponseBuilder)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && (result.Rejected || result.Aggregate.Intent.Status != domain.IntentSucceeded) {
			t.Fatalf("first namespace owner result=%+v", result)
		}
		if index == 1 && (!result.Rejected || result.Aggregate.Intent.IntegrityStatus != domain.IntegrityReviewRequired) {
			t.Fatalf("alias escaped namespace collision detection: %+v", result)
		}
	}
	firstOwner, err := store.GetAggregateByOperation(ctx, first.Operation.ID)
	if err != nil || firstOwner.Intent.IntegrityStatus != domain.IntegrityReviewRequired {
		t.Fatalf("original namespace owner was not placed in review: %+v err=%v", firstOwner, err)
	}
}

func TestProviderReferencesMayRepeatAcrossDistinctNamespaces(t *testing.T) {
	store, _, ctx := integrationStore(t)
	first, _, _ := prepareIntegrationAggregateWithIdentity(t, ctx, store, "namespace_a", true, "provider_us", "provider_account_us")
	second, _, _ := prepareIntegrationAggregateWithIdentity(t, ctx, store, "namespace_b", true, "provider_eu", "provider_account_eu")
	sharedReference := "account_local_reference_" + first.Operation.ID
	for index, aggregate := range []Aggregate{first, second} {
		result, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
			EventID:                    fmt.Sprintf("evt_namespace_%d_%s", index, aggregate.Operation.ID),
			ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
			ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
			ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
			Provider:                   aggregate.Operation.Provider,
			OperationID:                aggregate.Operation.ID,
			Status:                     domain.OutcomeSucceeded,
			SideEffectCertainty:        domain.SideEffectConfirmed,
			ProviderPaymentReference:   sharedReference,
			ProviderOperationReference: sharedReference,
		}, []byte(fmt.Sprintf(`{"namespace":%d}`, index)), time.Now().UTC(), integrationResponseBuilder)
		if err != nil || result.Rejected || result.Aggregate.Intent.Status != domain.IntentSucceeded {
			t.Fatalf("distinct namespace was falsely collided: result=%+v err=%v", result, err)
		}
	}
}

func TestDatabaseRejectsOperationExecutionIdentityDrift(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "identity_fk", false)
	_, err := pool.Exec(ctx, `
		UPDATE provider_operations SET provider_connection_revision='rebound_v2' WHERE id=$1`, aggregate.Operation.ID,
	)
	var constraintError *pgconn.PgError
	if !errors.As(err, &constraintError) || constraintError.Code != "23503" {
		t.Fatalf("identity drift error=%v, want foreign-key violation", err)
	}
	stored, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil || stored.Operation.ProviderConnectionRevision != aggregate.Operation.ProviderConnectionRevision {
		t.Fatalf("rejected identity drift changed the durable operation: %+v err=%v", stored, err)
	}
}

func integrationStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool), pool, ctx
}

func prepareIntegrationAggregate(t *testing.T, ctx context.Context, store *Store, label string, markSending bool) (Aggregate, string, string) {
	return prepareIntegrationAggregateWithConnection(t, ctx, store, label, markSending, "provider_integration_us_primary")
}

func prepareIntegrationAggregateWithConnection(t *testing.T, ctx context.Context, store *Store, label string, markSending bool, providerConnectionID string) (Aggregate, string, string) {
	return prepareIntegrationAggregateWithIdentity(t, ctx, store, label, markSending, providerConnectionID, "provider_integration")
}

func prepareIntegrationAggregateWithIdentity(t *testing.T, ctx context.Context, store *Store, label string, markSending bool, providerConnectionID, providerNamespaceID string) (Aggregate, string, string) {
	t.Helper()
	suffix := fmt.Sprintf("%s_%d", label, time.Now().UnixNano())
	now := time.Now().UTC()
	merchantID := "merchant_" + suffix
	intent, err := domain.NewIntent("pi_"+suffix, merchantID, "order_"+suffix, 2599, "USD", "pm_"+suffix, now)
	if err != nil {
		t.Fatal(err)
	}
	createBody, _ := json.Marshal(map[string]string{"id": intent.ID})
	if _, err := store.CreateIntent(ctx, CreateIntentCommand{
		Intent: intent, IdempotencyKey: "create_" + suffix, Operation: "create_payment_intent",
		RequestHash: []byte("create_hash_" + suffix), Response: StoredResponse{Status: 201, Body: createBody},
	}); err != nil {
		t.Fatal(err)
	}
	attempt := domain.PaymentAttempt{
		ID: "att_" + suffix, PaymentIntentID: intent.ID, AttemptNumber: 1,
		RoutingDecisionID: "route_" + suffix, ProviderConnectionID: providerConnectionID,
		ProviderConnectionRevision: providerConnectionID + "_v1", ProviderNamespaceID: providerNamespaceID,
		Provider: "provider_integration", CreatedAt: now, UpdatedAt: now,
	}
	operation := domain.ProviderOperation{
		ID: "op_" + suffix, PaymentIntentID: intent.ID, PaymentAttemptID: attempt.ID,
		Kind: domain.OperationSale, AmountMinor: intent.AmountMinor, Currency: intent.Currency,
		ProviderConnectionID: attempt.ProviderConnectionID, Provider: attempt.Provider, ProviderIdempotencyKey: "op_" + suffix,
		ProviderConnectionRevision: attempt.ProviderConnectionRevision, ProviderNamespaceID: attempt.ProviderNamespaceID,
		MerchantReference: "op_" + suffix, RequestHash: []byte("provider_hash_" + suffix),
		CreatedAt: now, UpdatedAt: now,
	}
	confirmationKey := "confirm_" + suffix
	prepared, err := store.PrepareConfirmation(ctx, PrepareConfirmationCommand{
		MerchantID: merchantID, IdempotencyKey: confirmationKey,
		OperationName: "confirm_payment_intent:" + intent.ID, RequestHash: []byte("confirm_hash_" + suffix),
		Attempt: attempt, Operation: operation, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregate := prepared.Aggregate
	if markSending {
		aggregate, err = store.MarkOperationSending(ctx, operation.ID, now.Add(time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
	}
	return aggregate, merchantID, confirmationKey
}

func integrationResponseBuilder(intent domain.PaymentIntent) (StoredResponse, error) {
	body, err := json.Marshal(map[string]any{"id": intent.ID, "status": intent.Status})
	status := 200
	if intent.Status == domain.IntentProcessing || intent.Status == domain.IntentRequiresAction {
		status = 202
	}
	return StoredResponse{Status: status, Body: body}, err
}

func containsRecoverable(operations []RecoverableOperation, operationID string) bool {
	for _, operation := range operations {
		if operation.OperationID == operationID {
			return true
		}
	}
	return false
}
