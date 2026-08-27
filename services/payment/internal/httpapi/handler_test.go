package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/YouEclipse/payments/services/payment/internal/app"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
)

func TestProviderOutcomeAcknowledgementUsesKnownPaymentIdentity(t *testing.T) {
	payload := providerOutcomeAcknowledgement(app.Result{
		Quarantined: true,
		Intent: app.PaymentIntentView{
			ID:              "pi_known",
			IntegrityStatus: domain.IntegrityReviewRequired,
		},
	}, false)
	if payload["accepted"] != true || payload["quarantined"] != true || payload["payment_intent_id"] != "pi_known" {
		t.Fatalf("unexpected acknowledgement: %#v", payload)
	}
	if payload["integrity_status"] != domain.IntegrityReviewRequired {
		t.Fatalf("integrity status missing: %#v", payload)
	}
}

func TestDurablyQuarantinedProviderOutcomeReturnsAcceptedWithKnownIdentity(t *testing.T) {
	service := fakePaymentService{outcomeResult: app.Result{
		Status:      http.StatusAccepted,
		Quarantined: true,
		Intent: app.PaymentIntentView{
			ID:              "pi_known",
			IntegrityStatus: domain.IntegrityReviewRequired,
		},
	}}
	handler := NewHandler(service, nil, NewAuthenticator(nil, "internal-test-token"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := []byte(`{"event_id":"evt_bad","operation_id":"op_bad","provider_connection_id":"provider_a_us_primary","provider_connection_revision":"provider_a_us_primary_v1","provider_namespace_id":"provider_a","provider":"provider_a","status":"succeeded","side_effect_certainty":"confirmed"}`)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/provider-outcomes", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Internal-Token", "internal-test-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["accepted"] != true || payload["quarantined"] != true || payload["payment_intent_id"] != "pi_known" || payload["integrity_status"] != string(domain.IntegrityReviewRequired) {
		t.Fatalf("unexpected quarantine ACK: %#v", payload)
	}
}

func TestProviderOutcomeRejectsAmbiguousJSONWithoutCallingService(t *testing.T) {
	const valid = `{"event_id":"evt_1","operation_id":"op_1","attempt_id":"att_1","provider_connection_id":"provider_a_us_primary","provider_connection_revision":"provider_a_us_primary_v1","provider_namespace_id":"provider_a","provider":"provider_a","provider_idempotency_key":"op_1","status":"succeeded","side_effect_certainty":"confirmed","occurred_at":"2026-01-01T00:00:00Z"}`

	tests := []struct {
		name string
		body string
	}{
		{
			name: "duplicate key",
			body: strings.Replace(valid, `"status":"succeeded"`, `"status":"succeeded","status":"failed"`, 1),
		},
		{
			name: "case alias",
			body: strings.Replace(valid, `"status":"succeeded"`, `"Status":"succeeded"`, 1),
		},
		{
			name: "unknown field",
			body: strings.TrimSuffix(valid, "}") + `,"debug":true}`,
		},
		{
			name: "trailing second object",
			body: valid + `{}`,
		},
		{
			name: "top-level null",
			body: `null`,
		},
		{
			name: "null field",
			body: strings.Replace(valid, `"occurred_at":"2026-01-01T00:00:00Z"`, `"occurred_at":null`, 1),
		},
		{
			name: "oversize",
			body: strings.TrimSuffix(valid, "}") + `,"provider_reference":"` + strings.Repeat("x", maxRequestBody) + `"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var outcomeCalls atomic.Int32
			service := fakePaymentService{outcomeCalls: &outcomeCalls}
			handler := NewHandler(service, nil, NewAuthenticator(nil, "internal-test-token"), slog.New(slog.NewTextHandler(io.Discard, nil)))
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/provider-outcomes", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Internal-Token", "internal-test-token")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
			}
			if calls := outcomeCalls.Load(); calls != 0 {
				t.Fatalf("ReceiveProviderOutcome calls=%d, want 0", calls)
			}
		})
	}
}

func TestProviderOutcomeAcknowledgementDoesNotInventIdentityForUnknownOperation(t *testing.T) {
	payload := providerOutcomeAcknowledgement(app.Result{Quarantined: true}, false)
	if payload["accepted"] != true {
		t.Fatalf("unexpected acknowledgement: %#v", payload)
	}
	if payload["quarantined"] != true {
		t.Fatalf("acknowledgement did not expose quarantine state: %#v", payload)
	}
	if _, exists := payload["payment_intent_id"]; exists {
		t.Fatalf("acknowledgement invented a payment identity: %#v", payload)
	}
	if _, exists := payload["integrity_status"]; exists {
		t.Fatalf("acknowledgement emitted a zero integrity status: %#v", payload)
	}
}

type fakePaymentService struct {
	outcomeResult    app.Result
	outcomeDuplicate bool
	outcomeErr       error
	outcomeCalls     *atomic.Int32
}

func (f fakePaymentService) CreateIntent(context.Context, string, string, app.CreateIntentInput) (app.Result, error) {
	return app.Result{}, nil
}

func (f fakePaymentService) ConfirmIntent(context.Context, string, string, string) (app.Result, error) {
	return app.Result{}, nil
}

func (f fakePaymentService) GetIntent(context.Context, string, string) (app.Result, error) {
	return app.Result{}, nil
}

func (f fakePaymentService) ReceiveProviderOutcome(context.Context, domain.ProviderOutcome, []byte) (app.Result, bool, error) {
	if f.outcomeCalls != nil {
		f.outcomeCalls.Add(1)
	}
	return f.outcomeResult, f.outcomeDuplicate, f.outcomeErr
}
