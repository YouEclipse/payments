package clients

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
)

func TestGatewayQueryRejectsConnectionIdentityDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"operation_id":"op_1","attempt_id":"att_1","provider_connection_id":"provider_a_eu_backup","provider_connection_revision":"provider_a_eu_backup_v1","provider_namespace_id":"provider_a","provider":"provider_a","provider_idempotency_key":"op_1","status":"unknown","side_effect_certainty":"possible"}`))
	}))
	defer server.Close()

	client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Query(t.Context(), GatewayOperationIdentity{
		OperationID:                "op_1",
		AttemptID:                  "att_1",
		ProviderConnectionID:       "provider_a_us_primary",
		ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID:        "provider_a",
		Provider:                   "provider_a",
	}); !errors.Is(err, ErrGatewayIdentityMismatch) {
		t.Fatalf("query error = %v, want connection identity mismatch", err)
	}
}

func TestGatewayQueryRejectsRevisionAndNamespaceDrift(t *testing.T) {
	for _, field := range []string{"revision", "namespace"} {
		t.Run(field, func(t *testing.T) {
			identity := GatewayOperationIdentity{
				OperationID:                "op_1",
				AttemptID:                  "att_1",
				ProviderConnectionID:       "provider_a_us_primary",
				ProviderConnectionRevision: "provider_a_us_primary_v1",
				ProviderNamespaceID:        "provider_a",
				Provider:                   "provider_a",
			}
			payload := GatewayOperationResponse{
				OperationID:                identity.OperationID,
				AttemptID:                  identity.AttemptID,
				ProviderConnectionID:       identity.ProviderConnectionID,
				ProviderConnectionRevision: identity.ProviderConnectionRevision,
				ProviderNamespaceID:        identity.ProviderNamespaceID,
				Provider:                   "provider_a",
				Status:                     "unknown",
				SideEffectCertainty:        "possible",
			}
			if field == "revision" {
				payload.ProviderConnectionRevision = "rebound_v2"
			} else {
				payload.ProviderNamespaceID = "another_account"
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(response).Encode(payload)
			}))
			defer server.Close()
			client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Query(t.Context(), identity); !errors.Is(err, ErrGatewayIdentityMismatch) {
				t.Fatalf("query error=%v, want pinned %s mismatch", err, field)
			}
		})
	}
}

func TestGatewayExplicitIdentityConflictIsNotTransportUnknown(t *testing.T) {
	for _, code := range []string{"provider_connection_identity_mismatch", "operation_id_reused"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]string{"code": code}})
			}))
			defer server.Close()
			client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Query(t.Context(), GatewayOperationIdentity{OperationID: "op_pinned"})
			if !errors.Is(err, ErrGatewayIdentityMismatch) || !errors.Is(err, ErrUpstream) {
				t.Fatalf("identity conflict classification was lost: %v", err)
			}
		})
	}
}

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
	if _, err := client.Query(t.Context(), GatewayOperationIdentity{
		OperationID:                "op_created",
		AttemptID:                  "att_created",
		ProviderConnectionID:       "simulator_a_us_primary",
		ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID:        "simulator_a",
		Provider:                   "simulator_a",
	}); !errors.Is(err, ErrGatewayOperationNotFound) {
		t.Fatalf("query error = %v, want ErrGatewayOperationNotFound", err)
	}
}

func TestGatewayAmbiguousFailureNormalizesToUnknown(t *testing.T) {
	outcome, err := NormalizeGatewayResponse(GatewayOperationResponse{
		OperationID:                "op_1",
		ProviderConnectionID:       "simulator_a_us_primary",
		ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID:        "simulator_a",
		Provider:                   "simulator_a",
		ProviderIdempotencyKey:     "op_1",
		Status:                     "failed",
		SideEffectCertainty:        "possible",
		FailureCode:                "http_500",
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
		OperationID:                "op_1",
		ProviderConnectionID:       "simulator_a_us_primary",
		ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID:        "simulator_a",
		Provider:                   "simulator_a",
		ProviderIdempotencyKey:     "op_1",
		Status:                     "failed",
		SideEffectCertainty:        "none",
		FailureCode:                "card_declined",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != domain.OutcomeDeclined || outcome.SideEffectCertainty != domain.SideEffectNone {
		t.Fatalf("decline mapping failed: %+v", outcome)
	}
}
