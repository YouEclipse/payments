package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
)

func TestTimeoutBecomesUnknownThenQueryConverges(t *testing.T) {
	var mu sync.Mutex
	operations := map[string]providerResponse{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if r.Method == http.MethodPost {
			var input struct {
				OperationID string `json:"operation_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			result := providerResponse{Provider: "simulator_a", OperationID: input.OperationID, ProviderReference: "provider_123", Status: "succeeded"}
			mu.Lock()
			operations[key] = result
			mu.Unlock()
			// Model a provider that committed the side effect but never delivered
			// its response. Bound the wait even if a broken transport fails to
			// propagate cancellation so httptest.Server.Close cannot hang forever.
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		mu.Lock()
		result := operations[strings.TrimPrefix(r.URL.Path, "/v1/sales/")]
		mu.Unlock()
		writeJSON(w, http.StatusOK, result)
	}))
	defer provider.Close()

	store := NewMemoryStore()
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{"simulator_a": "simulator_a"},
		ProviderConnectionRevisions: map[string]string{"simulator_a": "simulator_a_v1"}, ProviderNamespaceIDs: map[string]string{"simulator_a": "simulator_a"},
		ProviderEndpoints: map[string]string{"simulator_a": provider.URL},
		WebhookBaseURL:    provider.URL, WebhookSecrets: map[string]string{"simulator_a": "secret"}, ProviderTimeout: 100 * time.Millisecond,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	input := OperationRequest{OperationID: "op_1", AttemptID: "attempt_1", ProviderConnectionID: "simulator_a", ProviderConnectionRevision: "simulator_a_v1", ProviderNamespaceID: "simulator_a", Provider: "simulator_a", AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_sim_timeout_then_succeeded", CaptureMethod: "automatic"}
	op, err := service.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown || op.SideEffectCertainty != "possible" {
		t.Fatalf("timeout must be unknown/possible, got %+v", op)
	}
	op, err = service.Get(context.Background(), input.OperationID, true)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusSucceeded || op.SideEffectCertainty != "confirmed" {
		t.Fatalf("query did not converge: %+v", op)
	}
	if op.FailureCode != "" {
		t.Fatalf("converged success retained a transient failure code: %+v", op)
	}
}

func TestSameProviderConnectionsStayPinnedThroughUnknownRecovery(t *testing.T) {
	var primaryPosts atomic.Int32
	var primaryQueries atomic.Int32
	var backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			primaryPosts.Add(1)
			// An ambiguous 5xx is enough to exercise UNKNOWN recovery here; the
			// dedicated timeout test above covers transport cancellation. Returning
			// promptly also keeps the pinning test free of server-close timing races.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		primaryQueries.Add(1)
		_, _ = io.WriteString(w, `{"provider":"provider_a","operation_id":"op_pinned","provider_reference":"ref_pinned","status":"captured"}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backup.Close()

	service, err := NewService(Config{
		Environment: "test",
		ProviderKinds: map[string]string{
			"provider_a_us_primary": "provider_a",
			"provider_a_eu_backup":  "provider_a",
		},
		ProviderConnectionRevisions: map[string]string{
			"provider_a_us_primary": "provider_a_us_primary_v1",
			"provider_a_eu_backup":  "provider_a_eu_backup_v1",
		},
		ProviderNamespaceIDs: map[string]string{
			"provider_a_us_primary": "provider_a",
			"provider_a_eu_backup":  "provider_a",
		},
		ProviderEndpoints: map[string]string{
			"provider_a_us_primary": primary.URL,
			"provider_a_eu_backup":  backup.URL,
		},
		WebhookBaseURL: "http://gateway.test",
		WebhookSecrets: map[string]string{
			"provider_a_us_primary": "primary-secret",
			"provider_a_eu_backup":  "backup-secret",
		},
		ProviderTimeout: 100 * time.Millisecond,
	}, NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	input := OperationRequest{
		OperationID: "op_pinned", AttemptID: "att_pinned",
		ProviderConnectionID: "provider_a_us_primary", ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID: "provider_a", Provider: "provider_a",
		AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_test", CaptureMethod: "automatic",
	}
	unknown, err := service.Execute(t.Context(), input)
	if err != nil || unknown.Status != StatusUnknown {
		t.Fatalf("Execute() = %+v, %v; want unknown", unknown, err)
	}
	result, err := service.Get(t.Context(), input.OperationID, true)
	if err != nil || result.Status != StatusSucceeded || result.ProviderConnectionID != input.ProviderConnectionID {
		t.Fatalf("Get(reconcile) = %+v, %v; want pinned success", result, err)
	}
	if primaryPosts.Load() != 1 || primaryQueries.Load() != 1 || backupCalls.Load() != 0 {
		t.Fatalf("calls primary_post=%d primary_query=%d backup=%d", primaryPosts.Load(), primaryQueries.Load(), backupCalls.Load())
	}
}

func TestOperationIDCannotMoveBetweenProviderConnections(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			OperationID string `json:"operation_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		writeJSON(w, http.StatusOK, providerResponse{
			Provider: "provider_a", OperationID: input.OperationID,
			ProviderReference: "ref_fixed", Status: "captured",
		})
	}))
	defer provider.Close()

	service, err := NewService(Config{
		Environment: "test",
		ProviderKinds: map[string]string{
			"provider_a_us_primary": "provider_a",
			"provider_a_eu_backup":  "provider_a",
		},
		ProviderConnectionRevisions: map[string]string{
			"provider_a_us_primary": "provider_a_us_primary_v1",
			"provider_a_eu_backup":  "provider_a_eu_backup_v1",
		},
		ProviderNamespaceIDs: map[string]string{
			"provider_a_us_primary": "provider_a",
			"provider_a_eu_backup":  "provider_a",
		},
		ProviderEndpoints: map[string]string{
			"provider_a_us_primary": provider.URL,
			"provider_a_eu_backup":  provider.URL,
		},
		WebhookBaseURL: "http://gateway.test",
		WebhookSecrets: map[string]string{
			"provider_a_us_primary": "primary-secret",
			"provider_a_eu_backup":  "backup-secret",
		},
	}, NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	input := OperationRequest{
		OperationID: "op_connection_immutable", AttemptID: "att_connection_immutable",
		ProviderConnectionID: "provider_a_us_primary", ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID: "provider_a", Provider: "provider_a",
		AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_test", CaptureMethod: "automatic",
	}
	if _, err := service.Execute(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	input.ProviderConnectionID = "provider_a_eu_backup"
	input.ProviderConnectionRevision = "provider_a_eu_backup_v1"
	if _, err := service.Execute(t.Context(), input); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("connection drift error = %v, want ErrIdempotencyConflict", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("connection drift reached provider %d times, want 1", calls.Load())
	}
}

func TestOperationIDCannotChangeRevisionOrNamespaceAndReturnsHTTP409(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, providerResponse{
			Provider: "provider_a", OperationID: "op_identity_tuple", ProviderReference: "ref_identity_tuple", Status: "captured",
		})
	}))
	defer provider.Close()

	store := NewMemoryStore()
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a_us_primary": "provider_a_us_primary_v1"},
		ProviderNamespaceIDs:        map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderEndpoints:           map[string]string{"provider_a_us_primary": provider.URL}, WebhookBaseURL: "http://gateway.test",
		WebhookSecrets: map[string]string{"provider_a_us_primary": "secret"},
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	input := OperationRequest{
		OperationID: "op_identity_tuple", AttemptID: "att_identity_tuple",
		ProviderConnectionID: "provider_a_us_primary", ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID: "provider_a", Provider: "provider_a", AmountMinor: 100, Currency: "USD",
		PaymentMethodToken: "pm_test", CaptureMethod: "automatic",
	}
	if _, err := service.Execute(t.Context(), input); err != nil {
		t.Fatal(err)
	}

	drifted := input
	drifted.ProviderConnectionRevision = "provider_a_us_primary_v2"
	body, _ := json.Marshal(drifted)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/provider-operations", bytes.NewReader(body))
	request.Header.Set("X-Internal-Token", "internal-secret")
	response := httptest.NewRecorder()
	NewHTTPHandler(service, store, "internal-secret").ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("revision drift HTTP status=%d body=%s, want 409", response.Code, response.Body.String())
	}

	drifted = input
	drifted.ProviderNamespaceID = "provider_a_other_namespace"
	if _, err := service.Execute(t.Context(), drifted); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("namespace drift error=%v, want ErrIdempotencyConflict", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("identity drift reached provider %d times, want 1", calls.Load())
	}
}

func TestHTTPReportsConnectionIdentityMismatchWithoutProviderIO(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a_us_primary": "provider_a_us_primary_v2"},
		ProviderNamespaceIDs:        map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderEndpoints:           map[string]string{"provider_a_us_primary": provider.URL},
		WebhookBaseURL:              "http://gateway.test", WebhookSecrets: map[string]string{"provider_a_us_primary": "secret"},
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	oldInput := testOperationRequest("op_unresolved_revision", "provider_a")
	oldInput.ProviderConnectionID, oldInput.ProviderConnectionRevision = "provider_a_us_primary", "provider_a_us_primary_v1"
	seedOperation(t, store, oldInput, StatusUnknown)
	oldCreatedInput := oldInput
	oldCreatedInput.OperationID, oldCreatedInput.AttemptID = "op_unresolved_created_revision", "att_unresolved_created_revision"
	seedOperation(t, store, oldCreatedInput, StatusCreated)
	freshInput := oldInput
	freshInput.OperationID, freshInput.AttemptID = "op_fresh_unresolved_revision", "att_fresh_unresolved_revision"
	oldBody, _ := json.Marshal(oldInput)
	freshBody, _ := json.Marshal(freshInput)
	handler := NewHTTPHandler(service, store, "internal-secret")
	for _, testCase := range []struct {
		method string
		path   string
		body   []byte
		status int
	}{
		{method: http.MethodPost, path: "/internal/v1/provider-operations", body: oldBody, status: http.StatusConflict},
		{method: http.MethodGet, path: "/internal/v1/provider-operations/" + oldInput.OperationID + "?reconcile=true", status: http.StatusConflict},
		{method: http.MethodGet, path: "/internal/v1/provider-operations/" + oldCreatedInput.OperationID + "?reconcile=true", status: http.StatusConflict},
		{method: http.MethodPost, path: "/internal/v1/provider-operations", body: freshBody, status: http.StatusUnprocessableEntity},
	} {
		request := httptest.NewRequest(testCase.method, testCase.path, bytes.NewReader(testCase.body))
		request.Header.Set("X-Internal-Token", "internal-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != testCase.status || !strings.Contains(response.Body.String(), `"code":"provider_connection_identity_mismatch"`) {
			t.Fatalf("%s %s status=%d body=%s", testCase.method, testCase.path, response.Code, response.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unresolved connection identity reached provider %d times", calls.Load())
	}
	if _, err := store.Get(t.Context(), freshInput.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unresolved fresh request was persisted: %v", err)
	}
}

func TestReadinessRejectsRevisionAndNamespaceDrift(t *testing.T) {
	configured := Config{
		Environment: "test", ProviderKinds: map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a_us_primary": "provider_a_us_primary_v2"},
		ProviderNamespaceIDs:        map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderEndpoints:           map[string]string{"provider_a_us_primary": "http://provider.test"},
		WebhookBaseURL:              "http://gateway.test", WebhookSecrets: map[string]string{"provider_a_us_primary": "secret"},
	}
	request := OperationRequest{
		OperationID: "op_revision_drift", AttemptID: "att_revision_drift",
		ProviderConnectionID: "provider_a_us_primary", ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID: "provider_a", Provider: "provider_a", AmountMinor: 100, Currency: "USD",
		PaymentMethodToken: "pm_test", CaptureMethod: "automatic",
	}
	startupStore := NewMemoryStore()
	seedOperation(t, startupStore, request, StatusUnknown)
	if _, err := NewService(configured, startupStore, nil); !errors.Is(err, ErrUnconfiguredConnection) {
		t.Fatalf("revision drift startup error=%v, want ErrUnconfiguredConnection", err)
	}

	readyStore := NewMemoryStore()
	service, err := NewService(configured, readyStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.ProviderConnectionRevision = "provider_a_us_primary_v2"
	request.ProviderNamespaceID = "provider_a_other_namespace"
	seedOperation(t, readyStore, request, StatusSending)
	if err := service.Ready(t.Context()); !errors.Is(err, ErrUnconfiguredConnection) {
		t.Fatalf("namespace drift readiness error=%v, want ErrUnconfiguredConnection", err)
	}
}

func TestRecoveryAliasCanonicalizesWebhookTupleAndSharesNamespaceCollisionDomain(t *testing.T) {
	store := NewMemoryStore()
	primaryRequest := OperationRequest{
		OperationID: "op_primary_alias", AttemptID: "att_primary_alias",
		ProviderConnectionID: "provider_a_us_primary", ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID: "provider_a", Provider: "provider_a", AmountMinor: 100, Currency: "USD",
		PaymentMethodToken: "pm_test", CaptureMethod: "automatic",
	}
	backupRequest := primaryRequest
	backupRequest.OperationID = "op_backup_alias"
	backupRequest.AttemptID = "att_backup_alias"
	backupRequest.ProviderConnectionID = "provider_a_eu_backup"
	backupRequest.ProviderConnectionRevision = "provider_a_eu_backup_v1"
	seedOperation(t, store, primaryRequest, StatusUnknown)
	seedOperation(t, store, backupRequest, StatusUnknown)

	service, err := NewService(Config{
		Environment:                 "test",
		ProviderKinds:               map[string]string{"provider_a": "provider_a", "provider_a_us_primary": "provider_a", "provider_a_eu_backup": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a": legacyConnectionRevision, "provider_a_us_primary": "provider_a_us_primary_v1", "provider_a_eu_backup": "provider_a_eu_backup_v1"},
		ProviderNamespaceIDs:        map[string]string{"provider_a": "provider_a", "provider_a_us_primary": "provider_a", "provider_a_eu_backup": "provider_a"},
		ProviderRecoveryAliases:     map[string]string{"provider_a": "provider_a_us_primary"},
		ProviderEndpoints:           map[string]string{"provider_a": "http://primary.test", "provider_a_us_primary": "http://primary.test", "provider_a_eu_backup": "http://backup.test"},
		WebhookBaseURL:              "http://gateway.test", WebhookSecrets: map[string]string{"provider_a": "primary-secret", "provider_a_us_primary": "primary-secret", "provider_a_eu_backup": "backup-secret"},
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	freshAlias := primaryRequest
	freshAlias.OperationID, freshAlias.AttemptID = "op_fresh_alias_rejected", "att_fresh_alias_rejected"
	freshAlias.ProviderConnectionID, freshAlias.ProviderConnectionRevision = "provider_a", legacyConnectionRevision
	if _, err := service.Execute(t.Context(), freshAlias); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("recovery alias accepted a fresh operation: %v", err)
	}
	if _, err := store.Get(t.Context(), freshAlias.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh recovery alias operation was persisted: %v", err)
	}
	legacyReplay := freshAlias
	legacyReplay.OperationID, legacyReplay.AttemptID = "op_legacy_alias_replay", "att_legacy_alias_replay"
	seedOperation(t, store, legacyReplay, StatusReview)
	if recovered, err := service.Execute(t.Context(), legacyReplay); err != nil || recovered.Status != StatusReview {
		t.Fatalf("existing recovery alias replay: operation=%+v err=%v", recovered, err)
	}
	apply := func(path, secret string, event map[string]any) (bool, error) {
		t.Helper()
		body, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		return service.ApplyWebhook(t.Context(), path, body, hex.EncodeToString(mac.Sum(nil)))
	}
	event := map[string]any{
		"event_id": "evt_alias_shared", "provider": "provider_a", "operation_id": primaryRequest.OperationID,
		"provider_reference": "ref_primary", "status": "captured", "occurred_at": time.Now().UTC(),
	}
	if inserted, err := apply("provider_a", "primary-secret", event); err != nil || !inserted {
		t.Fatalf("alias webhook inserted=%v err=%v", inserted, err)
	}
	storedEvent := store.events[providerEventKey{NamespaceID: "provider_a", EventID: "evt_alias_shared"}]
	if storedEvent.ProviderConnectionID != primaryRequest.ProviderConnectionID ||
		storedEvent.ProviderConnectionRevision != primaryRequest.ProviderConnectionRevision ||
		storedEvent.ProviderNamespaceID != primaryRequest.ProviderNamespaceID {
		t.Fatalf("alias path overwrote durable operation tuple: %+v", storedEvent)
	}
	if inserted, err := apply("provider_a_us_primary", "primary-secret", event); err != nil || inserted {
		t.Fatalf("canonical replay inserted=%v err=%v", inserted, err)
	}

	wrongTrustEvent := map[string]any{
		"event_id": "evt_alias_wrong_trust", "provider": "provider_a", "operation_id": backupRequest.OperationID,
		"provider_reference": "ref_wrong", "status": "captured", "occurred_at": time.Now().UTC(),
	}
	if inserted, err := apply("provider_a", "primary-secret", wrongTrustEvent); !errors.Is(err, ErrNotFound) || inserted {
		t.Fatalf("alias crossed its configured trust target: inserted=%v err=%v", inserted, err)
	}

	conflictEvent := map[string]any{
		"event_id": "evt_alias_shared", "provider": "provider_a", "operation_id": backupRequest.OperationID,
		"provider_reference": "ref_backup", "status": "captured", "occurred_at": time.Now().UTC(),
	}
	if inserted, err := apply("provider_a_eu_backup", "backup-secret", conflictEvent); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("namespace event collision inserted=%v err=%v", inserted, err)
	}
	for _, operationID := range []string{primaryRequest.OperationID, backupRequest.OperationID} {
		op, getErr := store.Get(t.Context(), operationID)
		if getErr != nil || op.Status != StatusReview || op.FailureCode != "webhook_event_collision" {
			t.Fatalf("collision operation %q = %+v err=%v", operationID, op, getErr)
		}
	}
}

func TestProviderReferenceCollisionUsesNamespaceAndPreservesEveryObservation(t *testing.T) {
	store := NewMemoryStore()
	first := OperationRequest{OperationID: "op_ref_primary", AttemptID: "att_ref_primary", ProviderConnectionID: "provider_a_us_primary", ProviderConnectionRevision: "provider_a_us_primary_v1", ProviderNamespaceID: "provider_a", Provider: "provider_a", AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_test", CaptureMethod: "automatic"}
	second := first
	second.OperationID, second.AttemptID = "op_ref_backup", "att_ref_backup"
	second.ProviderConnectionID, second.ProviderConnectionRevision = "provider_a_eu_backup", "provider_a_eu_backup_v1"
	seedOperation(t, store, first, StatusUnknown)
	seedOperation(t, store, second, StatusUnknown)
	if _, err := store.SetStatus(t.Context(), first.OperationID, StatusSucceeded, "confirmed", "ref_shared_namespace", ""); err != nil {
		t.Fatal(err)
	}
	secondResult, err := store.SetStatus(t.Context(), second.OperationID, StatusSucceeded, "confirmed", "ref_shared_namespace", "")
	if err != nil || secondResult.Status != StatusReview {
		t.Fatalf("second reference claim result=%+v err=%v", secondResult, err)
	}
	for _, operationID := range []string{first.OperationID, second.OperationID} {
		op, getErr := store.Get(t.Context(), operationID)
		if getErr != nil || op.Status != StatusReview || op.FailureCode != "provider_reference_collision" {
			t.Fatalf("reference collision operation %q = %+v err=%v", operationID, op, getErr)
		}
	}
	for _, operationID := range []string{first.OperationID, second.OperationID} {
		observationKey := providerReferenceObservationKey{
			Reference: providerReferenceKey{NamespaceID: "provider_a", Reference: "ref_shared_namespace"}, OperationID: operationID,
		}
		if _, exists := store.referenceObservations[observationKey]; !exists {
			t.Fatalf("missing reference observation for %q", operationID)
		}
	}
	items, err := store.PendingOutbox(t.Context(), 10)
	if err != nil || len(items) != 1 || items[0].Outcome.OperationID != first.OperationID ||
		items[0].Outcome.ProviderConnectionRevision == "" || items[0].Outcome.ProviderNamespaceID != "provider_a" {
		t.Fatalf("reference collision owner outbox=%+v err=%v", items, err)
	}
	retainedReview, err := store.SetStatus(t.Context(), first.OperationID, StatusSucceeded, "confirmed", "ref_shared_namespace", "")
	if err != nil || retainedReview.Status != StatusReview || retainedReview.FailureCode != "provider_reference_collision" {
		t.Fatalf("later matching evidence erased durable collision review: operation=%+v err=%v", retainedReview, err)
	}
}

func TestMemoryWebhookKeysDoNotAliasAcrossNamespaces(t *testing.T) {
	store := NewMemoryStore()
	for index, identity := range []struct {
		namespaceID string
		eventID     string
	}{{namespaceID: "a:b", eventID: "c"}, {namespaceID: "a", eventID: "b:c"}} {
		input := testOperationRequest(fmt.Sprintf("op_compound_%d", index), "provider_a")
		input.ProviderConnectionID = fmt.Sprintf("provider_a_%d", index)
		input.ProviderConnectionRevision = input.ProviderConnectionID + "_v1"
		input.ProviderNamespaceID = identity.namespaceID
		seedOperation(t, store, input, StatusUnknown)
		outcome, inserted, err := store.ApplyWebhook(t.Context(), testWebhookEvent(ProviderOutcome{
			EventID: identity.eventID, OperationID: input.OperationID,
			ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
			ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
			Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: fmt.Sprintf("ref_compound_%d", index),
			OccurredAt: time.Now().UTC(),
		}))
		if err != nil || !inserted || outcome.Status != StatusSucceeded {
			t.Fatalf("independent compound key %d: outcome=%+v inserted=%v err=%v", index, outcome, inserted, err)
		}
	}
	if len(store.events) != 2 || len(store.webhookConflicts) != 0 {
		t.Fatalf("compound keys aliased: events=%d conflicts=%d", len(store.events), len(store.webhookConflicts))
	}
}

func TestCollisionEscalationPreservesOriginalReviewReason(t *testing.T) {
	store := NewMemoryStore()
	first := testOperationRequest("op_original_review", "provider_a")
	second := testOperationRequest("op_second_review", "provider_a")
	seedOperation(t, store, first, StatusUnknown)
	seedOperation(t, store, second, StatusUnknown)
	if _, err := store.SetStatus(t.Context(), first.OperationID, StatusReview, "possible", "ref_review_shared", "manual_review"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetStatus(t.Context(), second.OperationID, StatusSucceeded, "confirmed", "ref_review_shared", ""); err != nil {
		t.Fatal(err)
	}
	for _, input := range []OperationRequest{first, second} {
		_, inserted, err := store.ApplyWebhook(t.Context(), testWebhookEvent(ProviderOutcome{
			EventID: "evt_review_shared", OperationID: input.OperationID,
			ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
			ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
			Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "ref_review_shared", OccurredAt: time.Time{},
		}))
		if input.OperationID == first.OperationID {
			if err != nil || !inserted {
				t.Fatalf("first review webhook inserted=%v err=%v", inserted, err)
			}
		} else if !errors.Is(err, ErrWebhookEventConflict) || inserted {
			t.Fatalf("second review webhook inserted=%v err=%v", inserted, err)
		}
	}
	for operationID, expectedReason := range map[string]string{first.OperationID: "manual_review", second.OperationID: "provider_reference_collision"} {
		op, err := store.Get(t.Context(), operationID)
		if err != nil || op.Status != StatusReview || op.FailureCode != expectedReason {
			t.Fatalf("original review reason changed: operation=%+v expected=%q err=%v", op, expectedReason, err)
		}
	}
}

func TestMemoryStoreUpgradesExactLegacyRequestHash(t *testing.T) {
	store := NewMemoryStore()
	input := testOperationRequest("op_legacy_hash", "provider_a")
	input.ProviderConnectionRevision = legacyConnectionRevision
	now := time.Now().UTC()
	store.operations[input.OperationID] = Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID,
		ProviderConnectionID: input.Provider, ProviderConnectionRevision: legacyConnectionRevision,
		ProviderNamespaceID: input.Provider, Provider: input.Provider,
		ProviderIdempotencyKey: input.OperationID,
		RequestHash:            operationHashV1(input), RequestHashVersion: requestHashVersionLegacy,
		Status: StatusCreated, SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	}
	incoming := Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID,
		ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
		ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
		ProviderIdempotencyKey: input.OperationID,
		RequestHash:            operationHashV2(input), RequestHashVersion: requestHashVersionConnection,
		LegacyRequestHash: operationHashV1(input), Status: StatusCreated, SideEffectCertainty: "none",
		CreatedAt: now, UpdatedAt: now,
	}
	stored, created, err := store.CreateOrGet(t.Context(), incoming)
	if err != nil || created {
		t.Fatalf("legacy replay: created=%v stored=%+v err=%v", created, stored, err)
	}
	if stored.RequestHashVersion != requestHashVersionConnection || stored.RequestHash != incoming.RequestHash {
		t.Fatalf("legacy request hash was not upgraded: %+v", stored)
	}
}

func TestOperationHashV2UsesUnambiguousSemanticEncoding(t *testing.T) {
	first := testOperationRequest("op_hash_encoding", "provider_a")
	first.PaymentMethodToken = "pm_test\x00automatic"
	first.Scenario = "approved"
	second := first
	second.PaymentMethodToken = "pm_test"
	second.Scenario = "automatic\x00approved"
	if operationHashV2(first) == operationHashV2(second) {
		t.Fatal("different token/scenario semantics shared a v2 request hash")
	}
}

func TestMemoryStoreRejectsLegacyHashConnectionDrift(t *testing.T) {
	store := NewMemoryStore()
	legacyInput := testOperationRequest("op_legacy_drift", "provider_a")
	legacyInput.ProviderConnectionRevision = legacyConnectionRevision
	now := time.Now().UTC()
	store.operations[legacyInput.OperationID] = Operation{
		OperationID: legacyInput.OperationID, AttemptID: legacyInput.AttemptID,
		ProviderConnectionID: legacyInput.Provider, ProviderConnectionRevision: legacyConnectionRevision,
		ProviderNamespaceID: legacyInput.Provider, Provider: legacyInput.Provider,
		ProviderIdempotencyKey: legacyInput.OperationID,
		RequestHash:            operationHashV1(legacyInput), RequestHashVersion: requestHashVersionLegacy,
		Status: StatusCreated, SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	}
	drifted := legacyInput
	drifted.ProviderConnectionID = "provider_a_eu_backup"
	drifted.ProviderConnectionRevision = "provider_a_eu_backup_v1"
	_, _, err := store.CreateOrGet(t.Context(), Operation{
		OperationID: drifted.OperationID, AttemptID: drifted.AttemptID,
		ProviderConnectionID: drifted.ProviderConnectionID, ProviderConnectionRevision: drifted.ProviderConnectionRevision,
		ProviderNamespaceID: drifted.ProviderNamespaceID, Provider: drifted.Provider,
		ProviderIdempotencyKey: drifted.OperationID,
		RequestHash:            operationHashV2(drifted), RequestHashVersion: requestHashVersionConnection,
		LegacyRequestHash: operationHashV1(drifted), Status: StatusCreated, SideEffectCertainty: "none",
		CreatedAt: now, UpdatedAt: now,
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy connection drift error=%v, want ErrIdempotencyConflict", err)
	}
}

func TestServiceStartupAndReadinessRejectUnconfiguredNonterminalConnections(t *testing.T) {
	configured := Config{
		Environment: "test", ProviderKinds: map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a_us_primary": "provider_a_us_primary_v1"},
		ProviderNamespaceIDs:        map[string]string{"provider_a_us_primary": "provider_a"},
		ProviderEndpoints:           map[string]string{"provider_a_us_primary": "http://provider.test"},
		WebhookBaseURL:              "http://gateway.test", WebhookSecrets: map[string]string{"provider_a_us_primary": "secret"},
	}
	store := NewMemoryStore()
	legacy := testOperationRequest("op_unconfigured_startup", "provider_a")
	seedOperation(t, store, legacy, StatusUnknown)
	if _, err := NewService(configured, store, nil); !errors.Is(err, ErrUnconfiguredConnection) {
		t.Fatalf("startup error=%v, want ErrUnconfiguredConnection", err)
	}

	readyStore := NewMemoryStore()
	service, err := NewService(configured, readyStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	missing := testOperationRequest("op_unconfigured_ready", "provider_a")
	seedOperation(t, readyStore, missing, StatusSending)
	if err := service.Ready(t.Context()); !errors.Is(err, ErrUnconfiguredConnection) {
		t.Fatalf("readiness error=%v, want ErrUnconfiguredConnection", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	NewHTTPHandler(service, readyStore, "").ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status=%d body=%s, want %d", response.Code, response.Body.String(), http.StatusServiceUnavailable)
	}
}

func TestReconcileOutboxIdentityBackfillsAndRejectsDrift(t *testing.T) {
	legacy := ProviderOutcome{Provider: "provider_a", EventID: "evt_legacy", OperationID: "op_legacy"}
	if err := reconcileOutboxIdentity(&legacy, "provider_a", "legacy_v1", "provider_a", "provider_a", "evt_legacy", "op_legacy"); err != nil {
		t.Fatal(err)
	}
	if legacy.ProviderConnectionID != "provider_a" {
		t.Fatalf("legacy outbox identity=%+v", legacy)
	}
	drifted := legacy
	drifted.ProviderConnectionID = "provider_a_eu_backup"
	if err := reconcileOutboxIdentity(&drifted, "provider_a_us_primary", "provider_a_us_primary_v1", "provider_a", "provider_a", "evt_legacy", "op_legacy"); err == nil {
		t.Fatal("drifted outbox payload identity unexpectedly accepted")
	}
}

func TestWebhookIsVerifiedAndDeduplicated(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, providerResponse{Provider: "simulator_a", OperationID: "op_1", ProviderReference: "provider_1", Status: "succeeded"})
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{"simulator_a": "simulator_a"},
		ProviderConnectionRevisions: map[string]string{"simulator_a": "simulator_a_v1"}, ProviderNamespaceIDs: map[string]string{"simulator_a": "simulator_a"},
		ProviderEndpoints: map[string]string{"simulator_a": provider.URL}, WebhookBaseURL: provider.URL,
		WebhookSecrets: map[string]string{"simulator_a": "secret"},
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(context.Background(), OperationRequest{OperationID: "op_1", AttemptID: "attempt_1", ProviderConnectionID: "simulator_a", ProviderConnectionRevision: "simulator_a_v1", ProviderNamespaceID: "simulator_a", Provider: "simulator_a", AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_sim_approved", CaptureMethod: "automatic"})
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"event_id": "evt_1", "provider": "simulator_a", "operation_id": "op_1", "provider_reference": "provider_1", "status": "succeeded", "occurred_at": time.Now().UTC()}
	body, _ := json.Marshal(event)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))
	inserted, err := service.ApplyWebhook(context.Background(), "simulator_a", body, signature)
	if err != nil || !inserted {
		t.Fatalf("first webhook: inserted=%v err=%v", inserted, err)
	}
	inserted, err = service.ApplyWebhook(context.Background(), "simulator_a", body, signature)
	if err != nil || inserted {
		t.Fatalf("duplicate webhook: inserted=%v err=%v", inserted, err)
	}
	items, err := store.PendingOutbox(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("expected one outbox item, got %d err=%v", len(items), err)
	}
}

func TestWebhookEventIDCannotBeReusedForAnotherOperation(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	for _, operationID := range []string{"op_event_a", "op_event_b"} {
		_, _, err := store.CreateOrGet(context.Background(), Operation{
			OperationID: operationID, AttemptID: "attempt_" + operationID,
			ProviderConnectionID: "provider_a", ProviderConnectionRevision: "provider_a_v1",
			ProviderNamespaceID: "provider_a", Provider: "provider_a",
			ProviderIdempotencyKey: operationID, RequestHash: "hash_" + operationID,
			RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: "legacy_hash_" + operationID,
			Status: StatusUnknown, SideEffectCertainty: "possible", CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ProviderOutcome{
		EventID: "evt_reused", OperationID: "op_event_a", ProviderConnectionID: "provider_a", Provider: "provider_a",
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "ref_a", OccurredAt: now,
	}
	if _, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(first)); err != nil || !inserted {
		t.Fatalf("first event: inserted=%v err=%v", inserted, err)
	}
	conflict := first
	conflict.OperationID = "op_event_b"
	conflict.ProviderReference = "ref_b"
	if _, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(conflict)); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("reused event: inserted=%v err=%v, want ErrWebhookEventConflict", inserted, err)
	}
	op, err := store.Get(context.Background(), "op_event_b")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusReview || op.FailureCode != "webhook_event_collision" {
		t.Fatalf("reused event did not durably quarantine the second operation: %+v", op)
	}
}

func TestHTTPRequiresInternalToken(t *testing.T) {
	store := NewMemoryStore()
	service, _ := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{"simulator_a": "simulator_a"},
		ProviderConnectionRevisions: map[string]string{"simulator_a": "simulator_a_v1"}, ProviderNamespaceIDs: map[string]string{"simulator_a": "simulator_a"},
		ProviderEndpoints: map[string]string{"simulator_a": "http://127.0.0.1"}, WebhookBaseURL: "http://127.0.0.1",
		WebhookSecrets: map[string]string{"simulator_a": "secret"},
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(NewHTTPHandler(service, store, "internal-secret"))
	defer ts.Close()
	body := bytes.NewBufferString(`{"operation_id":"op_1"}`)
	resp, err := http.Post(ts.URL+"/internal/v1/provider-operations", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHTTPClassifiesPersistenceFailuresAsRetryable(t *testing.T) {
	store := &faultStore{MemoryStore: NewMemoryStore(), createErr: errors.New("database unavailable"), webhookErr: errors.New("database unavailable")}
	service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})
	handler := NewHTTPHandler(service, store, "internal-secret")

	operationBody, _ := json.Marshal(testOperationRequest("op_persistence", "provider_a"))
	operationRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/provider-operations", bytes.NewReader(operationBody))
	operationRequest.Header.Set("X-Internal-Token", "internal-secret")
	operationResponse := httptest.NewRecorder()
	handler.ServeHTTP(operationResponse, operationRequest)
	if operationResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("operation persistence failure status=%d body=%s", operationResponse.Code, operationResponse.Body.String())
	}

	event := map[string]any{
		"event_id": "evt_persistence", "provider": "provider_a", "operation_id": "op_persistence",
		"provider_reference": "provider_ref", "status": "captured", "occurred_at": time.Now().UTC(),
	}
	webhookBody, _ := json.Marshal(event)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(webhookBody)
	webhookRequest := httptest.NewRequest(http.MethodPost, "/webhooks/provider_a", bytes.NewReader(webhookBody))
	webhookRequest.Header.Set("X-Provider-Signature", hex.EncodeToString(mac.Sum(nil)))
	webhookResponse := httptest.NewRecorder()
	handler.ServeHTTP(webhookResponse, webhookRequest)
	if webhookResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("webhook persistence failure status=%d body=%s", webhookResponse.Code, webhookResponse.Body.String())
	}
}

func TestProductionRejectsSimulatorProvider(t *testing.T) {
	_, err := NewService(Config{
		Environment: "production", ProviderKinds: map[string]string{"simulator_a": "simulator_a"},
		ProviderConnectionRevisions: map[string]string{"simulator_a": "simulator_a_v1"}, ProviderNamespaceIDs: map[string]string{"simulator_a": "simulator_a"},
		ProviderEndpoints: map[string]string{"simulator_a": "https://example.invalid"},
		WebhookBaseURL:    "https://gateway.example.invalid", WebhookSecrets: map[string]string{"simulator_a": "secret"},
	}, NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected production simulator configuration to be rejected")
	}
}

func TestConflictingTerminalWebhookRequiresReview(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _, err := store.CreateOrGet(context.Background(), Operation{
		OperationID: "op_conflict", AttemptID: "attempt_1", ProviderConnectionID: "provider_a",
		ProviderConnectionRevision: "provider_a_v1", ProviderNamespaceID: "provider_a", Provider: "provider_a",
		ProviderIdempotencyKey: "op_conflict", RequestHash: "hash", RequestHashVersion: requestHashVersionConnection,
		LegacyRequestHash: "legacy_hash", Status: StatusFailed, SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(ProviderOutcome{
		EventID: "evt_late_success", OperationID: "op_conflict", ProviderConnectionID: "provider_a", Provider: "provider_a", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_late", OccurredAt: now,
	}))
	if err != nil || !inserted {
		t.Fatalf("apply webhook: inserted=%v err=%v", inserted, err)
	}
	if outcome.Status != StatusReview || outcome.SideEffectCertainty != "possible" {
		t.Fatalf("expected review_required, got %+v", outcome)
	}
}

func TestUnknownTransportEvidenceCannotDowngradeSuccess(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _, err := store.CreateOrGet(context.Background(), Operation{
		OperationID: "op_monotonic", AttemptID: "attempt_1", ProviderConnectionID: "provider_a",
		ProviderConnectionRevision: "provider_a_v1", ProviderNamespaceID: "provider_a", Provider: "provider_a",
		ProviderIdempotencyKey: "op_monotonic", RequestHash: "hash", RequestHashVersion: requestHashVersionConnection,
		LegacyRequestHash: "legacy_hash", Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := store.SetStatus(context.Background(), "op_monotonic", StatusUnknown, "possible", "", "provider_response_unknown")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusSucceeded || op.SideEffectCertainty != "confirmed" || op.FailureCode != "" {
		t.Fatalf("less-certain evidence downgraded a conclusive success: %+v", op)
	}
}

func TestLateAuthorizationWebhookCannotDowngradeCapturedSuccess(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _, err := store.CreateOrGet(context.Background(), Operation{
		OperationID: "op_captured", AttemptID: "attempt_captured", ProviderConnectionID: "provider_a",
		ProviderConnectionRevision: "provider_a_v1", ProviderNamespaceID: "provider_a", Provider: "provider_a",
		ProviderIdempotencyKey: "op_captured", RequestHash: "hash", RequestHashVersion: requestHashVersionConnection,
		LegacyRequestHash: "legacy_hash", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_ref", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	lateAuthorization := ProviderOutcome{
		EventID: "evt_late_authorized", OperationID: "op_captured", ProviderConnectionID: "provider_a", Provider: "provider_a",
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "provider_ref", OccurredAt: now,
	}
	outcome, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(lateAuthorization))
	if err != nil || !inserted {
		t.Fatalf("late authorization: inserted=%v err=%v", inserted, err)
	}
	if outcome.Status != StatusSucceeded || outcome.SideEffectCertainty != "confirmed" {
		t.Fatalf("late authorization downgraded capture: %+v", outcome)
	}
	if _, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(lateAuthorization)); err != nil || inserted {
		t.Fatalf("duplicate late authorization: inserted=%v err=%v", inserted, err)
	}
}

func TestAuthorizationAndConclusiveFailureRequireReviewInEitherOrder(t *testing.T) {
	t.Run("decline then authorization", func(t *testing.T) {
		store := NewMemoryStore()
		input := testOperationRequest("op_decline_then_auth", "provider_a")
		seedOperation(t, store, input, StatusSending)
		if _, err := store.SetStatus(context.Background(), input.OperationID, StatusFailed, "none", "", "do_not_honor"); err != nil {
			t.Fatal(err)
		}
		service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})
		inserted, err := applySignedWebhook(t, service, map[string]any{
			"event_id": "evt_late_auth_after_decline", "provider": "provider_a", "operation_id": input.OperationID,
			"provider_reference": "auth_ref_late", "status": "authorized", "occurred_at": time.Now().UTC(),
		})
		if err != nil || !inserted {
			t.Fatalf("late authorization: inserted=%v err=%v", inserted, err)
		}
		op, err := store.Get(context.Background(), input.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != StatusReview || op.SideEffectCertainty != "possible" || op.FailureCode != "authorization_failure_conflict" {
			t.Fatalf("late authorization was hidden behind decline: %+v", op)
		}
	})

	t.Run("authorization then decline", func(t *testing.T) {
		store := NewMemoryStore()
		input := testOperationRequest("op_auth_then_decline", "provider_a")
		seedOperation(t, store, input, StatusSending)
		service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})
		inserted, err := applySignedWebhook(t, service, map[string]any{
			"event_id": "evt_auth_first", "provider": "provider_a", "operation_id": input.OperationID,
			"provider_reference": "auth_ref_first", "status": "authorized", "occurred_at": time.Now().UTC(),
		})
		if err != nil || !inserted {
			t.Fatalf("authorization: inserted=%v err=%v", inserted, err)
		}
		inserted, err = applySignedWebhook(t, service, map[string]any{
			"event_id": "evt_decline_after_auth", "provider": "provider_a", "operation_id": input.OperationID,
			"provider_reference": "auth_ref_first", "status": "declined", "failure_code": "do_not_honor", "occurred_at": time.Now().UTC(),
		})
		if err != nil || !inserted {
			t.Fatalf("decline after authorization: inserted=%v err=%v", inserted, err)
		}
		op, err := store.Get(context.Background(), input.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != StatusReview || op.SideEffectCertainty != "possible" || op.FailureCode != "authorization_failure_conflict" {
			t.Fatalf("decline erased authorization evidence: %+v", op)
		}
	})
}

func TestConflictingProviderReferenceRequiresReview(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _, err := store.CreateOrGet(context.Background(), Operation{
		OperationID: "op_reference", AttemptID: "attempt_reference", ProviderConnectionID: "provider_a",
		ProviderConnectionRevision: "provider_a_v1", ProviderNamespaceID: "provider_a", Provider: "provider_a",
		ProviderIdempotencyKey: "op_reference", RequestHash: "hash", RequestHashVersion: requestHashVersionConnection,
		LegacyRequestHash: "legacy_hash", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_a", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(ProviderOutcome{
		EventID: "evt_reference_conflict", OperationID: "op_reference", ProviderConnectionID: "provider_a", Provider: "provider_a",
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_b", OccurredAt: now,
	}))
	if err != nil || !inserted {
		t.Fatalf("reference conflict: inserted=%v err=%v", inserted, err)
	}
	if outcome.Status != StatusReview || outcome.SideEffectCertainty != "possible" || outcome.ProviderReference != "provider_ref_a" {
		t.Fatalf("reference conflict was not quarantined: %+v", outcome)
	}
	if outcome.FailureCode != "provider_reference_conflict" {
		t.Fatalf("reference conflict failure code = %q", outcome.FailureCode)
	}
	claimKey := providerReferenceKey{NamespaceID: "provider_a", Reference: "provider_ref_b"}
	if owner := store.referenceClaims[claimKey]; owner != "op_reference" {
		t.Fatalf("replacement reference claim owner = %q, want op_reference", owner)
	}
	if _, exists := store.referenceObservations[providerReferenceObservationKey{Reference: claimKey, OperationID: "op_reference"}]; !exists {
		t.Fatal("replacement reference evidence was not preserved")
	}
}

func TestCreatedOperationResubmitsTheExactDurableOperation(t *testing.T) {
	var postCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected provider method %s", r.Method)
		}
		postCalls.Add(1)
		if key := r.Header.Get("Idempotency-Key"); key != "op_created" {
			t.Errorf("provider idempotency key = %q, want op_created", key)
		}
		writeJSON(w, http.StatusOK, providerResponse{
			Provider: "provider_a", OperationID: "op_created", ProviderReference: "provider_created", Status: "captured",
		})
	}))
	defer provider.Close()

	input := testOperationRequest("op_created", "provider_a")
	store := NewMemoryStore()
	seedOperation(t, store, input, StatusCreated)
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})

	op, err := service.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusSucceeded || op.ProviderIdempotencyKey != input.OperationID || postCalls.Load() != 1 {
		t.Fatalf("created operation did not safely resume: calls=%d operation=%+v", postCalls.Load(), op)
	}
}

func TestExistingSendingOperationQueriesBeforeAnyResubmission(t *testing.T) {
	var getCalls atomic.Int32
	var postCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCalls.Add(1)
			writeJSON(w, http.StatusOK, providerResponse{
				Provider: "provider_a", OperationID: "op_sending", ProviderReference: "provider_existing", Status: "captured",
			})
		case http.MethodPost:
			postCalls.Add(1)
			writeJSON(w, http.StatusOK, providerResponse{Provider: "provider_a", OperationID: "op_sending", Status: "captured"})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer provider.Close()

	input := testOperationRequest("op_sending", "provider_a")
	store := NewMemoryStore()
	seedOperation(t, store, input, StatusSending)
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
	op, err := service.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusSucceeded || getCalls.Load() != 1 || postCalls.Load() != 0 {
		t.Fatalf("sending recovery must query without blind resubmission: get=%d post=%d operation=%+v", getCalls.Load(), postCalls.Load(), op)
	}
}

func TestUnknownOperationResubmitsSameKeyOnlyAfterConclusiveNotFound(t *testing.T) {
	var getCalls atomic.Int32
	var postCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCalls.Add(1)
			writeError(w, http.StatusNotFound, "operation_not_found", "not found")
		case http.MethodPost:
			postCalls.Add(1)
			if key := r.Header.Get("Idempotency-Key"); key != "op_unknown" {
				t.Errorf("provider idempotency key = %q, want op_unknown", key)
			}
			writeJSON(w, http.StatusOK, providerResponse{
				Provider: "provider_a", OperationID: "op_unknown", ProviderReference: "provider_resubmitted", Status: "captured",
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer provider.Close()

	input := testOperationRequest("op_unknown", "provider_a")
	store := NewMemoryStore()
	seedOperation(t, store, input, StatusUnknown)
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
	op, err := service.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusSucceeded || getCalls.Load() != 1 || postCalls.Load() != 1 {
		t.Fatalf("not-found recovery did not use query then same-key submission: get=%d post=%d operation=%+v", getCalls.Load(), postCalls.Load(), op)
	}
}

func TestCreatedOperationReconcileSignalsPaymentToResubmit(t *testing.T) {
	input := testOperationRequest("op_created_query", "provider_a")
	store := NewMemoryStore()
	seedOperation(t, store, input, StatusCreated)
	service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})

	if _, err := service.Get(context.Background(), input.OperationID, true); !errors.Is(err, ErrResubmitRequired) {
		t.Fatalf("created reconcile error = %v, want ErrResubmitRequired", err)
	}
	handler := NewHTTPHandler(service, store, "internal-secret")
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/provider-operations/"+input.OperationID+"?reconcile=true", nil)
	request.Header.Set("X-Internal-Token", "internal-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "operation_resubmit_required") {
		t.Fatalf("HTTP reconcile status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRecoveryRejectsDifferentPayloadForSameOperation(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()

	input := testOperationRequest("op_hash", "provider_a")
	store := NewMemoryStore()
	seedOperation(t, store, input, StatusCreated)
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
	input.AmountMinor++
	if _, err := service.Execute(context.Background(), input); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload error = %v, want ErrIdempotencyConflict", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider received %d calls for a mismatched replay", calls.Load())
	}
}

func TestDurableStatusWritesDetachFromCanceledRequest(t *testing.T) {
	store := &contextCheckingStore{MemoryStore: NewMemoryStore()}
	service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op, err := service.Execute(ctx, testOperationRequest("op_canceled", "provider_a"))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown {
		t.Fatalf("canceled provider request must remain UNKNOWN, got %+v", op)
	}
	if store.sawCanceled.Load() {
		t.Fatal("durable SetStatus reused a canceled request context")
	}
}

func TestAuthorizedResponseDoesNotBecomeCapturedSuccess(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, providerResponse{Provider: "provider_a", OperationID: "op_authorized", ProviderReference: "auth_only", Status: "authorized"})
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
	op, err := service.Execute(context.Background(), testOperationRequest("op_authorized", "provider_a"))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown || op.SideEffectCertainty != "possible" {
		t.Fatalf("authorization-only response was treated as captured: %+v", op)
	}
}

func TestAuthorizationWithoutReferenceRequiresReview(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, providerResponse{Provider: "provider_a", OperationID: "op_authorized_without_ref", Status: "authorized"})
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
	op, err := service.Execute(context.Background(), testOperationRequest("op_authorized_without_ref", "provider_a"))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusReview || op.SideEffectCertainty != "possible" || op.FailureCode != "authorization_reference_missing" {
		t.Fatalf("reference-less authorization was accepted: %+v", op)
	}
}

func TestCapturedResponseWithoutProviderReferenceRequiresReview(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, providerResponse{Provider: "provider_a", OperationID: "op_missing_reference", Status: "captured"})
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
	op, err := service.Execute(context.Background(), testOperationRequest("op_missing_reference", "provider_a"))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusReview || op.SideEffectCertainty != "possible" || op.FailureCode != "provider_reference_missing" {
		t.Fatalf("reference-less capture was accepted: %+v", op)
	}
}

func TestProviderResponseIdentityMismatchRequiresReview(t *testing.T) {
	t.Run("sale", func(t *testing.T) {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, providerResponse{Provider: "provider_b", OperationID: "op_other", Status: "captured"})
		}))
		defer provider.Close()
		store := NewMemoryStore()
		service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
		op, err := service.Execute(context.Background(), testOperationRequest("op_identity_sale", "provider_a"))
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != StatusReview || op.SideEffectCertainty != "possible" {
			t.Fatalf("mismatched sale response did not require review: %+v", op)
		}
	})

	t.Run("query", func(t *testing.T) {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, providerResponse{Provider: "provider_a", OperationID: "op_wrong", Status: "captured"})
		}))
		defer provider.Close()
		input := testOperationRequest("op_identity_query", "provider_a")
		store := NewMemoryStore()
		seedOperation(t, store, input, StatusSending)
		service := newTestService(t, store, map[string]string{"provider_a": provider.URL})
		op, err := service.Get(context.Background(), input.OperationID, true)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != StatusReview || op.SideEffectCertainty != "possible" {
			t.Fatalf("mismatched query response did not require review: %+v", op)
		}
	})
}

func TestWebhookCannotMutateAnOperationOwnedByAnotherProvider(t *testing.T) {
	store := NewMemoryStore()
	input := testOperationRequest("op_provider_b", "provider_b")
	seedOperation(t, store, input, StatusUnknown)
	service, err := NewService(Config{
		Environment:                 "test",
		ProviderKinds:               map[string]string{"provider_a": "provider_a", "provider_b": "provider_b"},
		ProviderConnectionRevisions: map[string]string{"provider_a": "provider_a_v1", "provider_b": "provider_b_v1"},
		ProviderNamespaceIDs:        map[string]string{"provider_a": "provider_a", "provider_b": "provider_b"},
		ProviderEndpoints: map[string]string{
			"provider_a": "http://127.0.0.1:1",
			"provider_b": "http://127.0.0.1:2",
		},
		WebhookBaseURL: "http://gateway.test",
		WebhookSecrets: map[string]string{"provider_a": "secret-a", "provider_b": "secret-b"},
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"event_id": "evt_wrong_provider", "provider": "provider_a", "operation_id": input.OperationID,
		"provider_reference": "wrong_ref", "status": "captured", "occurred_at": time.Now().UTC(),
	}
	body, _ := json.Marshal(event)
	mac := hmac.New(sha256.New, []byte("secret-a"))
	_, _ = mac.Write(body)
	if inserted, err := service.ApplyWebhook(context.Background(), "provider_a", body, hex.EncodeToString(mac.Sum(nil))); !errors.Is(err, ErrNotFound) || inserted {
		t.Fatalf("cross-provider webhook inserted=%v err=%v, want ErrNotFound", inserted, err)
	}
	op, err := store.Get(context.Background(), input.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown || op.ProviderReference != "" {
		t.Fatalf("cross-provider webhook mutated operation: %+v", op)
	}
	items, err := store.PendingOutbox(context.Background(), 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-provider webhook wrote outbox items=%d err=%v", len(items), err)
	}
}

func TestAuthorizationOnlyWebhookRemainsUnknownUntilCapture(t *testing.T) {
	store := NewMemoryStore()
	input := testOperationRequest("op_authorization_webhook", "provider_a")
	seedOperation(t, store, input, StatusSending)
	service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})
	event := map[string]any{
		"event_id": "evt_authorized", "provider": "provider_a", "operation_id": input.OperationID,
		"provider_reference": "auth_ref", "status": "authorized", "occurred_at": time.Now().UTC(),
	}
	body, _ := json.Marshal(event)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	inserted, err := service.ApplyWebhook(context.Background(), "provider_a", body, hex.EncodeToString(mac.Sum(nil)))
	if err != nil || !inserted {
		t.Fatalf("authorization webhook: inserted=%v err=%v", inserted, err)
	}
	op, err := store.Get(context.Background(), input.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown || op.SideEffectCertainty != "possible" || op.ProviderReference != "auth_ref" {
		t.Fatalf("authorization-only webhook was treated as capture: %+v", op)
	}
}

func TestEnvironmentNormalizationAndProductionFailClosed(t *testing.T) {
	for input, expected := range map[string]string{
		" LOCAL ": "local", "Dev": "development", "Production": "production", "PROD": "production", "stage": "staging",
	} {
		actual, err := NormalizeEnvironment(input)
		if err != nil || actual != expected {
			t.Fatalf("NormalizeEnvironment(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	if _, err := NormalizeEnvironment("sandbox"); err == nil {
		t.Fatal("unknown environment must fail closed")
	}

	base := Config{
		Environment: "Production", ProviderKinds: map[string]string{"provider_a": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a": "provider_a_v1"}, ProviderNamespaceIDs: map[string]string{"provider_a": "provider_a"},
		ProviderEndpoints: map[string]string{"provider_a": "https://provider.example"},
		WebhookBaseURL:    "https://gateway.example", WebhookSecrets: map[string]string{"provider_a": "webhook-secret"},
	}
	if _, err := NewService(base, NewMemoryStore(), nil); err == nil || !strings.Contains(err.Error(), "internal service token") {
		t.Fatalf("production without explicit internal token error = %v", err)
	}
	base.InternalToken = "explicit-internal-token"
	if _, err := NewService(base, NewMemoryStore(), nil); err == nil || !strings.Contains(err.Error(), "payment outcome URL") {
		t.Fatalf("production without payment outcome URL error = %v", err)
	}
	base.PaymentOutcomeURL = "https://payment.internal/provider-outcomes"
	if _, err := NewService(base, NewMemoryStore(), nil); err != nil {
		t.Fatalf("explicit production configuration was rejected: %v", err)
	}
	base.ProviderEndpoints = map[string]string{"Simulator_A": "https://provider.example"}
	base.ProviderKinds = map[string]string{"Simulator_A": "Simulator_A"}
	base.ProviderConnectionRevisions = map[string]string{"Simulator_A": "Simulator_A_v1"}
	base.ProviderNamespaceIDs = map[string]string{"Simulator_A": "Simulator_A"}
	base.WebhookSecrets = map[string]string{"Simulator_A": "webhook-secret"}
	if _, err := NewService(base, NewMemoryStore(), nil); err == nil || !strings.Contains(err.Error(), "simulator") {
		t.Fatalf("production simulator configuration error = %v", err)
	}
}

func TestOutboxFailureDoesNotBlockLaterItems(t *testing.T) {
	var failedCalls atomic.Int32
	var successfulCalls atomic.Int32
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "outbox-internal-token" {
			t.Errorf("internal token = %q", r.Header.Get("X-Internal-Token"))
		}
		var outcome ProviderOutcome
		if err := json.NewDecoder(r.Body).Decode(&outcome); err != nil {
			t.Errorf("decode outcome: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if outcome.OperationID == "op_outbox_fail" {
			failedCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		successfulCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer payment.Close()

	store := NewMemoryStore()
	for _, operationID := range []string{"op_outbox_fail", "op_outbox_ok"} {
		input := testOperationRequest(operationID, "provider_a")
		seedOperation(t, store, input, StatusUnknown)
		_, inserted, err := store.ApplyWebhook(context.Background(), testWebhookEvent(ProviderOutcome{
			EventID: "evt_" + operationID, OperationID: operationID,
			ProviderConnectionID: "provider_a", Provider: "provider_a",
			Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "ref_" + operationID,
			OccurredAt: time.Now().UTC(),
		}))
		if err != nil || !inserted {
			t.Fatalf("seed outbox for %s: inserted=%v err=%v", operationID, inserted, err)
		}
	}
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: map[string]string{"provider_a": "provider_a"},
		ProviderConnectionRevisions: map[string]string{"provider_a": "provider_a_v1"}, ProviderNamespaceIDs: map[string]string{"provider_a": "provider_a"},
		ProviderEndpoints: map[string]string{"provider_a": "http://127.0.0.1:1"}, WebhookBaseURL: "http://gateway.test",
		WebhookSecrets: map[string]string{"provider_a": "test-secret"}, PaymentOutcomeURL: payment.URL, InternalToken: "outbox-internal-token",
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.dispatchOutboxBatch(context.Background())
	if failedCalls.Load() != 1 || successfulCalls.Load() != 1 {
		t.Fatalf("outbox calls: failed=%d successful=%d", failedCalls.Load(), successfulCalls.Load())
	}
	pending, err := store.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Outcome.OperationID != "op_outbox_fail" {
		t.Fatalf("pending outbox = %+v, want only failed item", pending)
	}
}

type contextCheckingStore struct {
	*MemoryStore
	sawCanceled atomic.Bool
}

type faultStore struct {
	*MemoryStore
	createErr  error
	webhookErr error
}

func (s *faultStore) CreateOrGet(ctx context.Context, operation Operation) (Operation, bool, error) {
	if s.createErr != nil {
		return Operation{}, false, s.createErr
	}
	return s.MemoryStore.CreateOrGet(ctx, operation)
}

func (s *faultStore) ApplyWebhook(ctx context.Context, event VerifiedWebhookEvent) (ProviderOutcome, bool, error) {
	if s.webhookErr != nil {
		return ProviderOutcome{}, false, s.webhookErr
	}
	return s.MemoryStore.ApplyWebhook(ctx, event)
}

func (s *contextCheckingStore) SetStatus(ctx context.Context, id, status, certainty, reference, failure string) (Operation, error) {
	if ctx.Err() != nil {
		s.sawCanceled.Store(true)
	}
	return s.MemoryStore.SetStatus(ctx, id, status, certainty, reference, failure)
}

func newTestService(t *testing.T, store Store, providers map[string]string) *Service {
	t.Helper()
	secrets := make(map[string]string, len(providers))
	providerKinds := make(map[string]string, len(providers))
	revisions := make(map[string]string, len(providers))
	namespaces := make(map[string]string, len(providers))
	for connectionID := range providers {
		secrets[connectionID] = "test-secret"
		providerKinds[connectionID] = connectionID
		revisions[connectionID] = connectionID + "_v1"
		namespaces[connectionID] = connectionID
	}
	service, err := NewService(Config{
		Environment: "test", ProviderKinds: providerKinds, ProviderConnectionRevisions: revisions,
		ProviderNamespaceIDs: namespaces, ProviderEndpoints: providers,
		WebhookBaseURL: "http://gateway.test", WebhookSecrets: secrets, ProviderTimeout: 500 * time.Millisecond,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testOperationRequest(operationID, provider string) OperationRequest {
	return OperationRequest{
		OperationID: operationID, AttemptID: "attempt_" + operationID,
		ProviderConnectionID: provider, ProviderConnectionRevision: provider + "_v1",
		ProviderNamespaceID: provider, Provider: provider,
		AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_sim_approved", CaptureMethod: "automatic",
	}
}

func seedOperation(t *testing.T, store Store, input OperationRequest, status string) Operation {
	t.Helper()
	now := time.Now().UTC()
	op := Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID,
		ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
		ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
		ProviderIdempotencyKey: input.OperationID, RequestHash: operationHashV2(input),
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: operationHashV1(input), Status: status,
		SideEffectCertainty: "possible", CreatedAt: now, UpdatedAt: now,
	}
	if status == StatusCreated {
		op.SideEffectCertainty = "none"
	}
	stored, created, err := store.CreateOrGet(context.Background(), op)
	if err != nil || !created {
		t.Fatalf("seed operation: created=%v operation=%+v err=%v", created, stored, err)
	}
	return stored
}

func testWebhookEvent(outcome ProviderOutcome) VerifiedWebhookEvent {
	if outcome.ProviderConnectionID == "" {
		outcome.ProviderConnectionID = outcome.Provider
	}
	if outcome.ProviderConnectionRevision == "" {
		outcome.ProviderConnectionRevision = outcome.ProviderConnectionID + "_v1"
	}
	if outcome.ProviderNamespaceID == "" {
		outcome.ProviderNamespaceID = outcome.Provider
	}
	identity := ProviderConnectionIdentity{
		Provider: outcome.Provider, ConnectionRevision: outcome.ProviderConnectionRevision, NamespaceID: outcome.ProviderNamespaceID,
	}
	return VerifiedWebhookEvent{
		PathConnectionID: outcome.ProviderConnectionID, PathIdentity: identity,
		AllowedOperationIdentities: map[string]ProviderConnectionIdentity{outcome.ProviderConnectionID: identity},
		EventID:                    outcome.EventID, OperationID: outcome.OperationID, Status: outcome.Status,
		SideEffectCertainty: outcome.SideEffectCertainty, ProviderReference: outcome.ProviderReference,
		FailureCode: outcome.FailureCode, OccurredAt: outcome.OccurredAt,
	}
}

func applySignedWebhook(t *testing.T, service *Service, event map[string]any) (bool, error) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	return service.ApplyWebhook(context.Background(), "provider_a", body, hex.EncodeToString(mac.Sum(nil)))
}
