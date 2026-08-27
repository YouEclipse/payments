//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/recovery"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecoveryBudgetDeadlineAndIdentitySurviveWorkerRestart(t *testing.T) {
	store, pool, ctx := isolatedRecoveryStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "durable_budget", true)
	// PostgreSQL persists timestamptz values at microsecond precision. Establish
	// the restart baseline from a durable round trip instead of comparing a
	// nanosecond-precision write result with a later database read.
	durable, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := durable.Operation
	now := original.UpdatedAt.Add(10 * time.Second)

	for charged := 1; charged <= recovery.InitialBudget; charged++ {
		// A new Store has no process-local recovery state; the database remains
		// the authority after a worker restart.
		if charged == 9 {
			store = NewStore(pool)
		}
		claims, err := store.ClaimRecoverableOperations(ctx, now, 0, 4*time.Second, 1)
		if err != nil || len(claims) != 1 || claims[0].OperationID != original.ID || claims[0].Action != recovery.Query {
			t.Fatalf("charged=%d claims=%+v err=%v", charged, claims, err)
		}
		loaded, err := store.LoadRecoveryClaim(ctx, claims[0], now.Add(time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Operation.RecoveryIdentity() != original.RecoveryIdentity() ||
			!loaded.Operation.RecoveryDeadlineAt.Equal(original.RecoveryDeadlineAt) ||
			loaded.Operation.RecoveryBudgetRemaining != recovery.InitialBudget-charged ||
			loaded.Operation.FirstDispatchedAt == nil || !loaded.Operation.FirstDispatchedAt.Equal(*original.FirstDispatchedAt) {
			t.Fatalf("durable recovery identity reset: %+v", loaded.Operation)
		}
		// Model one crash after reservation. Its allowance is intentionally
		// lost; the next worker may claim only after lease/backoff expiry.
		if charged != 8 {
			if err := store.ReleaseRecoveryClaim(ctx, claims[0], now.Add(time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(5 * time.Minute)
	}

	claims, err := store.ClaimRecoverableOperations(ctx, now, 0, 4*time.Second, 1)
	if err != nil || len(claims) != 0 {
		t.Fatalf("exhausted operation produced funds I/O: claims=%+v err=%v", claims, err)
	}
	stopped, err := store.GetAggregateByOperation(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Intent.Status != domain.IntentProcessing || stopped.Intent.IntegrityStatus != domain.IntegrityReviewRequired ||
		stopped.Attempt.Status != domain.AttemptUnknown || stopped.Attempt.RerouteDisposition != domain.RerouteBlocked ||
		stopped.Operation.Status != domain.OperationReviewRequired || stopped.Operation.RecoveryBudgetRemaining != 0 ||
		stopped.Operation.RecoveryStopReason != "recovery_budget_exhausted" {
		t.Fatalf("budget exhaustion fabricated a terminal funds outcome: %+v", stopped)
	}
	var attempts, operations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE payment_intent_id=$1`, original.PaymentIntentID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_operations WHERE payment_intent_id=$1`, original.PaymentIntentID).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || operations != 1 {
		t.Fatalf("recovery created another funds path: attempts=%d operations=%d", attempts, operations)
	}
	for _, query := range []string{
		`UPDATE provider_operations SET recovery_budget_remaining=1 WHERE id=$1`,
		`UPDATE provider_operations SET recovery_deadline_at=recovery_deadline_at + interval '1 hour' WHERE id=$1`,
		`UPDATE provider_operations SET provider_idempotency_key=provider_idempotency_key || '_other' WHERE id=$1`,
		`UPDATE provider_operations SET recovery_action='query', recovery_lease_until=recovery_deadline_at + interval '1 second' WHERE id=$1`,
	} {
		_, updateErr := pool.Exec(ctx, query, original.ID)
		var constraint *pgconn.PgError
		if !errors.As(updateErr, &constraint) || constraint.Code != "23514" {
			t.Fatalf("recovery reset was accepted for %q: %v", query, updateErr)
		}
	}
}

func TestFreshAbsenceEvidenceCreatesOnlyASeparateSameKeyClaim(t *testing.T) {
	store, pool, ctx := isolatedRecoveryStore(t)
	aggregate, _, _ := prepareIntegrationAggregate(t, ctx, store, "same_key_evidence", true)
	now := aggregate.Operation.UpdatedAt.Add(10 * time.Second)
	queryClaims, err := store.ClaimRecoverableOperations(ctx, now, 0, 4*time.Second, 1)
	if err != nil || len(queryClaims) != 1 || queryClaims[0].Action != recovery.Query {
		t.Fatalf("query claim=%+v err=%v", queryClaims, err)
	}
	evidence := recovery.AbsenceEvidence{
		Kind: recovery.ProviderConclusiveAbsent, Identity: aggregate.Operation.RecoveryIdentity(),
		ObservedAt: now.Add(time.Millisecond), ValidUntil: now.Add(time.Minute),
	}
	wrong := evidence
	wrong.Identity.PayloadHash += "changed"
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], wrong, now.Add(2*time.Millisecond)); err == nil {
		t.Fatal("changed payload evidence was persisted")
	}
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], evidence, now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	store = NewStore(pool)
	resubmitClaims, err := store.ClaimRecoverableOperations(ctx, now.Add(3*time.Millisecond), 0, 4*time.Second, 1)
	if err != nil || len(resubmitClaims) != 1 || resubmitClaims[0].Action != recovery.SameKeyResubmit {
		t.Fatalf("same-key claim=%+v err=%v", resubmitClaims, err)
	}
	loaded, err := store.LoadRecoveryClaim(ctx, resubmitClaims[0], now.Add(4*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Operation.ID != aggregate.Operation.ID || loaded.Operation.PaymentAttemptID != aggregate.Attempt.ID ||
		loaded.Operation.ProviderConnectionID != aggregate.Operation.ProviderConnectionID ||
		loaded.Operation.ProviderIdempotencyKey != aggregate.Operation.ProviderIdempotencyKey ||
		loaded.Operation.RecoveryBudgetRemaining != recovery.InitialBudget-2 {
		t.Fatalf("resubmit changed the original operation: %+v", loaded.Operation)
	}
	var attemptCount, operationCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE payment_intent_id=$1`, aggregate.Intent.ID).Scan(&attemptCount)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_operations WHERE payment_intent_id=$1`, aggregate.Intent.ID).Scan(&operationCount)
	if attemptCount != 1 || operationCount != 1 {
		t.Fatalf("proof created another path: attempts=%d operations=%d", attemptCount, operationCount)
	}
}

func TestAlreadyDispatchedRecoveryDoesNotReinterpretExpiredRoutingAuthority(t *testing.T) {
	store, _, ctx := isolatedRecoveryStore(t)
	preparedAt := time.Now().UTC()
	prepared := prepareRecoveryWithRoutingEvidence(t, ctx, store, "expired_after_dispatch", preparedAt)
	// executable_until is exclusive. Dispatch well inside the authority window;
	// the later recovery assertions intentionally run after that window expires.
	dispatched, err := store.MarkOperationSending(ctx, prepared.Operation.ID, preparedAt.Add(time.Second))
	if err != nil || dispatched.Operation.FirstDispatchedAt == nil {
		t.Fatalf("fresh first dispatch was not recorded: aggregate=%+v err=%v", dispatched, err)
	}

	expiredAt := preparedAt.Add(6 * time.Minute)
	if err := validateRoutingEvidence(dispatched.Attempt, dispatched.Intent, expiredAt); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("fixture routing authority is not actually expired: %v", err)
	}
	queryClaims, err := store.ClaimRecoverableOperations(ctx, expiredAt, 0, 4*time.Second, 1)
	if err != nil || len(queryClaims) != 1 || queryClaims[0].Action != recovery.Query {
		t.Fatalf("expired authority blocked query of an already-dispatched operation: claims=%+v err=%v", queryClaims, err)
	}
	evidence := recovery.AbsenceEvidence{
		Kind: recovery.ProviderConclusiveAbsent, Identity: dispatched.Operation.RecoveryIdentity(),
		ObservedAt: expiredAt.Add(time.Millisecond), ValidUntil: expiredAt.Add(time.Minute),
	}
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], evidence, expiredAt.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	resubmitClaims, err := store.ClaimRecoverableOperations(ctx, expiredAt.Add(3*time.Millisecond), 0, 4*time.Second, 1)
	if err != nil || len(resubmitClaims) != 1 || resubmitClaims[0].Action != recovery.SameKeyResubmit {
		t.Fatalf("expired authority blocked proof-backed same-key recovery: claims=%+v err=%v", resubmitClaims, err)
	}
	loaded, err := store.LoadRecoveryClaim(ctx, resubmitClaims[0], expiredAt.Add(4*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Operation.Status == domain.OperationReviewRequired || loaded.Operation.FirstDispatchedAt == nil ||
		loaded.Operation.RecoveryIdentity() != dispatched.Operation.RecoveryIdentity() ||
		loaded.Operation.RecoveryBudgetRemaining != recovery.InitialBudget-2 {
		t.Fatalf("already-dispatched recovery was reinterpreted or changed identity: %+v", loaded.Operation)
	}
}

func TestLegacyUnknownWithoutDispatchMarkerCanUseProofAfterRoutingAuthorityExpires(t *testing.T) {
	store, _, ctx := isolatedRecoveryStore(t)
	preparedAt := time.Now().UTC()
	prepared := prepareRecoveryWithRoutingEvidence(t, ctx, store, "legacy_unknown_null_dispatch", preparedAt)

	// Pre-005 UNKNOWN rows deliberately retain a NULL first_dispatched_at: NULL
	// means historical observation unavailable, not "never sent". Model that
	// state without inventing a dispatch timestamp.
	result, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		OperationID: prepared.Operation.ID, ProviderIdempotencyKey: prepared.Operation.ProviderIdempotencyKey,
		ProviderConnectionID:       prepared.Operation.ProviderConnectionID,
		ProviderConnectionRevision: prepared.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        prepared.Operation.ProviderNamespaceID, Provider: prepared.Operation.Provider,
		Status: domain.OutcomeUnknown, SideEffectCertainty: domain.SideEffectPossible,
	}, []byte(`{"status":"unknown","legacy_first_dispatched_at":null}`), preparedAt.Add(time.Minute), nil)
	if err != nil || result.Aggregate.Operation.Status != domain.OperationUnknown || result.Aggregate.Operation.FirstDispatchedAt != nil {
		t.Fatalf("legacy UNKNOWN fixture invented dispatch history: result=%+v err=%v", result, err)
	}

	expiredAt := preparedAt.Add(6 * time.Minute)
	if err := validateRoutingEvidence(result.Aggregate.Attempt, result.Aggregate.Intent, expiredAt); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("fixture routing authority is not actually expired: %v", err)
	}
	queryClaims, err := store.ClaimRecoverableOperations(ctx, expiredAt, 0, 4*time.Second, 1)
	if err != nil || len(queryClaims) != 1 || queryClaims[0].Action != recovery.Query {
		t.Fatalf("legacy UNKNOWN did not query first: claims=%+v err=%v", queryClaims, err)
	}
	evidence := recovery.AbsenceEvidence{
		Kind: recovery.ProviderConclusiveAbsent, Identity: result.Aggregate.Operation.RecoveryIdentity(),
		ObservedAt: expiredAt.Add(time.Millisecond), ValidUntil: expiredAt.Add(time.Minute),
	}
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], evidence, expiredAt.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	resubmitClaims, err := store.ClaimRecoverableOperations(ctx, expiredAt.Add(3*time.Millisecond), 0, 4*time.Second, 1)
	if err != nil || len(resubmitClaims) != 1 || resubmitClaims[0].Action != recovery.SameKeyResubmit {
		t.Fatalf("legacy already-possible operation was mistaken for first dispatch: claims=%+v err=%v", resubmitClaims, err)
	}
	loaded, err := store.LoadRecoveryClaim(ctx, resubmitClaims[0], expiredAt.Add(4*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Operation.FirstDispatchedAt != nil || loaded.Operation.Status == domain.OperationReviewRequired ||
		loaded.Operation.RecoveryIdentity() != prepared.Operation.RecoveryIdentity() ||
		loaded.Operation.RecoveryBudgetRemaining != recovery.InitialBudget-2 {
		t.Fatalf("legacy recovery changed identity or invented dispatch time: %+v", loaded.Operation)
	}
}

func prepareRecoveryWithRoutingEvidence(t *testing.T, ctx context.Context, store *Store, label string, now time.Time) Aggregate {
	t.Helper()
	suffix := fmt.Sprintf("%s_%d", label, now.UnixNano())
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
		ProviderConnectionRevision: attempt.ProviderConnectionRevision, ProviderNamespaceID: attempt.ProviderNamespaceID,
		Provider: attempt.Provider, ProviderIdempotencyKey: "op_" + suffix, MerchantReference: "op_" + suffix,
		RequestHash: []byte("provider_hash_" + suffix), CreatedAt: now, UpdatedAt: now,
	}
	prepared, err := store.PrepareConfirmation(ctx, PrepareConfirmationCommand{
		MerchantID: intent.MerchantID, IdempotencyKey: "confirm_" + suffix,
		OperationName: "confirm_payment_intent:" + intent.ID, RequestHash: []byte("confirm_hash_" + suffix),
		Attempt: attempt, Operation: operation, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared.Aggregate
}

func isolatedRecoveryStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("payment_recovery_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+identifier+" CASCADE")
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool), pool, ctx
}
