package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YouEclipse/payments/services/router/internal/routing"
)

func TestRoutingDecision(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	response := postJSON(t, server.URL+"/internal/v1/routing-decisions", validRequestJSON())
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 200; body = %s", response.StatusCode, body)
	}

	var decision routing.Decision
	if err := json.NewDecoder(response.Body).Decode(&decision); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decision.SelectedProvider != "psp_b" {
		t.Fatalf("SelectedProvider = %q, want psp_b", decision.SelectedProvider)
	}
	if len(decision.CandidateOrder) != 1 || decision.CandidateOrder[0] != "psp_b" {
		t.Fatalf("CandidateOrder = %#v, want [psp_b]", decision.CandidateOrder)
	}
	if len(decision.Exclusions) != 1 || decision.Exclusions[0].Provider != "psp_a" {
		t.Fatalf("Exclusions = %#v, want psp_a", decision.Exclusions)
	}
}

func TestRoutingDecisionRejectsCallerSuppliedPolicy(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	payload := strings.Replace(
		validRequestJSON(),
		`"operation":"sale"`,
		`"operation":"sale","policy":{"version":"caller_override","provider_order":["psp_a"]}`,
		1,
	)
	response := postJSON(t, server.URL+"/internal/v1/routing-decisions", payload)
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body = %s", response.StatusCode, body)
	}
	var envelope errorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Error.Code != "invalid_json" {
		t.Fatalf("error code = %q, want invalid_json", envelope.Error.Code)
	}
}

func TestRoutingDecisionRejectsPaymentMethodToken(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	payload := strings.Replace(
		validRequestJSON(),
		`"payment_method":"card"`,
		`"payment_method":"card","payment_method_token":"pm_secret"`,
		1,
	)
	response := postJSON(t, server.URL+"/internal/v1/routing-decisions", payload)
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body = %s", response.StatusCode, body)
	}
	var envelope errorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Error.Code != "invalid_json" {
		t.Fatalf("error code = %q, want invalid_json", envelope.Error.Code)
	}
}

func TestRoutingDecisionNoEligibleProviderIncludesExclusions(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	payload := strings.Replace(validRequestJSON(), `"currency":"USD"`, `"currency":"JPY"`, 1)
	response := postJSON(t, server.URL+"/internal/v1/routing-decisions", payload)
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 422; body = %s", response.StatusCode, body)
	}
	var result noEligibleProviderResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Error.Code != "no_eligible_provider" {
		t.Fatalf("error code = %q, want no_eligible_provider", result.Error.Code)
	}
	if result.DecisionID == "" || result.PolicyVersion != "policy_v1" {
		t.Fatalf("incomplete decision evidence: %#v", result)
	}
	if len(result.Exclusions) != 2 {
		t.Fatalf("Exclusions = %#v, want two providers", result.Exclusions)
	}
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	for _, path := range []string{"/livez", "/readyz"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			response, err := http.Get(server.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", path, response.StatusCode)
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("GET %s Content-Type = %q", path, contentType)
			}
		})
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := routing.NewEngine(routing.Config{
		Policy: routing.Policy{
			Version:       "policy_v1",
			ProviderOrder: []string{"psp_b", "psp_a"},
		},
		Candidates: []routing.Candidate{
			{
				Provider:                "psp_a",
				Enabled:                 true,
				SupportedCurrencies:     []string{"EUR"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
			{
				Provider:                "psp_b",
				Enabled:                 true,
				SupportedCurrencies:     []string{"USD"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	handler := NewHandler(engine, logger)
	mux := http.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func postJSON(t *testing.T, url, payload string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST routing decision: %v", err)
	}
	return response
}

func validRequestJSON() string {
	return `{
		"payment_id":"pay_123",
		"attempt_id":"att_123",
		"merchant_id":"merchant_123",
		"amount_minor":2599,
		"currency":"USD",
		"payment_method":"card",
		"operation":"sale"
	}`
}
