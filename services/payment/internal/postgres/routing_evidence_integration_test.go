//go:build integration

package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRoutingEvidencePersistsBeforeDispatchAndCannotBeRewritten(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	now := time.Now().UTC()
	suffix := fmt.Sprintf("snapshot_%d", now.UnixNano())
	intent, attempt := routingEvidenceFixture(t, suffix, now)
	if _, err := store.CreateIntent(ctx, CreateIntentCommand{
		Intent: intent, IdempotencyKey: "create_" + suffix, Operation: "create_payment_intent",
		RequestHash: []byte("create_hash_" + suffix), Response: StoredResponse{Status: 201, Body: []byte(`{"created":true}`)},
	}); err != nil {
		t.Fatal(err)
	}
	operation := domain.ProviderOperation{
		ID: "op_" + suffix, PaymentIntentID: intent.ID, PaymentAttemptID: attempt.ID, Kind: domain.OperationSale,
		AmountMinor: intent.AmountMinor, Currency: intent.Currency, ProviderConnectionID: attempt.ProviderConnectionID,
		ProviderConnectionRevision: attempt.ProviderConnectionRevision, ProviderNamespaceID: attempt.ProviderNamespaceID, Provider: attempt.Provider,
		ProviderIdempotencyKey: "op_" + suffix, MerchantReference: "op_" + suffix, RequestHash: []byte("provider_hash_" + suffix), CreatedAt: now, UpdatedAt: now,
	}
	command := PrepareConfirmationCommand{MerchantID: intent.MerchantID, IdempotencyKey: "confirm_" + suffix,
		OperationName: "confirm_payment_intent:" + intent.ID, RequestHash: []byte("confirm_hash_" + suffix), Attempt: attempt, Operation: operation, Now: now}
	bad := command
	bad.Attempt.RoutingContextSnapshot = strings.Replace(attempt.RoutingContextSnapshot, `"amount_minor":2599`, `"amount_minor":2600`, 1)
	if _, err := store.PrepareConfirmation(ctx, bad); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("accepted tampered pre-dispatch context: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM provider_operations WHERE payment_intent_id=$1", intent.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed prepare wrote a provider operation: count=%d err=%v", count, err)
	}
	if _, err := store.PrepareConfirmation(ctx, command); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetAggregateByOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Operation.Status != domain.OperationCreated {
		t.Fatalf("unexpected dispatch before verification: %s", loaded.Operation.Status)
	}
	if err := validateRoutingEvidence(loaded.Attempt, loaded.Intent); err != nil {
		t.Fatal(err)
	}
	var original, restored routingcontext.Context
	if err := json.Unmarshal([]byte(attempt.RoutingContextSnapshot), &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(loaded.Attempt.RoutingContextSnapshot), &restored); err != nil {
		t.Fatal(err)
	}
	wantDigest, err := original.Digest()
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, err := restored.Digest()
	if err != nil || gotDigest != wantDigest {
		t.Fatalf("JSONB round trip changed canonical digest: %s != %s, %v", gotDigest, wantDigest, err)
	}
	if strings.Contains(loaded.Attempt.RoutingContextSnapshot+loaded.Attempt.RoutingDecisionSnapshot, intent.PaymentMethodToken) {
		t.Fatal("stored credential in routing evidence")
	}
	for _, query := range []string{
		`UPDATE payment_attempts SET routing_context_snapshot=jsonb_set(routing_context_snapshot,'{base,amount_minor}','9999') WHERE id=$1`,
		`UPDATE payment_attempts SET routing_decision_snapshot=jsonb_set(routing_decision_snapshot,'{reason_codes}','["rewritten"]') WHERE id=$1`,
		`UPDATE payment_attempts SET routing_context_snapshot=NULL,routing_decision_snapshot=NULL WHERE id=$1`,
	} {
		_, err := pool.Exec(ctx, query, attempt.ID)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatalf("immutable evidence constraint did not reject rewrite: %v", err)
		}
	}
	// Ordinary state transitions leave the original context and decision intact.
	sending, err := store.MarkOperationSending(ctx, operation.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if sending.Attempt.RoutingContextSnapshot != loaded.Attempt.RoutingContextSnapshot || sending.Attempt.RoutingDecisionSnapshot != loaded.Attempt.RoutingDecisionSnapshot {
		t.Fatal("dispatch rewrote frozen routing evidence")
	}
}

func TestLegacyAttemptKeepsNullEvidenceAcrossMigrationAndRecovery(t *testing.T) {
	store, pool, ctx := integrationStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "legacy_snapshot", true)
	var absent bool
	if err := pool.QueryRow(ctx, `SELECT routing_context_snapshot IS NULL AND routing_decision_snapshot IS NULL FROM payment_attempts WHERE id=$1`, aggregate.Attempt.ID).Scan(&absent); err != nil || !absent {
		t.Fatalf("legacy history was fabricated: absent=%v err=%v", absent, err)
	}
	loaded, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil || loaded.Attempt.RoutingContextSnapshot != "" || loaded.Attempt.RoutingDecisionSnapshot != "" {
		t.Fatalf("legacy snapshot reading failed: %+v %v", loaded.Attempt, err)
	}
	// Backfill cannot claim today's configuration was the old decision input.
	_, err = pool.Exec(ctx, `UPDATE payment_attempts SET routing_context_snapshot='{}',routing_decision_snapshot='{}' WHERE id=$1`, aggregate.Attempt.ID)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		t.Fatalf("fabricated legacy backfill was not rejected: %v", err)
	}
}
