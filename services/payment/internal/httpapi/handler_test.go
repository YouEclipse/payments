package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	body := []byte(`{"event_id":"evt_bad","operation_id":"op_bad","provider":"provider_a","status":"succeeded","side_effect_certainty":"confirmed"}`)
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
	return f.outcomeResult, f.outcomeDuplicate, f.outcomeErr
}
