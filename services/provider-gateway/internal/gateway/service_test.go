package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
			time.Sleep(80 * time.Millisecond)
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
		Environment: "test", ProviderEndpoints: map[string]string{"simulator_a": provider.URL},
		WebhookBaseURL: provider.URL, WebhookSecrets: map[string]string{"simulator_a": "secret"}, ProviderTimeout: 10 * time.Millisecond,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	input := OperationRequest{OperationID: "op_1", AttemptID: "attempt_1", Provider: "simulator_a", AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_sim_timeout_then_succeeded", CaptureMethod: "automatic"}
	op, err := service.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown || op.SideEffectCertainty != "possible" {
		t.Fatalf("timeout must be unknown/possible, got %+v", op)
	}
	time.Sleep(100 * time.Millisecond)
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

func TestWebhookIsVerifiedAndDeduplicated(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, providerResponse{Provider: "simulator_a", OperationID: "op_1", ProviderReference: "provider_1", Status: "succeeded"})
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service, err := NewService(Config{Environment: "test", ProviderEndpoints: map[string]string{"simulator_a": provider.URL}, WebhookBaseURL: provider.URL, WebhookSecrets: map[string]string{"simulator_a": "secret"}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(context.Background(), OperationRequest{OperationID: "op_1", AttemptID: "attempt_1", Provider: "simulator_a", AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_sim_approved", CaptureMethod: "automatic"})
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
			OperationID: operationID, AttemptID: "attempt_" + operationID, Provider: "provider_a",
			ProviderIdempotencyKey: operationID, RequestHash: "hash_" + operationID,
			Status: StatusUnknown, SideEffectCertainty: "possible", CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ProviderOutcome{
		EventID: "evt_reused", OperationID: "op_event_a", Provider: "provider_a",
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "ref_a", OccurredAt: now,
	}
	if _, inserted, err := store.ApplyWebhook(context.Background(), first); err != nil || !inserted {
		t.Fatalf("first event: inserted=%v err=%v", inserted, err)
	}
	conflict := first
	conflict.OperationID = "op_event_b"
	conflict.ProviderReference = "ref_b"
	if _, inserted, err := store.ApplyWebhook(context.Background(), conflict); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("reused event: inserted=%v err=%v, want ErrWebhookEventConflict", inserted, err)
	}
	op, err := store.Get(context.Background(), "op_event_b")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != StatusUnknown || op.ProviderReference != "" {
		t.Fatalf("reused event mutated the second operation: %+v", op)
	}
}

func TestHTTPRequiresInternalToken(t *testing.T) {
	store := NewMemoryStore()
	service, _ := NewService(Config{Environment: "test", ProviderEndpoints: map[string]string{"simulator_a": "http://127.0.0.1"}, WebhookBaseURL: "http://127.0.0.1", WebhookSecrets: map[string]string{"simulator_a": "secret"}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		Environment:       "production",
		ProviderEndpoints: map[string]string{"simulator_a": "https://example.invalid"},
		WebhookBaseURL:    "https://gateway.example.invalid",
		WebhookSecrets:    map[string]string{"simulator_a": "secret"},
	}, NewMemoryStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected production simulator configuration to be rejected")
	}
}

func TestConflictingTerminalWebhookRequiresReview(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, _, err := store.CreateOrGet(context.Background(), Operation{
		OperationID: "op_conflict", AttemptID: "attempt_1", Provider: "provider_a", ProviderIdempotencyKey: "op_conflict",
		RequestHash: "hash", Status: StatusFailed, SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, inserted, err := store.ApplyWebhook(context.Background(), ProviderOutcome{
		EventID: "evt_late_success", OperationID: "op_conflict", Provider: "provider_a", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_late", OccurredAt: now,
	})
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
		OperationID: "op_monotonic", AttemptID: "attempt_1", Provider: "provider_a", ProviderIdempotencyKey: "op_monotonic",
		RequestHash: "hash", Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref",
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
		OperationID: "op_captured", AttemptID: "attempt_captured", Provider: "provider_a",
		ProviderIdempotencyKey: "op_captured", RequestHash: "hash", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_ref", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	lateAuthorization := ProviderOutcome{
		EventID: "evt_late_authorized", OperationID: "op_captured", Provider: "provider_a",
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "provider_ref", OccurredAt: now,
	}
	outcome, inserted, err := store.ApplyWebhook(context.Background(), lateAuthorization)
	if err != nil || !inserted {
		t.Fatalf("late authorization: inserted=%v err=%v", inserted, err)
	}
	if outcome.Status != StatusSucceeded || outcome.SideEffectCertainty != "confirmed" {
		t.Fatalf("late authorization downgraded capture: %+v", outcome)
	}
	if _, inserted, err := store.ApplyWebhook(context.Background(), lateAuthorization); err != nil || inserted {
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
		OperationID: "op_reference", AttemptID: "attempt_reference", Provider: "provider_a",
		ProviderIdempotencyKey: "op_reference", RequestHash: "hash", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_a", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, inserted, err := store.ApplyWebhook(context.Background(), ProviderOutcome{
		EventID: "evt_reference_conflict", OperationID: "op_reference", Provider: "provider_a",
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_b", OccurredAt: now,
	})
	if err != nil || !inserted {
		t.Fatalf("reference conflict: inserted=%v err=%v", inserted, err)
	}
	if outcome.Status != StatusReview || outcome.SideEffectCertainty != "possible" || outcome.ProviderReference != "provider_ref_a" {
		t.Fatalf("reference conflict was not quarantined: %+v", outcome)
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
		Environment: "test",
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
		Environment:       "Production",
		ProviderEndpoints: map[string]string{"provider_a": "https://provider.example"},
		WebhookBaseURL:    "https://gateway.example",
		WebhookSecrets:    map[string]string{"provider_a": "webhook-secret"},
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
		_, inserted, err := store.ApplyWebhook(context.Background(), ProviderOutcome{
			EventID: "evt_" + operationID, OperationID: operationID, Provider: "provider_a",
			Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "ref_" + operationID,
			OccurredAt: time.Now().UTC(),
		})
		if err != nil || !inserted {
			t.Fatalf("seed outbox for %s: inserted=%v err=%v", operationID, inserted, err)
		}
	}
	service, err := NewService(Config{
		Environment:       "test",
		ProviderEndpoints: map[string]string{"provider_a": "http://127.0.0.1:1"},
		WebhookBaseURL:    "http://gateway.test",
		WebhookSecrets:    map[string]string{"provider_a": "test-secret"},
		PaymentOutcomeURL: payment.URL,
		InternalToken:     "outbox-internal-token",
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

func (s *faultStore) ApplyWebhook(ctx context.Context, outcome ProviderOutcome) (ProviderOutcome, bool, error) {
	if s.webhookErr != nil {
		return ProviderOutcome{}, false, s.webhookErr
	}
	return s.MemoryStore.ApplyWebhook(ctx, outcome)
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
	for provider := range providers {
		secrets[provider] = "test-secret"
	}
	service, err := NewService(Config{
		Environment:       "test",
		ProviderEndpoints: providers,
		WebhookBaseURL:    "http://gateway.test",
		WebhookSecrets:    secrets,
		ProviderTimeout:   500 * time.Millisecond,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testOperationRequest(operationID, provider string) OperationRequest {
	return OperationRequest{
		OperationID: operationID, AttemptID: "attempt_" + operationID, Provider: provider,
		AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_sim_approved", CaptureMethod: "automatic",
	}
}

func seedOperation(t *testing.T, store Store, input OperationRequest, status string) Operation {
	t.Helper()
	now := time.Now().UTC()
	op := Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID, Provider: input.Provider,
		ProviderIdempotencyKey: input.OperationID, RequestHash: operationHash(input), Status: status,
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
