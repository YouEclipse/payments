package clients

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
)

func TestGatewayReconcileResubmitSignalIsOperationNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Internal-Token") != "internal-test-token" {
			t.Error("internal service token was not sent")
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(`{"error":{"code":"operation_resubmit_required"}}`))
	}))
	defer server.Close()
	client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Query(t.Context(), "op_created"); !errors.Is(err, ErrGatewayOperationNotFound) {
		t.Fatalf("query error = %v, want ErrGatewayOperationNotFound", err)
	}
}

func TestGatewayAmbiguousFailureNormalizesToUnknown(t *testing.T) {
	outcome, err := NormalizeGatewayResponse(GatewayOperationResponse{
		OperationID:            "op_1",
		Provider:               "simulator_a",
		ProviderIdempotencyKey: "op_1",
		Status:                 "failed",
		SideEffectCertainty:    "possible",
		FailureCode:            "http_500",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != domain.OutcomeUnknown || outcome.RetryMode != domain.RetryQueryOrWait {
		t.Fatalf("ambiguous failure was not normalized to UNKNOWN: %+v", outcome)
	}
}

func TestGatewayConclusiveDecline(t *testing.T) {
	outcome, err := NormalizeGatewayResponse(GatewayOperationResponse{
		OperationID:            "op_1",
		Provider:               "simulator_a",
		ProviderIdempotencyKey: "op_1",
		Status:                 "failed",
		SideEffectCertainty:    "none",
		FailureCode:            "card_declined",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != domain.OutcomeDeclined || outcome.SideEffectCertainty != domain.SideEffectNone {
		t.Fatalf("decline mapping failed: %+v", outcome)
	}
}
