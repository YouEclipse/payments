package clients

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/recovery"
)

func TestGatewayQueryRejectsConnectionIdentityDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"operation_id":"op_1","attempt_id":"att_1","provider_connection_id":"provider_a_eu_backup","provider_connection_revision":"provider_a_eu_backup_v1","provider_namespace_id":"provider_a","provider":"provider_a","provider_idempotency_key":"op_1","status":"unknown","side_effect_certainty":"possible","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`))
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

func TestGatewayQueryRejectsResponseLargerThanOneMiB(t *testing.T) {
	payload := []byte(`{"padding":"` + strings.Repeat("x", 1<<20) + `"}`)
	if len(payload) <= 1<<20 {
		t.Fatalf("test response size=%d, want more than 1 MiB", len(payload))
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Query(t.Context(), GatewayOperationIdentity{OperationID: "op_oversize"})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "response exceeds size limit") {
		t.Fatalf("query error=%v, want oversized response classified as ErrUpstream", err)
	}
}

func TestGatewayQueryRejectsCaseAliasInNormalResponse(t *testing.T) {
	const payload = `{"operation_id":"op_case","attempt_id":"att_case","provider_connection_id":"provider_a_us_primary","provider_connection_revision":"provider_a_us_primary_v1","provider_namespace_id":"provider_a","provider":"provider_a","provider_idempotency_key":"op_case","Status":"unknown","side_effect_certainty":"possible","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(payload))
	}))
	defer server.Close()

	client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Query(t.Context(), GatewayOperationIdentity{
		OperationID:                "op_case",
		AttemptID:                  "att_case",
		ProviderConnectionID:       "provider_a_us_primary",
		ProviderConnectionRevision: "provider_a_us_primary_v1",
		ProviderNamespaceID:        "provider_a",
		Provider:                   "provider_a",
		ProviderIdempotencyKey:     "op_case",
	})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "ambiguous gateway response") {
		t.Fatalf("query error=%v, want case alias classified as ambiguous ErrUpstream", err)
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
				CreatedAt:                  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				UpdatedAt:                  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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

func TestGatewayOrdinary404CannotAuthorizeResubmit(t *testing.T) {
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
	_, err = client.Query(t.Context(), GatewayOperationIdentity{
		OperationID:                "op_created",
		AttemptID:                  "att_created",
		ProviderConnectionID:       "simulator_a_us_primary",
		ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID:        "simulator_a",
		Provider:                   "simulator_a",
	})
	if errors.Is(err, ErrGatewayOperationNotFound) || !errors.Is(err, ErrUpstream) {
		t.Fatalf("ordinary 404 error = %v, want unproven upstream failure", err)
	}
}

func TestGatewayStructuredRecoveryProofBindsWholeCommand(t *testing.T) {
	now := time.Now().UTC()
	hash := strings.Repeat("a", 64)
	identity := GatewayOperationIdentity{
		OperationID: "op_created", AttemptID: "att_created",
		ProviderConnectionID: "simulator_a_us_primary", ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID: "simulator_a", Provider: "simulator_a",
		ProviderIdempotencyKey: "op_created", RequestHash: hash,
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"error": map[string]string{"code": "operation_resubmit_required", "message": "proof attached"},
			"recovery": map[string]any{
				"schema_version": "gateway_recovery.v1", "kind": "gateway_operation_absent", "request_hash_version": 2,
				"operation_id": identity.OperationID, "attempt_id": identity.AttemptID,
				"provider_connection_id":       identity.ProviderConnectionID,
				"provider_connection_revision": identity.ProviderConnectionRevision,
				"provider_namespace_id":        identity.ProviderNamespaceID, "provider": identity.Provider,
				"provider_idempotency_key": identity.ProviderIdempotencyKey, "request_hash": hash,
				"observed_at": now, "valid_until": now.Add(5 * time.Second),
			},
		})
	}))
	defer server.Close()
	client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Query(t.Context(), identity)
	var proof *GatewayRecoveryRequired
	if !errors.As(err, &proof) || !errors.Is(err, ErrGatewayOperationNotFound) {
		t.Fatalf("structured proof classification=%v", err)
	}
	original := recovery.Identity{
		OperationID: identity.OperationID, AttemptID: identity.AttemptID,
		ConnectionID: identity.ProviderConnectionID, ConnectionRevision: identity.ProviderConnectionRevision,
		NamespaceID: identity.ProviderNamespaceID, Provider: identity.Provider,
		Key: identity.ProviderIdempotencyKey, PayloadHash: "payment-command-hash",
	}
	evidence, err := proof.EvidenceFor(original)
	if err != nil || evidence.Identity != original {
		t.Fatalf("proof was not rebound to the original durable payload: evidence=%+v err=%v", evidence, err)
	}
	changed := original
	changed.Key = "different_key"
	if _, err := proof.EvidenceFor(changed); !errors.Is(err, ErrGatewayIdentityMismatch) {
		t.Fatalf("changed identity accepted: %v", err)
	}
}

func TestGatewayAmbiguousOrExtendedRecoveryProofCannotAuthorizeResubmit(t *testing.T) {
	now := time.Now().UTC()
	hash := strings.Repeat("a", 64)
	identity := GatewayOperationIdentity{
		OperationID: "op_strict", AttemptID: "att_strict",
		ProviderConnectionID: "simulator_a_us_primary", ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID: "simulator_a", Provider: "simulator_a",
		ProviderIdempotencyKey: "op_strict", RequestHash: hash,
	}
	baseProof := fmt.Sprintf(`{"error":{"code":"operation_resubmit_required","message":"proof attached"},"recovery":{"schema_version":"gateway_recovery.v1","kind":"gateway_operation_absent","request_hash_version":2,"operation_id":"%s","attempt_id":"%s","provider_connection_id":"%s","provider_connection_revision":"%s","provider_namespace_id":"%s","provider":"%s","provider_idempotency_key":"%s","request_hash":"%s","observed_at":"%s","valid_until":"%s"}}`,
		identity.OperationID, identity.AttemptID, identity.ProviderConnectionID, identity.ProviderConnectionRevision,
		identity.ProviderNamespaceID, identity.Provider, identity.ProviderIdempotencyKey, hash,
		now.Format(time.RFC3339Nano), now.Add(5*time.Second).Format(time.RFC3339Nano))
	for _, test := range []struct {
		name    string
		payload string
	}{
		{"duplicate_kind", strings.Replace(baseProof, `"kind":"gateway_operation_absent"`, `"kind":"gateway_operation_absent","kind":"provider_conclusive_absent"`, 1)},
		{"case_alias_error_code", strings.Replace(baseProof, `"code":"operation_resubmit_required"`, `"Code":"operation_resubmit_required"`, 1)},
		{"case_alias_recovery_kind", strings.Replace(baseProof, `"kind":"gateway_operation_absent"`, `"Kind":"gateway_operation_absent"`, 1)},
		{"unknown_proof_field", strings.Replace(baseProof, `"request_hash_version":2`, `"request_hash_version":2,"unreviewed_authority":true`, 1)},
		{"unknown_envelope_field", strings.TrimSuffix(baseProof, "}") + `,"debug":"not contract evidence"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusNotFound)
				_, _ = response.Write([]byte(test.payload))
			}))
			defer server.Close()
			client, err := NewGatewayClient(server.URL, "internal-test-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Query(t.Context(), identity)
			var proof *GatewayRecoveryRequired
			if errors.As(err, &proof) || !errors.Is(err, ErrUpstream) {
				t.Fatalf("non-contract proof gained resubmit authority: %v", err)
			}
		})
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

func TestGatewayRequestHashV2GoldenContract(t *testing.T) {
	command := GatewayOperationRequest{
		OperationID: "op_1", AttemptID: "att_1", ProviderConnectionID: "conn_1",
		ProviderConnectionRevision: "rev_1", ProviderNamespaceID: "ns_1", Provider: "provider_1",
		AmountMinor: 2599, Currency: "USD", PaymentMethodToken: "pm_1", CaptureMethod: "automatic",
	}
	const expected = "9879af8f672f44c4e6fcb44e3f53befba1ef9f75b5df534a9cac489aa50f5d7f"
	if got := GatewayRequestHashV2(command); got != expected {
		t.Fatalf("Gateway request hash contract drifted: %s", got)
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
