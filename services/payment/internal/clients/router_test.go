package clients

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterRejectsSelectedCandidateOutsideFirstOrderPosition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"decision_id":"rtd_test","selected_candidate":{"candidate_id":"provider_a_us_primary","connection_revision":"provider_a_us_primary_v1","provider_namespace_id":"provider_a","provider":"provider_a","merchant_account":"acct_us","connector":"provider_a","region":"us-east-1"},"candidate_order":["provider_a_eu_backup","provider_a_us_primary"],"exclusions":[],"policy_version":"test-v1"}`))
	}))
	defer server.Close()

	client, err := NewRouterClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Decide(t.Context(), RoutingRequest{
		PaymentID: "pi_1", AttemptID: "att_1", MerchantID: "merchant_1",
		AmountMinor: 100, Currency: "USD", PaymentMethod: "card", Operation: "sale",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("Decide() error = %v, want ErrUpstream", err)
	}
}

func TestRouterReceivesOnlyNonSensitiveRoutingContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal/v1/routing-decisions" {
			t.Errorf("unexpected path %s", request.URL.Path)
			http.Error(response, "bad path", http.StatusBadRequest)
			return
		}
		if request.Header.Get("X-Internal-Token") != "internal-test-token" {
			t.Error("internal service token was not sent")
			http.Error(response, "bad auth", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(response, "bad json", http.StatusBadRequest)
			return
		}
		if len(payload) != 7 {
			t.Errorf("router request leaked extra fields: %+v", payload)
		}
		for _, forbidden := range []string{"payment_method_token", "policy", "candidates"} {
			if _, exists := payload[forbidden]; exists {
				t.Errorf("router request contains forbidden field %q", forbidden)
			}
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"decision_id":"rtd_test","selected_candidate":{"candidate_id":"simulator_a_us_primary","connection_revision":"simulator_a_us_primary_v1","provider_namespace_id":"simulator_a","provider":"simulator_a","merchant_account":"acct_us","connector":"simulator","region":"us-east-1"},"candidate_order":["simulator_a_us_primary"],"exclusions":[],"policy_version":"test-v1"}`))
	}))
	defer server.Close()

	client, err := NewRouterClient(server.URL, "internal-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	decision, err := client.Decide(context.Background(), RoutingRequest{
		PaymentID:     "pi_1",
		AttemptID:     "att_1",
		MerchantID:    "merchant_1",
		AmountMinor:   2599,
		Currency:      "USD",
		PaymentMethod: "card",
		Operation:     "sale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedCandidate.CandidateID != "simulator_a_us_primary" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}
