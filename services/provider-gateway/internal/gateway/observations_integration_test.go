//go:build integration

package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostgresProviderObservationLedgerFencesStaleGenerationAndIsImmutable(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprint(now.UnixNano())
	operationID := "op_gateway_observation_" + suffix
	seed := Operation{
		OperationID: operationID, AttemptID: "att_" + operationID,
		ProviderConnectionID: "provider_observation_primary", ProviderConnectionRevision: "provider_observation_primary_v1",
		ProviderNamespaceID: "provider_observation", Provider: "provider_observation",
		ProviderIdempotencyKey: operationID, RequestHash: "request_hash_" + suffix,
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: "legacy_hash_" + suffix,
		Status: StatusCreated, SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	}
	created, inserted, err := store.CreateOrGet(ctx, seed)
	if err != nil || !inserted {
		t.Fatalf("create observation operation: inserted=%v err=%v", inserted, err)
	}
	first, claimed, err := store.ClaimDispatch(ctx, created, time.Second)
	if err != nil || !claimed {
		t.Fatalf("first claim=%+v claimed=%v err=%v", first, claimed, err)
	}
	if _, err := store.pool.Exec(ctx, `
		UPDATE provider_gateway.operations
		SET dispatch_lease_until=clock_timestamp()-interval '1 microsecond'
		WHERE operation_id=$1`, operationID); err != nil {
		t.Fatal(err)
	}
	claimable, err := store.Get(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	second, claimed, err := store.ClaimDispatch(ctx, claimable, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("second claim=%+v claimed=%v err=%v", second, claimed, err)
	}

	late := providerResponseObservation(operationID, ObservationSourceQuery, providerResponse{
		Provider: seed.Provider, OperationID: operationID,
		ProviderReference: "late_authorization_" + suffix, Status: "authorized",
	}, StatusUnknown, "possible", "late_authorization_"+suffix, "authorization_only")
	current, applied, err := store.ApplyProviderObservation(ctx, late, &first)
	if err != nil || applied || current.Status != StatusSending || !current.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("stale PostgreSQL observation current=%+v applied=%v err=%v", current, applied, err)
	}
	if active, err := store.DispatchInFlight(ctx, operationID); err != nil || !active {
		t.Fatalf("stale PostgreSQL observation cleared newer lease: active=%v err=%v", active, err)
	}
	observations, err := store.ListProviderObservations(ctx, operationID)
	if err != nil || len(observations) != 1 {
		t.Fatalf("stale PostgreSQL observation ledger=%+v err=%v", observations, err)
	}
	// PostgreSQL hashes the canonical jsonb text it actually persisted. pgx
	// returns those same bytes, so this detects caller-hash/storage drift.
	latePayloadHash := sha256.Sum256(observations[0].SanitizedPayload)
	if observations[0].PayloadHash != hex.EncodeToString(latePayloadHash[:]) ||
		observations[0].AttemptID != seed.AttemptID ||
		observations[0].ProviderConnectionRevision != seed.ProviderConnectionRevision {
		t.Fatalf("stale PostgreSQL observation identity/hash=%+v", observations[0])
	}

	reference := "terminal_reference_" + suffix
	terminal := providerResponseObservation(operationID, ObservationSourceQuery, providerResponse{
		Provider: seed.Provider, OperationID: operationID, ProviderReference: reference, Status: "captured",
	}, StatusSucceeded, "confirmed", reference, "")
	current, applied, err = store.ApplyProviderObservation(ctx, terminal, nil)
	if err != nil || !applied || current.Status != StatusSucceeded || current.ProviderReference != reference {
		t.Fatalf("terminal PostgreSQL observation current=%+v applied=%v err=%v", current, applied, err)
	}
	if active, err := store.DispatchInFlight(ctx, operationID); err != nil || active {
		t.Fatalf("terminal PostgreSQL observation retained lease: active=%v err=%v", active, err)
	}

	conflicting := providerResponseObservation(operationID, ObservationSourceQuery, providerResponse{
		Provider: seed.Provider, OperationID: operationID, ProviderReference: reference, Status: "declined", FailureCode: "do_not_honor",
	}, StatusFailed, "none", reference, "do_not_honor")
	current, applied, err = store.ApplyProviderObservation(ctx, conflicting, nil)
	if err != nil || !applied || current.Status != StatusReview {
		t.Fatalf("conflicting terminal PostgreSQL observation current=%+v applied=%v err=%v", current, applied, err)
	}
	observations, err = store.ListProviderObservations(ctx, operationID)
	if err != nil || len(observations) != 3 {
		t.Fatalf("terminal conflict evidence count=%d observations=%+v err=%v", len(observations), observations, err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO provider_gateway.provider_operation_observations (
			operation_id,attempt_id,provider_connection_id,provider_connection_revision,
			provider_namespace_id,provider,source,raw_provider_status,normalized_status,
			normalized_certainty,payload_hash,sanitized_payload,observed_at
		)
		SELECT operation_id,attempt_id,provider_connection_id,provider_connection_revision,
		       provider_namespace_id,provider,'sale','created','created','none',
		       sha256(convert_to('{}'::jsonb::text,'UTF8')),'{}'::jsonb,clock_timestamp()
		FROM provider_gateway.operations WHERE operation_id=$1`, operationID); err == nil {
		t.Fatal("PostgreSQL accepted created as a runtime Provider observation")
	} else {
		requireGatewayCheckViolation(t, err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO provider_gateway.provider_operation_observations (
			operation_id,attempt_id,provider_connection_id,provider_connection_revision,
			provider_namespace_id,provider,source,raw_provider_status,normalized_status,
			normalized_certainty,payload_hash,sanitized_payload,observed_at
		)
		SELECT operation_id,attempt_id,provider_connection_id,provider_connection_revision,
		       provider_namespace_id,provider,'query','authorized','unknown','possible',
		       sha256(convert_to('{}'::jsonb::text,'UTF8')),'{"different":true}'::jsonb,clock_timestamp()
		FROM provider_gateway.operations WHERE operation_id=$1`, operationID); err == nil {
		t.Fatal("PostgreSQL accepted a payload hash that did not match persisted jsonb")
	} else {
		requireGatewayCheckViolation(t, err)
	}

	if _, err := store.pool.Exec(ctx, `
		UPDATE provider_gateway.provider_operation_observations
		SET raw_provider_status='rewritten'
		WHERE id=$1`, observations[0].ID); err == nil {
		t.Fatal("append-only observation UPDATE succeeded")
	} else {
		requireGatewayCheckViolation(t, err)
	}
	if _, err := store.pool.Exec(ctx, `
		DELETE FROM provider_gateway.provider_operation_observations WHERE id=$1`, observations[0].ID); err == nil {
		t.Fatal("append-only observation DELETE succeeded")
	} else {
		requireGatewayCheckViolation(t, err)
	}
	if _, err := store.pool.Exec(ctx, `TRUNCATE provider_gateway.provider_operation_observations`); err == nil {
		t.Fatal("append-only observation TRUNCATE succeeded")
	} else {
		requireGatewayCheckViolation(t, err)
	}
	var retained int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.provider_operation_observations
		WHERE operation_id=$1`, operationID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != len(observations) {
		t.Fatalf("observation count=%d after rejected mutation, want %d", retained, len(observations))
	}

	futureOperationID := "op_gateway_future_observation_" + suffix
	future := now.Add(5 * time.Minute)
	futureSeed := seed
	futureSeed.OperationID = futureOperationID
	futureSeed.AttemptID = "att_" + futureOperationID
	futureSeed.ProviderIdempotencyKey = futureOperationID
	futureSeed.RequestHash = "request_hash_future_" + suffix
	futureSeed.LegacyRequestHash = "legacy_hash_future_" + suffix
	futureSeed.Status = StatusCreated
	futureSeed.SideEffectCertainty = "none"
	futureSeed.FirstDispatchedAt = nil
	futureSeed.ProviderReference = ""
	futureSeed.FailureCode = ""
	futureSeed.CreatedAt = future
	futureSeed.UpdatedAt = future
	futureCreated, inserted, err := store.CreateOrGet(ctx, futureSeed)
	if err != nil || !inserted {
		t.Fatalf("future observation operation inserted=%v err=%v", inserted, err)
	}
	futureClaim, claimed, err := store.ClaimDispatch(ctx, futureCreated, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("future observation claim=%+v claimed=%v err=%v", futureClaim, claimed, err)
	}
	futureTerminal := providerResponseObservation(futureOperationID, ObservationSourceSale, providerResponse{
		Provider: futureSeed.Provider, OperationID: futureOperationID,
		ProviderReference: "future_observation_reference_" + suffix, Status: "captured",
	}, StatusSucceeded, "confirmed", "future_observation_reference_"+suffix, "")
	futureCompleted, applied, err := store.ApplyProviderObservation(ctx, futureTerminal, nil)
	if err != nil || !applied || futureCompleted.UpdatedAt.Before(futureCompleted.CreatedAt) ||
		!futureCompleted.UpdatedAt.After(futureClaim.UpdatedAt) {
		t.Fatalf("future terminal observation=%+v applied=%v claim=%+v err=%v", futureCompleted, applied, futureClaim, err)
	}
}

func TestPostgresWebhookObservationIsAtomicWithReducerAndRetainsConflictingDelivery(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprint(now.UnixNano())
	operationID := "op_gateway_webhook_observation_" + suffix
	identity := ProviderConnectionIdentity{
		Provider: "provider_webhook_observation", ConnectionRevision: "provider_webhook_observation_primary_v1",
		NamespaceID: "provider_webhook_observation",
	}
	seed := Operation{
		OperationID: operationID, AttemptID: "att_" + operationID,
		ProviderConnectionID:       "provider_webhook_observation_primary",
		ProviderConnectionRevision: identity.ConnectionRevision,
		ProviderNamespaceID:        identity.NamespaceID, Provider: identity.Provider,
		ProviderIdempotencyKey: operationID, RequestHash: "request_hash_" + suffix,
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: "legacy_hash_" + suffix,
		Status: StatusSending, SideEffectCertainty: "possible", FirstDispatchedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, inserted, err := store.CreateOrGet(ctx, seed); err != nil || !inserted {
		t.Fatalf("create webhook observation operation: inserted=%v err=%v", inserted, err)
	}
	event := VerifiedWebhookEvent{
		PathConnectionID: seed.ProviderConnectionID, PathIdentity: identity,
		AllowedOperationIdentities: map[string]ProviderConnectionIdentity{seed.ProviderConnectionID: identity},
		EventID:                    "evt_gateway_observation_" + suffix, OperationID: operationID,
		RawProviderStatus: "captured", Status: StatusSucceeded, SideEffectCertainty: "confirmed",
		ProviderReference: "webhook_reference_" + suffix, OccurredAt: now,
		SanitizedPayload: mustJSON(map[string]any{
			"event_id": "evt_gateway_observation_" + suffix, "operation_id": operationID,
			"provider": identity.Provider, "status": "captured",
		}),
	}
	outcome, inserted, err := store.ApplyWebhook(ctx, event)
	if err != nil || !inserted || outcome.Status != StatusSucceeded {
		t.Fatalf("webhook observation outcome=%+v inserted=%v err=%v", outcome, inserted, err)
	}
	observations, err := store.ListProviderObservations(ctx, operationID)
	if err != nil || len(observations) != 1 || observations[0].ProviderEventID != event.EventID || observations[0].RawProviderStatus != "captured" {
		t.Fatalf("webhook evidence after reducer=%+v err=%v", observations, err)
	}

	conflict := event
	conflict.RawProviderStatus = "succeeded"
	conflict.SanitizedPayload = mustJSON(map[string]any{
		"event_id": event.EventID, "operation_id": operationID,
		"provider": identity.Provider, "status": "succeeded",
	})
	if _, inserted, err := store.ApplyWebhook(ctx, conflict); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("conflicting webhook inserted=%v err=%v", inserted, err)
	}
	current, err := store.Get(ctx, operationID)
	if err != nil || current.Status != StatusReview || current.FailureCode != "webhook_event_collision" {
		t.Fatalf("conflicting webhook reducer state=%+v err=%v", current, err)
	}
	observations, err = store.ListProviderObservations(ctx, operationID)
	if err != nil || len(observations) != 2 || observations[1].RawProviderStatus != "succeeded" {
		t.Fatalf("conflicting webhook delivery evidence=%+v err=%v", observations, err)
	}
}

func TestPostgresAuthorizedWebhookPreservesActiveDispatchGeneration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var getCalls atomic.Int32
	var postCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCalls.Add(1)
		case http.MethodPost:
			postCalls.Add(1)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "operation_not_found"}})
	}))
	defer provider.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprint(now.UnixNano())
	connectionID := "provider_active_webhook_" + suffix
	providerKind := "provider_active_webhook"
	input := OperationRequest{
		OperationID: "op_active_webhook_" + suffix, AttemptID: "att_active_webhook_" + suffix,
		ProviderConnectionID: connectionID, ProviderConnectionRevision: connectionID + "_v1",
		ProviderNamespaceID: connectionID, Provider: providerKind,
		AmountMinor: 1250, Currency: "USD", PaymentMethodToken: "pm_active_webhook",
		CaptureMethod: "automatic",
	}
	seed := Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID,
		ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
		ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
		ProviderIdempotencyKey: input.OperationID, RequestHash: operationHashV2(input),
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: operationHashV1(input),
		Status: StatusCreated, SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	}
	created, inserted, err := store.CreateOrGet(ctx, seed)
	if err != nil || !inserted {
		t.Fatalf("create active webhook operation: inserted=%v err=%v", inserted, err)
	}
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{connectionID: providerKind},
		ProviderConnectionRevisions: map[string]string{connectionID: input.ProviderConnectionRevision},
		ProviderNamespaceIDs:        map[string]string{connectionID: input.ProviderNamespaceID},
		ProviderConclusiveAbsence:   map[string]bool{connectionID: true},
		ProviderEndpoints:           map[string]string{connectionID: provider.URL},
		WebhookBaseURL:              "http://gateway.test", WebhookSecrets: map[string]string{connectionID: "test-secret"},
		ProviderTimeout: time.Second,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	claimed, won, err := store.ClaimDispatch(ctx, created, time.Minute)
	if err != nil || !won {
		t.Fatalf("active PostgreSQL webhook claim=%+v won=%v err=%v", claimed, won, err)
	}
	occurredAt := time.Now().UTC()
	body := mustJSON(map[string]any{
		"event_id": "evt_active_webhook_" + suffix, "provider": providerKind,
		"operation_id": input.OperationID, "provider_reference": "active_webhook_reference_" + suffix,
		"status": "authorized", "occurred_at": occurredAt,
	})
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	inserted, err = service.ApplyWebhook(ctx, connectionID, body, hex.EncodeToString(mac.Sum(nil)))
	if err != nil || !inserted {
		t.Fatalf("active PostgreSQL webhook inserted=%v err=%v", inserted, err)
	}
	afterWebhook, err := store.Get(ctx, input.OperationID)
	if err != nil || afterWebhook.Status != StatusSending || !afterWebhook.UpdatedAt.Equal(claimed.UpdatedAt) {
		t.Fatalf("active PostgreSQL webhook operation=%+v err=%v", afterWebhook, err)
	}
	if active, err := store.DispatchInFlight(ctx, input.OperationID); err != nil || !active {
		t.Fatalf("active PostgreSQL webhook lease=%v err=%v", active, err)
	}
	concurrent, err := service.Execute(ctx, input)
	if err != nil || concurrent.Status != StatusSending || !concurrent.UpdatedAt.Equal(claimed.UpdatedAt) {
		t.Fatalf("concurrent PostgreSQL execution=%+v err=%v", concurrent, err)
	}
	if getCalls.Load() != 0 || postCalls.Load() != 0 {
		t.Fatalf("active PostgreSQL lease admitted Provider calls: get=%d post=%d", getCalls.Load(), postCalls.Load())
	}
}
