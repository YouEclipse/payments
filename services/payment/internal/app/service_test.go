package app

import (
	"net/http"
	"testing"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
)

func TestRetrievedProcessingIntentReturnsHTTP200(t *testing.T) {
	result, err := resultForRetrievedIntent(domain.PaymentIntent{
		ID:              "pi_processing",
		Status:          domain.IntentProcessing,
		IntegrityStatus: domain.IntegrityHealthy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != http.StatusOK {
		t.Fatalf("GET representation status=%d, want %d", result.Status, http.StatusOK)
	}
	if result.Intent.Status != domain.IntentProcessing {
		t.Fatalf("payment state=%s, want %s", result.Intent.Status, domain.IntentProcessing)
	}
}
