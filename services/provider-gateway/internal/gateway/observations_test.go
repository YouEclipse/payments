package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderObservationLedgerCoversSaleQueryAndWebhook(t *testing.T) {
	t.Run("sale", func(t *testing.T) {
		input := testOperationRequest("op_observation_sale", "provider_a")
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("provider method=%s, want POST", r.Method)
			}
			writeJSON(w, http.StatusOK, providerResponse{
				Provider: input.Provider, OperationID: input.OperationID,
				ProviderReference: "sale_reference", Status: "captured",
			})
		}))
		defer provider.Close()
		store := NewMemoryStore()
		service := newTestService(t, store, map[string]string{input.ProviderConnectionID: provider.URL})
		operation, err := service.Execute(t.Context(), input)
		if err != nil || operation.Status != StatusSucceeded {
			t.Fatalf("sale operation=%+v err=%v", operation, err)
		}
		observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
		if err != nil || len(observations) != 1 {
			t.Fatalf("sale observations=%+v err=%v", observations, err)
		}
		requireProviderObservation(t, observations[0], ObservationSourceSale, "captured", StatusSucceeded, "")
	})

	t.Run("query", func(t *testing.T) {
		input := testOperationRequest("op_observation_query", "provider_a")
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Fatalf("provider method=%s, want GET", r.Method)
			}
			writeJSON(w, http.StatusOK, providerResponse{
				Provider: input.Provider, OperationID: input.OperationID,
				ProviderReference: "query_reference", Status: "captured",
			})
		}))
		defer provider.Close()
		store := NewMemoryStore()
		seedOperation(t, store, input, StatusSending)
		service := newTestService(t, store, map[string]string{input.ProviderConnectionID: provider.URL})
		operation, err := service.Execute(t.Context(), input)
		if err != nil || operation.Status != StatusSucceeded {
			t.Fatalf("query operation=%+v err=%v", operation, err)
		}
		observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
		if err != nil || len(observations) != 1 {
			t.Fatalf("query observations=%+v err=%v", observations, err)
		}
		requireProviderObservation(t, observations[0], ObservationSourceQuery, "captured", StatusSucceeded, "")
	})

	t.Run("webhook duplicate deliveries remain evidence", func(t *testing.T) {
		input := testOperationRequest("op_observation_webhook", "provider_a")
		store := NewMemoryStore()
		seedOperation(t, store, input, StatusSending)
		service := newTestService(t, store, map[string]string{input.ProviderConnectionID: "http://127.0.0.1:1"})
		occurredAt := time.Now().UTC()
		body := mustJSON(map[string]any{
			"event_id": "event_observation_webhook", "provider": input.Provider,
			"operation_id": input.OperationID, "provider_reference": "webhook_reference",
			"status": "captured", "occurred_at": occurredAt,
		})
		mac := hmac.New(sha256.New, []byte("test-secret"))
		_, _ = mac.Write(body)
		signature := hex.EncodeToString(mac.Sum(nil))
		if inserted, err := service.ApplyWebhook(t.Context(), input.ProviderConnectionID, body, signature); err != nil || !inserted {
			t.Fatalf("first webhook inserted=%v err=%v", inserted, err)
		}
		if inserted, err := service.ApplyWebhook(t.Context(), input.ProviderConnectionID, body, signature); err != nil || inserted {
			t.Fatalf("duplicate webhook inserted=%v err=%v", inserted, err)
		}
		observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
		if err != nil || len(observations) != 2 {
			t.Fatalf("webhook observations=%+v err=%v", observations, err)
		}
		for _, observation := range observations {
			requireProviderObservation(t, observation, ObservationSourceWebhook, "captured", StatusSucceeded, "event_observation_webhook")
		}

		semanticConflictBody := mustJSON(map[string]any{
			"event_id": "event_observation_webhook", "provider": input.Provider,
			"operation_id": input.OperationID, "provider_reference": "webhook_reference",
			"status": "succeeded", "occurred_at": occurredAt,
		})
		mac = hmac.New(sha256.New, []byte("test-secret"))
		_, _ = mac.Write(semanticConflictBody)
		if inserted, err := service.ApplyWebhook(
			t.Context(), input.ProviderConnectionID, semanticConflictBody, hex.EncodeToString(mac.Sum(nil)),
		); !errors.Is(err, ErrWebhookEventConflict) || inserted {
			t.Fatalf("same normalized outcome with different raw payload inserted=%v err=%v", inserted, err)
		}
		observations, err = store.ListProviderObservations(t.Context(), input.OperationID)
		if err != nil || len(observations) != 3 || observations[2].RawProviderStatus != "succeeded" {
			t.Fatalf("semantic conflict observations=%+v err=%v", observations, err)
		}
	})
}

func TestProviderErrorObservationStoresOnlyBoundedDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
		category   string
	}{
		{name: "non_success", statusCode: http.StatusBadGateway, body: `{"payment_method_token":"must-not-persist"}`, category: "http_error"},
		{name: "malformed_success", statusCode: http.StatusOK, body: `{"provider":`, category: "malformed_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := testOperationRequest("op_observation_diagnostic_"+test.name, "provider_a")
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer provider.Close()
			store := NewMemoryStore()
			service := newTestService(t, store, map[string]string{input.ProviderConnectionID: provider.URL})
			operation, err := service.Execute(t.Context(), input)
			if err != nil || operation.Status != StatusUnknown {
				t.Fatalf("diagnostic operation=%+v err=%v", operation, err)
			}
			observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
			if err != nil || len(observations) != 1 {
				t.Fatalf("diagnostic observations=%+v err=%v", observations, err)
			}
			var diagnostic map[string]any
			if err := json.Unmarshal(observations[0].SanitizedPayload, &diagnostic); err != nil {
				t.Fatal(err)
			}
			bodyHash := sha256.Sum256([]byte(test.body))
			if diagnostic["category"] != test.category ||
				diagnostic["status_code"] != float64(test.statusCode) ||
				diagnostic["byte_count"] != float64(len(test.body)) ||
				diagnostic["body_sha256"] != hex.EncodeToString(bodyHash[:]) {
				t.Fatalf("diagnostic payload=%v", diagnostic)
			}
			if strings.Contains(string(observations[0].SanitizedPayload), "must-not-persist") {
				t.Fatalf("raw Provider body reached evidence ledger: %s", observations[0].SanitizedPayload)
			}
		})
	}
}

func TestOversizedProviderErrorObservationNamesObservedPrefix(t *testing.T) {
	body := strings.Repeat("x", maxProviderResponseBytes+32)
	input := testOperationRequest("op_observation_diagnostic_oversized", "provider_a")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer provider.Close()
	store := NewMemoryStore()
	service := newTestService(t, store, map[string]string{input.ProviderConnectionID: provider.URL})
	operation, err := service.Execute(t.Context(), input)
	if err != nil || operation.Status != StatusUnknown {
		t.Fatalf("diagnostic operation=%+v err=%v", operation, err)
	}
	observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
	if err != nil || len(observations) != 1 {
		t.Fatalf("diagnostic observations=%+v err=%v", observations, err)
	}
	var diagnostic map[string]any
	if err := json.Unmarshal(observations[0].SanitizedPayload, &diagnostic); err != nil {
		t.Fatal(err)
	}
	observedPrefix := []byte(body)[:maxProviderResponseBytes+1]
	prefixHash := sha256.Sum256(observedPrefix)
	if diagnostic["category"] != "oversized_response" ||
		diagnostic["truncated"] != true ||
		diagnostic["observed_prefix_byte_count"] != float64(len(observedPrefix)) ||
		diagnostic["observed_prefix_sha256"] != hex.EncodeToString(prefixHash[:]) {
		t.Fatalf("diagnostic payload=%v", diagnostic)
	}
	if _, ok := diagnostic["body_sha256"]; ok {
		t.Fatalf("truncated diagnostic claims a whole-body hash: %v", diagnostic)
	}
	if _, ok := diagnostic["byte_count"]; ok {
		t.Fatalf("truncated diagnostic claims a whole-body byte count: %v", diagnostic)
	}
}

func TestWebhookBoundaryRejectsPayloadThatCannotEnterEvidenceLedger(t *testing.T) {
	store := NewMemoryStore()
	service := newTestService(t, store, map[string]string{"provider_a": "http://127.0.0.1:1"})
	handler := NewHTTPHandler(service, store, "internal-secret")
	request := httptest.NewRequest(
		http.MethodPost,
		"/webhooks/provider_a",
		strings.NewReader(strings.Repeat("x", maxWebhookBodyBytes+1)),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized webhook status=%d body=%s", response.Code, response.Body.String())
	}

	occurredAt := time.Now().UTC()
	body := mustJSON(map[string]any{
		"event_id": strings.Repeat("e", 257), "provider": "provider_a",
		"operation_id": "op_bounded_webhook", "status": "captured", "occurred_at": occurredAt,
	})
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	if inserted, err := service.ApplyWebhook(t.Context(), "provider_a", body, hex.EncodeToString(mac.Sum(nil))); !errors.Is(err, ErrInvalidWebhook) || inserted {
		t.Fatalf("overlong webhook event ID inserted=%v err=%v", inserted, err)
	}
}

func TestStaleNonterminalObservationIsAppendOnlyButGenerationFenced(t *testing.T) {
	store := NewMemoryStore()
	input := testOperationRequest("op_observation_generation_fence", "provider_a")
	created := seedOperation(t, store, input, StatusCreated)
	first, claimed, err := store.ClaimDispatch(t.Context(), created, time.Second)
	if err != nil || !claimed {
		t.Fatalf("first claim=%+v claimed=%v err=%v", first, claimed, err)
	}
	store.mu.Lock()
	store.dispatchLeases[input.OperationID] = time.Now().UTC().Add(-time.Second)
	store.mu.Unlock()
	second, claimed, err := store.ClaimDispatch(t.Context(), first, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("second claim=%+v claimed=%v err=%v", second, claimed, err)
	}

	late := providerResponseObservation(input.OperationID, ObservationSourceQuery, providerResponse{
		Provider: input.Provider, OperationID: input.OperationID,
		ProviderReference: "late_authorization", Status: "authorized",
	}, StatusUnknown, "possible", "late_authorization", "authorization_only")
	current, applied, err := store.ApplyProviderObservation(t.Context(), late, &first)
	if err != nil || applied || current.Status != StatusSending || !current.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("stale observation current=%+v applied=%v err=%v", current, applied, err)
	}
	if active, err := store.DispatchInFlight(t.Context(), input.OperationID); err != nil || !active {
		t.Fatalf("stale observation cleared newer lease: active=%v err=%v", active, err)
	}
	observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
	if err != nil || len(observations) != 1 {
		t.Fatalf("stale observation ledger=%+v err=%v", observations, err)
	}
	requireProviderObservation(t, observations[0], ObservationSourceQuery, "authorized", StatusUnknown, "")

	terminal := providerResponseObservation(input.OperationID, ObservationSourceQuery, providerResponse{
		Provider: input.Provider, OperationID: input.OperationID,
		ProviderReference: "captured_reference", Status: "captured",
	}, StatusSucceeded, "confirmed", "captured_reference", "")
	current, applied, err = store.ApplyProviderObservation(t.Context(), terminal, nil)
	if err != nil || !applied || current.Status != StatusSucceeded {
		t.Fatalf("terminal observation current=%+v applied=%v err=%v", current, applied, err)
	}
	if active, err := store.DispatchInFlight(t.Context(), input.OperationID); err != nil || active {
		t.Fatalf("terminal observation retained lease: active=%v err=%v", active, err)
	}
}

func TestAuthorizedWebhookCannotClearActiveDispatchLease(t *testing.T) {
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

	input := testOperationRequest("op_webhook_active_lease", "provider_a")
	store := NewMemoryStore()
	created := seedOperation(t, store, input, StatusCreated)
	claimed, won, err := store.ClaimDispatch(t.Context(), created, time.Minute)
	if err != nil || !won {
		t.Fatalf("active webhook claim=%+v won=%v err=%v", claimed, won, err)
	}
	service := newTestService(t, store, map[string]string{input.ProviderConnectionID: provider.URL})
	occurredAt := time.Now().UTC()
	body := mustJSON(map[string]any{
		"event_id": "event_active_lease_authorized", "provider": input.Provider,
		"operation_id": input.OperationID, "provider_reference": "active_lease_authorization",
		"status": "authorized", "occurred_at": occurredAt,
	})
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	inserted, err := service.ApplyWebhook(t.Context(), input.ProviderConnectionID, body, hex.EncodeToString(mac.Sum(nil)))
	if err != nil || !inserted {
		t.Fatalf("active webhook inserted=%v err=%v", inserted, err)
	}
	afterWebhook, err := store.Get(t.Context(), input.OperationID)
	if err != nil || afterWebhook.Status != StatusSending || !afterWebhook.UpdatedAt.Equal(claimed.UpdatedAt) {
		t.Fatalf("authorized webhook changed active generation: operation=%+v err=%v", afterWebhook, err)
	}
	if active, err := store.DispatchInFlight(t.Context(), input.OperationID); err != nil || !active {
		t.Fatalf("authorized webhook cleared active lease: active=%v err=%v", active, err)
	}
	observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
	if err != nil || len(observations) != 1 || observations[0].NormalizedStatus != StatusUnknown {
		t.Fatalf("authorized webhook evidence=%+v err=%v", observations, err)
	}

	concurrent, err := service.Execute(t.Context(), input)
	if err != nil || concurrent.Status != StatusSending || !concurrent.UpdatedAt.Equal(claimed.UpdatedAt) {
		t.Fatalf("concurrent execution=%+v err=%v", concurrent, err)
	}
	if getCalls.Load() != 0 || postCalls.Load() != 0 {
		t.Fatalf("active lease admitted Provider calls: get=%d post=%d", getCalls.Load(), postCalls.Load())
	}
}

func TestFutureCreatedOperationKeepsMonotonicTerminalTimestamp(t *testing.T) {
	store := NewMemoryStore()
	input := testOperationRequest("op_future_terminal_observation", "provider_a")
	future := time.Now().UTC().Add(5 * time.Minute)
	seed := Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID,
		ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
		ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
		ProviderIdempotencyKey: input.OperationID, RequestHash: operationHashV2(input),
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: operationHashV1(input),
		Status: StatusCreated, SideEffectCertainty: "none", CreatedAt: future, UpdatedAt: future,
	}
	created, inserted, err := store.CreateOrGet(t.Context(), seed)
	if err != nil || !inserted {
		t.Fatalf("future operation inserted=%v err=%v", inserted, err)
	}
	claimed, won, err := store.ClaimDispatch(t.Context(), created, time.Minute)
	if err != nil || !won {
		t.Fatalf("future claim=%+v won=%v err=%v", claimed, won, err)
	}
	terminal := providerResponseObservation(input.OperationID, ObservationSourceSale, providerResponse{
		Provider: input.Provider, OperationID: input.OperationID, ProviderReference: "future_terminal_reference", Status: "captured",
	}, StatusSucceeded, "confirmed", "future_terminal_reference", "")
	completed, applied, err := store.ApplyProviderObservation(t.Context(), terminal, nil)
	if err != nil || !applied || completed.Status != StatusSucceeded ||
		completed.UpdatedAt.Before(completed.CreatedAt) || !completed.UpdatedAt.After(claimed.UpdatedAt) {
		t.Fatalf("future terminal operation=%+v applied=%v claim=%+v err=%v", completed, applied, claimed, err)
	}
}

func TestProviderObservationRejectsMisboundIdentityWithoutChangingState(t *testing.T) {
	store := NewMemoryStore()
	input := testOperationRequest("op_observation_misbound", "provider_a")
	created := seedOperation(t, store, input, StatusCreated)
	observation := providerResponseObservation(input.OperationID, ObservationSourceSale, providerResponse{
		Provider: input.Provider, OperationID: input.OperationID, Status: "captured",
	}, StatusSucceeded, "confirmed", "", "")
	observation.AttemptID = "another_attempt"
	if _, _, err := store.ApplyProviderObservation(t.Context(), observation, nil); err == nil {
		t.Fatal("misbound observation was accepted")
	}
	current, err := store.Get(t.Context(), input.OperationID)
	if err != nil || current.Status != created.Status || !current.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("misbound observation changed state: current=%+v err=%v", current, err)
	}
	observations, err := store.ListProviderObservations(t.Context(), input.OperationID)
	if err != nil || len(observations) != 0 {
		t.Fatalf("misbound observation reached ledger: observations=%+v err=%v", observations, err)
	}

	invalidRuntimeStatus := observation
	invalidRuntimeStatus.AttemptID = ""
	invalidRuntimeStatus.NormalizedStatus = StatusCreated
	invalidRuntimeStatus.NormalizedCertainty = "none"
	if _, _, err := store.ApplyProviderObservation(t.Context(), invalidRuntimeStatus, nil); err == nil {
		t.Fatal("runtime observation created a dispatch state")
	}
	invalidSensitivePayload := observation
	invalidSensitivePayload.AttemptID = ""
	invalidSensitivePayload.SanitizedPayload = mustJSON(map[string]string{"payment_method_token": "must_not_persist"})
	if _, _, err := store.ApplyProviderObservation(t.Context(), invalidSensitivePayload, nil); err == nil {
		t.Fatal("sensitive provider observation payload was accepted")
	}
	observations, err = store.ListProviderObservations(t.Context(), input.OperationID)
	if err != nil || len(observations) != 0 {
		t.Fatalf("invalid observations reached ledger: observations=%+v err=%v", observations, err)
	}
}

func requireProviderObservation(
	t *testing.T,
	observation ProviderOperationObservation,
	source string,
	rawStatus string,
	normalizedStatus string,
	eventID string,
) {
	t.Helper()
	if observation.Source != source || observation.RawProviderStatus != rawStatus ||
		observation.NormalizedStatus != normalizedStatus || observation.ProviderEventID != eventID ||
		observation.OperationID == "" || observation.AttemptID == "" ||
		observation.ProviderConnectionID == "" || observation.ProviderConnectionRevision == "" ||
		observation.ProviderNamespaceID == "" || observation.Provider == "" ||
		observation.ObservedAt.IsZero() || observation.CreatedAt.IsZero() {
		t.Fatalf("provider observation fields=%+v", observation)
	}
	hash := sha256.Sum256(observation.SanitizedPayload)
	if observation.PayloadHash != hex.EncodeToString(hash[:]) {
		t.Fatalf("payload hash=%s want=%s payload=%s", observation.PayloadHash, hex.EncodeToString(hash[:]), observation.SanitizedPayload)
	}
	if len(observation.PayloadHash) != sha256.Size*2 {
		t.Fatalf("payload hash length=%d", len(observation.PayloadHash))
	}
}
