package app

import (
	"errors"
	"net/http"
	"testing"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/postgres"
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

func TestGatewayIdentityConflictStopsAutomaticRecovery(t *testing.T) {
	aggregate := postgres.Aggregate{Operation: domain.ProviderOperation{
		ID:                         "op_pinned",
		ProviderConnectionID:       "provider_us_primary",
		ProviderConnectionRevision: "provider_us_primary_v1",
		ProviderNamespaceID:        "provider_account",
		Provider:                   "provider",
		ProviderIdempotencyKey:     "op_pinned",
	}}
	for _, test := range []struct {
		name       string
		err        error
		wantReview bool
	}{
		{name: "identity conflict", err: clients.ErrGatewayIdentityMismatch, wantReview: true},
		{name: "transport unknown", err: errors.New("connection reset"), wantReview: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := gatewayFailureOutcome(aggregate, "test_", "gateway_query_unknown", test.err)
			if outcome.RequiresReview != test.wantReview || outcome.Status != domain.OutcomeUnknown {
				t.Fatalf("unexpected failure classification: %+v", outcome)
			}
			if test.wantReview && (outcome.RetryMode != domain.RetryNone || outcome.NormalizedCode != "gateway_execution_identity_conflict") {
				t.Fatalf("identity conflict remained automatically recoverable: %+v", outcome)
			}
			if outcome.ProviderConnectionID != aggregate.Operation.ProviderConnectionID ||
				outcome.ProviderConnectionRevision != aggregate.Operation.ProviderConnectionRevision ||
				outcome.ProviderNamespaceID != aggregate.Operation.ProviderNamespaceID {
				t.Fatalf("failure outcome lost the original execution identity: %+v", outcome)
			}
		})
	}
}
