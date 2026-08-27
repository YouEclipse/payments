//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/YouEclipse/payments/services/payment/internal/postgres"
	"github.com/YouEclipse/payments/services/payment/internal/recovery"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func TestExpiredRoutingAuthorityStopsNeverDispatchedRecoveryBeforeGateway(t *testing.T) {
	store, _, ctx := isolatedAppStore(t)
	preparedAt := time.Now().UTC()
	aggregate := prepareVersionedRecoveryOperation(t, ctx, store, preparedAt)
	expiredAt := preparedAt.Add(6 * time.Minute)

	// Model a crash after the atomic prepare and before the synchronous send.
	// The durable worker must consume neither recovery budget nor Gateway I/O.
	claims, err := store.ClaimRecoverableOperations(ctx, expiredAt, 0, 4*time.Second, 1)
	if err != nil || len(claims) != 0 {
		t.Fatalf("expired first-dispatch authority produced an executable claim: claims=%+v err=%v", claims, err)
	}
	gateway := &forbiddenRecoveryGateway{}
	service := NewService(store, &concurrentRouter{}, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	service.now = func() time.Time { return expiredAt }
	service.reconcileBatch(ctx, 0)
	if gateway.executes.Load() != 0 || gateway.queries.Load() != 0 {
		t.Fatalf("review-only operation reached Gateway: executes=%d queries=%d", gateway.executes.Load(), gateway.queries.Load())
	}

	stopped, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Intent.Status != domain.IntentProcessing || stopped.Intent.IntegrityStatus != domain.IntegrityReviewRequired ||
		stopped.Attempt.Status != domain.AttemptUnknown || stopped.Attempt.RerouteDisposition != domain.RerouteBlocked ||
		stopped.Operation.Status != domain.OperationReviewRequired || stopped.Operation.FirstDispatchedAt != nil ||
		stopped.Operation.RecoveryBudgetRemaining != recovery.InitialBudget ||
		stopped.Operation.RecoveryStopReason != "routing_authority_expired_before_first_dispatch" {
		t.Fatalf("expired first-dispatch authority did not stop as unresolved review: %+v", stopped)
	}
}

func TestRoutingAuthorityExpiryAfterClaimStopsBeforeFirstGatewayIO(t *testing.T) {
	store, _, ctx := isolatedAppStore(t)
	preparedAt := time.Now().UTC()
	aggregate := prepareVersionedRecoveryOperation(t, ctx, store, preparedAt)
	executableUntil := *aggregate.Attempt.RoutingExecutableUntil
	claimAt := executableUntil.Add(-10 * time.Millisecond)

	// The claim atomically records the dispatch marker and debits one call, but
	// its executable deadline is still the frozen Router authority boundary.
	claims, err := store.ClaimRecoverableOperations(ctx, claimAt, 0, 4*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("near-expiry first-dispatch claim=%+v err=%v", claims, err)
	}
	claim := claims[0]
	if claim.Action != recovery.SameKeyResubmit || claim.FirstDispatchExecutableUntil == nil ||
		!claim.CallDeadline.Equal(executableUntil) {
		t.Fatalf("first-dispatch claim did not inherit routing boundary: %+v", claim)
	}

	gateway := &forbiddenRecoveryGateway{}
	service := NewService(store, &concurrentRouter{}, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	service.now = func() time.Time { return executableUntil }
	if err := service.executeRecovery(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if gateway.executes.Load() != 0 || gateway.queries.Load() != 0 {
		t.Fatalf("expired claimed operation reached Gateway: executes=%d queries=%d", gateway.executes.Load(), gateway.queries.Load())
	}

	stopped, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Operation.Status != domain.OperationReviewRequired || stopped.Operation.FirstDispatchedAt == nil ||
		stopped.Operation.RecoveryBudgetRemaining != recovery.InitialBudget-1 ||
		stopped.Operation.RecoveryStopReason != "routing_authority_expired_before_first_gateway_io" {
		t.Fatalf("claimed-but-not-called operation was not quarantined: %+v", stopped.Operation)
	}
}

func TestTerminalEvidenceWinsRaceWithRoutingAuthorityReviewCAS(t *testing.T) {
	store, _, ctx := isolatedAppStore(t)
	preparedAt := time.Now().UTC()
	aggregate := prepareVersionedRecoveryOperation(t, ctx, store, preparedAt)
	executableUntil := *aggregate.Attempt.RoutingExecutableUntil
	claims, err := store.ClaimRecoverableOperations(ctx, executableUntil.Add(-100*time.Millisecond), 0, 4*time.Second, 1)
	if err != nil || len(claims) != 1 || claims[0].Basis != recovery.ClaimFirstGatewayIO {
		t.Fatalf("first-I/O claim=%+v err=%v", claims, err)
	}
	terminalAt := executableUntil.Add(-10 * time.Millisecond)
	result, err := store.ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID: "evt_terminal_before_authority_review", OperationID: aggregate.Operation.ID,
		ProviderIdempotencyKey:     aggregate.Operation.ProviderIdempotencyKey,
		ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID, Provider: aggregate.Operation.Provider,
		Status: domain.OutcomeSucceeded, SideEffectCertainty: domain.SideEffectConfirmed,
		ProviderPaymentReference:   "provider_terminal_" + aggregate.Operation.ID,
		ProviderOperationReference: "provider_operation_terminal_" + aggregate.Operation.ID,
	}, []byte(`{"status":"succeeded"}`), terminalAt, nil)
	if err != nil || result.Aggregate.Operation.Status != domain.OperationSucceeded {
		t.Fatalf("terminal evidence result=%+v err=%v", result, err)
	}
	if _, err := store.StopRecoveryForReview(ctx, claims[0], executableUntil); !errors.Is(err, postgres.ErrRecoveryClaimLost) {
		t.Fatalf("stale authority review CAS overrode terminal evidence: %v", err)
	}
	current, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil || current.Operation.Status != domain.OperationSucceeded || current.Intent.IntegrityStatus != domain.IntegrityHealthy {
		t.Fatalf("terminal evidence was not sticky: aggregate=%+v err=%v", current, err)
	}
}

func TestSynchronousRoutingAuthorityExpiryStopsBeforeFirstGatewayIO(t *testing.T) {
	store, pool, ctx := isolatedAppStore(t)
	base := time.Now().UTC().Truncate(time.Millisecond)
	builder, err := routingcontext.NewBuilder([]routingcontext.MerchantProfile{{
		MerchantID: "merchant_sync_expiry", EntityID: "entity_us", Country: "US", Version: "merchant_config.v1",
		ObservedAt: base.Add(-time.Hour), ExpiresAt: base.Add(time.Hour),
	}}, routingcontext.SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &forbiddenRecoveryGateway{}
	service := NewService(
		store, &versionedRecoveryRouter{authorityWindow: time.Second}, gateway,
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, WithRoutingContext(builder),
	)
	var clockCalls atomic.Int32
	service.now = func() time.Time {
		// Create, context build, prepare, MarkSending and the first deadline
		// calculation all occur inside authority. The immediate pre-Execute
		// recheck observes the half-open boundary.
		if clockCalls.Add(1) <= 5 {
			return base
		}
		return base.Add(time.Second)
	}
	created, err := service.CreateIntent(ctx, "merchant_sync_expiry", "create_sync_expiry", CreateIntentInput{
		MerchantReference: "order_sync_expiry", AmountMinor: 2599, Currency: "USD",
		PaymentMethodToken: "pm_sim_approved", CaptureMethod: string(domain.CaptureAutomatic),
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := service.ConfirmIntent(ctx, "merchant_sync_expiry", created.Intent.ID, "confirm_sync_expiry")
	if err != nil {
		t.Fatal(err)
	}
	if gateway.executes.Load() != 0 || gateway.queries.Load() != 0 {
		t.Fatalf("expired synchronous authority reached Gateway: executes=%d queries=%d", gateway.executes.Load(), gateway.queries.Load())
	}
	if confirmed.Intent.Status != domain.IntentProcessing || confirmed.Intent.IntegrityStatus != domain.IntegrityReviewRequired {
		t.Fatalf("expired synchronous result did not remain unresolved review: %+v", confirmed)
	}
	var operationID string
	if err := pool.QueryRow(ctx, `SELECT id FROM provider_operations WHERE payment_intent_id=$1`, created.Intent.ID).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.GetAggregateByOperation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Operation.Status != domain.OperationReviewRequired || stopped.Operation.FirstDispatchedAt == nil ||
		stopped.Operation.RecoveryStopReason != "routing_authority_expired_before_first_gateway_io" {
		t.Fatalf("synchronous marker/authority ambiguity was not quarantined: %+v", stopped.Operation)
	}
}

func TestExpiredRoutingAuthorityDoesNotBlockAlreadyDispatchedSameKeyRecovery(t *testing.T) {
	store, _, ctx := isolatedAppStore(t)
	preparedAt := time.Now().UTC()
	aggregate := prepareVersionedRecoveryOperation(t, ctx, store, preparedAt)
	if _, err := store.MarkOperationSending(ctx, aggregate.Operation.ID, preparedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	expiredAt := aggregate.Attempt.RoutingExecutableUntil.Add(time.Second)

	queryClaims, err := store.ClaimRecoverableOperations(ctx, expiredAt, 0, 4*time.Second, 1)
	if err != nil || len(queryClaims) != 1 || queryClaims[0].Action != recovery.Query {
		t.Fatalf("already-dispatched query claim=%+v err=%v", queryClaims, err)
	}
	if queryClaims[0].FirstDispatchExecutableUntil != nil {
		t.Fatalf("already-dispatched query was incorrectly fenced by old routing deadline: %+v", queryClaims[0])
	}
	evidenceAt := expiredAt.Add(time.Millisecond)
	evidence := recovery.AbsenceEvidence{
		Kind: recovery.ProviderConclusiveAbsent, Identity: queryClaims[0].Identity,
		ObservedAt: evidenceAt, ValidUntil: evidenceAt.Add(5 * time.Second),
	}
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], evidence, evidenceAt); err != nil {
		t.Fatal(err)
	}
	resubmitAt := evidenceAt.Add(time.Millisecond)
	resubmitClaims, err := store.ClaimRecoverableOperations(ctx, resubmitAt, 0, 4*time.Second, 1)
	if err != nil || len(resubmitClaims) != 1 || resubmitClaims[0].Action != recovery.SameKeyResubmit {
		t.Fatalf("already-dispatched resubmit claim=%+v err=%v", resubmitClaims, err)
	}
	if resubmitClaims[0].FirstDispatchExecutableUntil != nil {
		t.Fatalf("already-dispatched resubmit was incorrectly fenced by old routing deadline: %+v", resubmitClaims[0])
	}
	if resubmitClaims[0].Basis != recovery.ClaimProviderRecovery {
		t.Fatalf("provider-proven recovery lost its durable claim basis: %+v", resubmitClaims[0])
	}
	forged := resubmitClaims[0]
	forged.Basis = recovery.ClaimFirstGatewayIO
	forged.FirstDispatchExecutableUntil = aggregate.Attempt.RoutingExecutableUntil
	if _, err := store.StopRecoveryForReview(ctx, forged, resubmitAt); !errors.Is(err, postgres.ErrRecoveryClaimLost) {
		t.Fatalf("forged first-I/O claim changed provider recovery: %v", err)
	}
	gateway := &countingGateway{}
	service := NewService(store, &concurrentRouter{}, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	service.now = func() time.Time { return resubmitAt.Add(time.Millisecond) }
	if err := service.executeRecovery(ctx, resubmitClaims[0]); err != nil {
		t.Fatal(err)
	}
	if gateway.executes.Load() != 1 {
		t.Fatalf("same-key recovery execute count=%d, want 1", gateway.executes.Load())
	}
	recovered, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil || recovered.Operation.Status != domain.OperationSucceeded || recovered.Attempt.ID != aggregate.Attempt.ID ||
		recovered.Operation.ProviderIdempotencyKey != aggregate.Operation.ProviderIdempotencyKey {
		t.Fatalf("already-dispatched recovery did not preserve the funds path: aggregate=%+v err=%v", recovered, err)
	}
}

func TestGatewayNeverDispatchedProofCannotBypassExpiredRoutingAuthority(t *testing.T) {
	store, _, ctx := isolatedAppStore(t)
	preparedAt := time.Now().UTC()
	aggregate := prepareVersionedRecoveryOperation(t, ctx, store, preparedAt)
	if _, err := store.MarkOperationSending(ctx, aggregate.Operation.ID, preparedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	expiredAt := aggregate.Attempt.RoutingExecutableUntil.Add(time.Second)
	queryClaims, err := store.ClaimRecoverableOperations(ctx, expiredAt, 0, 4*time.Second, 1)
	if err != nil || len(queryClaims) != 1 || queryClaims[0].Action != recovery.Query {
		t.Fatalf("post-crash query claim=%+v err=%v", queryClaims, err)
	}
	evidenceAt := expiredAt.Add(time.Millisecond)
	evidence := recovery.AbsenceEvidence{
		Kind: recovery.GatewayNeverDispatched, Identity: queryClaims[0].Identity,
		ObservedAt: evidenceAt, ValidUntil: evidenceAt.Add(5 * time.Second),
	}
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], evidence, evidenceAt); err != nil {
		t.Fatal(err)
	}

	claims, err := store.ClaimRecoverableOperations(ctx, evidenceAt.Add(time.Millisecond), 0, 4*time.Second, 1)
	if err != nil || len(claims) != 0 {
		t.Fatalf("Gateway no-dispatch proof bypassed expired routing authority: claims=%+v err=%v", claims, err)
	}
	gateway := &forbiddenRecoveryGateway{}
	service := NewService(store, &concurrentRouter{}, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	service.now = func() time.Time { return evidenceAt.Add(2 * time.Millisecond) }
	service.reconcileBatch(ctx, 0)
	if gateway.executes.Load() != 0 || gateway.queries.Load() != 0 {
		t.Fatalf("expired no-dispatch proof reached Gateway: executes=%d queries=%d", gateway.executes.Load(), gateway.queries.Load())
	}
	stopped, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Operation.Status != domain.OperationReviewRequired || stopped.Operation.FirstDispatchedAt == nil ||
		stopped.Operation.RecoveryStopReason != "routing_authority_expired_before_first_gateway_io" {
		t.Fatalf("crash-window operation was not quarantined: %+v", stopped.Operation)
	}
}

func TestCrashWindowNoDispatchClaimIsFencedAtRoutingDeadline(t *testing.T) {
	store, _, ctx := isolatedAppStore(t)
	preparedAt := time.Now().UTC()
	aggregate := prepareVersionedRecoveryOperation(t, ctx, store, preparedAt)
	if _, err := store.MarkOperationSending(ctx, aggregate.Operation.ID, preparedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	executableUntil := *aggregate.Attempt.RoutingExecutableUntil
	queryAt := executableUntil.Add(-100 * time.Millisecond)
	queryClaims, err := store.ClaimRecoverableOperations(ctx, queryAt, 0, 4*time.Second, 1)
	if err != nil || len(queryClaims) != 1 || queryClaims[0].Action != recovery.Query {
		t.Fatalf("crash-window query claim=%+v err=%v", queryClaims, err)
	}
	evidenceAt := executableUntil.Add(-90 * time.Millisecond)
	evidence := recovery.AbsenceEvidence{
		Kind: recovery.GatewayOperationAbsent, Identity: queryClaims[0].Identity,
		ObservedAt: evidenceAt, ValidUntil: executableUntil.Add(time.Second),
	}
	if err := store.RecordRecoveryEvidence(ctx, queryClaims[0], evidence, evidenceAt); err != nil {
		t.Fatal(err)
	}
	resubmitAt := executableUntil.Add(-80 * time.Millisecond)
	resubmitClaims, err := store.ClaimRecoverableOperations(ctx, resubmitAt, 0, 4*time.Second, 1)
	if err != nil || len(resubmitClaims) != 1 || resubmitClaims[0].Action != recovery.SameKeyResubmit {
		t.Fatalf("fresh no-dispatch resubmit claim=%+v err=%v", resubmitClaims, err)
	}
	claim := resubmitClaims[0]
	if claim.FirstDispatchExecutableUntil == nil || !claim.CallDeadline.Equal(executableUntil) {
		t.Fatalf("no-dispatch proof did not inherit the routing boundary: %+v", claim)
	}

	gateway := &forbiddenRecoveryGateway{}
	service := NewService(store, &concurrentRouter{}, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	service.now = func() time.Time { return executableUntil }
	if err := service.executeRecovery(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if gateway.executes.Load() != 0 || gateway.queries.Load() != 0 {
		t.Fatalf("deadline-crossing crash recovery reached Gateway: executes=%d queries=%d", gateway.executes.Load(), gateway.queries.Load())
	}
	stopped, err := store.GetAggregateByOperation(ctx, aggregate.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Operation.Status != domain.OperationReviewRequired ||
		stopped.Operation.RecoveryStopReason != "routing_authority_expired_before_first_gateway_io" {
		t.Fatalf("deadline-crossing crash recovery was not quarantined: %+v", stopped.Operation)
	}
}

func TestResponseLossRecoveryQueriesThenResubmitsTheSameDurableOperation(t *testing.T) {
	store, pool, ctx := isolatedAppStore(t)
	var posts, queries atomic.Int32
	var original clients.GatewayOperationRequest
	var originalMu sync.Mutex
	gatewayServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Internal-Token") != "recovery-internal" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodPost:
			var command clients.GatewayOperationRequest
			if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
				t.Error(err)
				return
			}
			call := posts.Add(1)
			originalMu.Lock()
			if call == 1 {
				original = command
			} else if command != original {
				t.Errorf("resubmit changed command: original=%+v resubmit=%+v", original, command)
			}
			originalMu.Unlock()
			responseAt := time.Now().UTC()
			payload := clients.GatewayOperationResponse{
				OperationID: command.OperationID, AttemptID: command.AttemptID,
				ProviderConnectionID: command.ProviderConnectionID, ProviderConnectionRevision: command.ProviderConnectionRevision,
				ProviderNamespaceID: command.ProviderNamespaceID, Provider: command.Provider,
				ProviderIdempotencyKey: command.OperationID, Status: "unknown", SideEffectCertainty: "possible",
				CreatedAt: responseAt, UpdatedAt: responseAt,
			}
			status := http.StatusAccepted
			if call > 1 {
				status = http.StatusOK
				payload.Status = "succeeded"
				payload.SideEffectCertainty = "confirmed"
				payload.ProviderReference = "provider_" + command.OperationID
			}
			response.WriteHeader(status)
			_ = json.NewEncoder(response).Encode(payload)
		case http.MethodGet:
			queries.Add(1)
			query := request.URL.Query()
			observed := time.Now().UTC()
			response.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"error": map[string]string{"code": "operation_resubmit_required", "message": "proof attached"},
				"recovery": map[string]any{
					"schema_version": "gateway_recovery.v1", "kind": "provider_conclusive_absent", "request_hash_version": 2,
					"operation_id": strings.TrimPrefix(request.URL.Path, "/internal/v1/provider-operations/"), "attempt_id": query.Get("attempt_id"),
					"provider_connection_id":       query.Get("provider_connection_id"),
					"provider_connection_revision": query.Get("provider_connection_revision"),
					"provider_namespace_id":        query.Get("provider_namespace_id"), "provider": query.Get("provider"),
					"provider_idempotency_key": query.Get("provider_idempotency_key"), "request_hash": query.Get("request_hash"),
					"observed_at": observed, "valid_until": observed.Add(5 * time.Second),
				},
			})
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer gatewayServer.Close()
	gatewayClient, err := clients.NewGatewayClient(gatewayServer.URL, "recovery-internal", gatewayServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &concurrentRouter{}, gatewayClient, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	created, err := service.CreateIntent(ctx, "merchant_recovery", "create_recovery", CreateIntentInput{
		MerchantReference: "order_recovery", AmountMinor: 2599, Currency: "USD",
		PaymentMethodToken: "pm_recovery", CaptureMethod: string(domain.CaptureAutomatic),
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := service.ConfirmIntent(ctx, "merchant_recovery", created.Intent.ID, "confirm_recovery")
	if err != nil || confirmed.Intent.Status != domain.IntentProcessing || posts.Load() != 1 {
		t.Fatalf("ambiguous first call result=%+v posts=%d err=%v", confirmed, posts.Load(), err)
	}
	// Skip only the scheduled wait, not the budget/deadline/identity checks.
	if _, err := pool.Exec(ctx, `UPDATE provider_operations SET next_retry_at=clock_timestamp() WHERE payment_intent_id=$1`, created.Intent.ID); err != nil {
		t.Fatal(err)
	}
	service.reconcileBatch(ctx, 0)
	if posts.Load() != 2 || queries.Load() != 1 {
		t.Fatalf("recovery calls posts=%d queries=%d, want one query and one separately claimed resubmit", posts.Load(), queries.Load())
	}
	recovered, err := service.GetIntent(ctx, "merchant_recovery", created.Intent.ID)
	if err != nil || recovered.Intent.Status != domain.IntentSucceeded || recovered.Intent.CapturedAmountMinor != 2599 {
		t.Fatalf("payment did not converge after recovery: result=%+v err=%v", recovered, err)
	}
	var attempts, operations, distinctOperations, budget int
	var operationID, providerKey, connectionID string
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(DISTINCT po.id),min(po.id),min(po.provider_idempotency_key),
		       min(po.provider_connection_id),min(po.recovery_budget_remaining)
		FROM provider_operations po WHERE po.payment_intent_id=$1`, created.Intent.ID,
	).Scan(&operations, &distinctOperations, &operationID, &providerKey, &connectionID, &budget); err != nil {
		t.Fatal(err)
	}
	// The second count above deliberately proves one distinct Operation; the
	// separate Attempt count catches an accidental fallback path.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE payment_intent_id=$1`, created.Intent.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || operations != 1 || distinctOperations != 1 || operationID == "" || providerKey != operationID ||
		connectionID != "simulator_a_us_primary" || budget != 30 {
		t.Fatalf("recovery changed the funds path: attempts=%d operations=%d op=%q key=%q connection=%q budget=%d",
			attempts, operations, operationID, providerKey, connectionID, budget)
	}
	originalMu.Lock()
	defer originalMu.Unlock()
	if original.OperationID != operationID || original.ProviderConnectionID != connectionID {
		t.Fatalf("HTTP command did not use the durable identity: command=%+v", original)
	}
}

func TestRecoveryOrdinary404NeverBecomesASecondPost(t *testing.T) {
	store, pool, ctx := isolatedAppStore(t)
	var posts, queries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			posts.Add(1)
			var command clients.GatewayOperationRequest
			_ = json.NewDecoder(request.Body).Decode(&command)
			responseAt := time.Now().UTC()
			response.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(response).Encode(clients.GatewayOperationResponse{
				OperationID: command.OperationID, AttemptID: command.AttemptID,
				ProviderConnectionID: command.ProviderConnectionID, ProviderConnectionRevision: command.ProviderConnectionRevision,
				ProviderNamespaceID: command.ProviderNamespaceID, Provider: command.Provider,
				ProviderIdempotencyKey: command.OperationID, Status: "unknown", SideEffectCertainty: "possible",
				CreatedAt: responseAt, UpdatedAt: responseAt,
			})
			return
		}
		queries.Add(1)
		response.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(response, `{"error":{"code":"operation_not_found"}}`)
	}))
	defer server.Close()
	client, err := clients.NewGatewayClient(server.URL, "recovery-internal", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &concurrentRouter{}, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	created, err := service.CreateIntent(ctx, "merchant_404", "create_404", CreateIntentInput{
		MerchantReference: "order_404", AmountMinor: 100, Currency: "USD",
		PaymentMethodToken: "pm_404", CaptureMethod: string(domain.CaptureAutomatic),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmIntent(ctx, "merchant_404", created.Intent.ID, "confirm_404"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_operations SET next_retry_at=clock_timestamp() WHERE payment_intent_id=$1`, created.Intent.ID); err != nil {
		t.Fatal(err)
	}
	service.reconcileBatch(ctx, 0)
	if posts.Load() != 1 || queries.Load() != 1 {
		t.Fatalf("ordinary 404 authorized resubmit: posts=%d queries=%d", posts.Load(), queries.Load())
	}
	current, err := service.GetIntent(ctx, "merchant_404", created.Intent.ID)
	if err != nil || current.Intent.Status != domain.IntentProcessing || current.Intent.IntegrityStatus != domain.IntegrityHealthy {
		t.Fatalf("ordinary 404 fabricated a terminal/review outcome: result=%+v err=%v", current, err)
	}
	var budget int
	if err := pool.QueryRow(ctx, `SELECT recovery_budget_remaining FROM provider_operations WHERE payment_intent_id=$1`, created.Intent.ID).Scan(&budget); err != nil {
		t.Fatal(err)
	}
	if budget != 31 {
		t.Fatalf("ordinary query budget=%d, want exactly one charged recovery call", budget)
	}
}

type forbiddenRecoveryGateway struct {
	executes atomic.Int32
	queries  atomic.Int32
}

type versionedRecoveryRouter struct {
	authorityWindow time.Duration
}

func (r *versionedRecoveryRouter) Decide(_ context.Context, request clients.RoutingRequest) (clients.RoutingDecision, error) {
	if request.Context == nil {
		return clients.RoutingDecision{}, errors.New("versioned routing context is required")
	}
	digest, err := request.Context.Digest()
	if err != nil {
		return clients.RoutingDecision{}, err
	}
	candidate := clients.RouteCandidate{
		CandidateID: "simulator_a_us_primary", ConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID: "simulator_a", Provider: "simulator_a", MerchantAccount: "local_account",
		Connector: "simulator", Region: "us",
	}
	decision := clients.RoutingDecision{
		SelectedCandidate: candidate, CandidateOrder: []string{candidate.CandidateID}, Exclusions: []clients.CandidateExclusion{},
		PolicyVersion: "rules.v1", ContextSchemaVersion: request.Context.SchemaVersion, ContextDigest: digest,
		ExecutableUntil: request.Context.BuiltAt.Add(r.authorityWindow), RuleSetVersion: "rules.v1",
		RuleSetDigest: "sha256:" + strings.Repeat("1", 64), CapabilityCatalogVersion: "catalog.v1",
		CapabilityCatalogDigest: "sha256:" + strings.Repeat("2", 64), RuleHits: []clients.RuleHit{},
		Eligibility: []clients.EligibilityEvidence{{CandidateID: candidate.CandidateID, Status: "eligible", Reasons: []clients.ExclusionReason{}}},
		ReasonCodes: []string{},
	}
	decision.DecisionID = clients.VersionedDecisionContentID(decision)
	return decision, nil
}

func (g *forbiddenRecoveryGateway) Execute(context.Context, clients.GatewayOperationRequest) (domain.ProviderOutcome, error) {
	g.executes.Add(1)
	return domain.ProviderOutcome{}, errors.New("forbidden recovery execute")
}

func (g *forbiddenRecoveryGateway) Query(context.Context, clients.GatewayOperationIdentity) (domain.ProviderOutcome, error) {
	g.queries.Add(1)
	return domain.ProviderOutcome{}, errors.New("forbidden recovery query")
}

func prepareVersionedRecoveryOperation(t *testing.T, ctx context.Context, store *postgres.Store, now time.Time) postgres.Aggregate {
	t.Helper()
	suffix := fmt.Sprintf("expired_before_dispatch_%d", now.UnixNano())
	intent, err := domain.NewIntent("pi_"+suffix, "merchant_"+suffix, "order_"+suffix, 2599, "USD", "pm_sim_approved", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateIntent(ctx, postgres.CreateIntentCommand{
		Intent: intent, IdempotencyKey: "create_" + suffix, Operation: "create_payment_intent",
		RequestHash: []byte("create_hash_" + suffix), Response: postgres.StoredResponse{Status: http.StatusCreated, Body: []byte(`{"created":true}`)},
	}); err != nil {
		t.Fatal(err)
	}
	attemptID, operationID := "att_"+suffix, "op_"+suffix
	builder, err := routingcontext.NewBuilder([]routingcontext.MerchantProfile{{
		MerchantID: intent.MerchantID, EntityID: "entity_us", Country: "US", Version: "merchant_config.v1",
		ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}}, routingcontext.SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := builder.Build(ctx, routingcontext.Input{
		PaymentID: intent.ID, AttemptID: attemptID, MerchantID: intent.MerchantID,
		AmountMinor: intent.AmountMinor, Currency: intent.Currency, CaptureMethod: string(intent.CaptureMethod),
		PaymentMethodToken: intent.PaymentMethodToken,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	decision := clients.RoutingDecision{
		SelectedCandidate: clients.RouteCandidate{
			CandidateID: "simulator_a_us_primary", ConnectionRevision: "simulator_a_us_primary_v1",
			ProviderNamespaceID: "simulator_a", Provider: "simulator_a", MerchantAccount: "local_account",
			Connector: "simulator", Region: "us",
		},
		CandidateOrder: []string{"simulator_a_us_primary"}, Exclusions: []clients.CandidateExclusion{}, PolicyVersion: "rules.v1",
		ContextSchemaVersion: routingcontext.SchemaVersion, ContextDigest: digest,
		ExecutableUntil: now.Add(time.Minute).UTC().Truncate(time.Microsecond),
		RuleSetVersion:  "rules.v1", RuleSetDigest: "sha256:" + strings.Repeat("1", 64),
		CapabilityCatalogVersion: "catalog.v1", CapabilityCatalogDigest: "sha256:" + strings.Repeat("2", 64),
		RuleHits: []clients.RuleHit{}, Eligibility: []clients.EligibilityEvidence{{CandidateID: "simulator_a_us_primary", Status: "eligible", Reasons: []clients.ExclusionReason{}}}, ReasonCodes: []string{},
	}
	decision.DecisionID = clients.VersionedDecisionContentID(decision)
	executableUntil := decision.ExecutableUntil.UTC()
	contextJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decisionJSON, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	gatewayCommand := clients.GatewayOperationRequest{
		OperationID: operationID, AttemptID: attemptID,
		ProviderConnectionID:       decision.SelectedCandidate.CandidateID,
		ProviderConnectionRevision: decision.SelectedCandidate.ConnectionRevision,
		ProviderNamespaceID:        decision.SelectedCandidate.ProviderNamespaceID, Provider: decision.SelectedCandidate.Provider,
		AmountMinor: intent.AmountMinor, Currency: intent.Currency, PaymentMethodToken: intent.PaymentMethodToken,
		CaptureMethod: string(intent.CaptureMethod),
	}
	providerPayload, err := json.Marshal(gatewayCommand)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PrepareConfirmation(ctx, postgres.PrepareConfirmationCommand{
		MerchantID: intent.MerchantID, IdempotencyKey: "confirm_" + suffix,
		OperationName: "confirm_payment_intent:" + intent.ID, RequestHash: []byte("confirm_hash_" + suffix), Now: now,
		Attempt: domain.PaymentAttempt{
			ID: attemptID, PaymentIntentID: intent.ID, AttemptNumber: 1, RoutingDecisionID: decision.DecisionID,
			RoutingContextSnapshot: string(contextJSON), RoutingDecisionSnapshot: string(decisionJSON),
			RoutingExecutableUntil:     &executableUntil,
			ProviderConnectionID:       decision.SelectedCandidate.CandidateID,
			ProviderConnectionRevision: decision.SelectedCandidate.ConnectionRevision,
			ProviderNamespaceID:        decision.SelectedCandidate.ProviderNamespaceID, Provider: decision.SelectedCandidate.Provider,
			RerouteDisposition: domain.RerouteBlocked, CreatedAt: now, UpdatedAt: now,
		},
		Operation: domain.ProviderOperation{
			ID: operationID, PaymentIntentID: intent.ID, PaymentAttemptID: attemptID, Kind: domain.OperationSale,
			AmountMinor: intent.AmountMinor, Currency: intent.Currency,
			ProviderConnectionID:       decision.SelectedCandidate.CandidateID,
			ProviderConnectionRevision: decision.SelectedCandidate.ConnectionRevision,
			ProviderNamespaceID:        decision.SelectedCandidate.ProviderNamespaceID, Provider: decision.SelectedCandidate.Provider,
			ProviderIdempotencyKey: operationID, MerchantReference: operationID,
			RequestHash: idempotency.Fingerprint("provider_sale", providerPayload), CreatedAt: now, UpdatedAt: now,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared.Aggregate
}

var _ Gateway = (*clients.GatewayClient)(nil)
