package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YouEclipse/payments/services/router/internal/routing"
)

func TestLoadRoutingConfigRejectsImplicitProductionPolicy(t *testing.T) {
	t.Parallel()

	for _, environment := range []string{"staging", "production", "live", ""} {
		environment := environment
		t.Run(environment, func(t *testing.T) {
			t.Parallel()
			_, err := loadRoutingConfig(environment, "")
			if err == nil || !strings.Contains(err.Error(), "ROUTER_CONFIG_JSON is required") {
				t.Fatalf("loadRoutingConfig(%q, empty) error = %v, want required-config error", environment, err)
			}
		})
	}
}

func TestInternalAuthenticationProtectsDecisionEndpoint(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := internalAuthentication(next, "internal-secret")

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/internal/v1/routing-decisions", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	authorizedRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/routing-decisions", nil)
	authorizedRequest.Header.Set("X-Internal-Token", "internal-secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("authorized status = %d, want 204", authorized.Code)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("health status = %d, want 204", health.Code)
	}
}

func TestLoadRoutingConfigUsesDevelopmentDefault(t *testing.T) {
	t.Parallel()

	config, err := loadRoutingConfig("test", "")
	if err != nil {
		t.Fatalf("loadRoutingConfig() error = %v", err)
	}
	if config.Policy.Version != "local_static_v1" {
		t.Fatalf("policy version = %q, want local_static_v1", config.Policy.Version)
	}
	if len(config.Candidates) != 2 || config.Candidates[0].Provider != "simulator_a" {
		t.Fatalf("development candidates = %#v", config.Candidates)
	}
	if _, err := routing.NewEngine(config); err != nil {
		t.Fatalf("development config is invalid: %v", err)
	}
}

func TestLoadRoutingConfigParsesExplicitPolicy(t *testing.T) {
	t.Parallel()

	raw := `{
		"policy":{"version":"production_v1","provider_order":["provider_a"]},
		"candidates":[{
			"provider":"provider_a",
			"enabled":true,
			"supported_currencies":["USD"],
			"supported_payment_methods":["card"],
			"supported_operations":["sale"]
		}]
	}`
	config, err := loadRoutingConfig("production", raw)
	if err != nil {
		t.Fatalf("loadRoutingConfig() error = %v", err)
	}
	if _, err := routing.NewEngine(config); err != nil {
		t.Fatalf("explicit config is invalid: %v", err)
	}
}

func TestLoadRoutingConfigRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	raw := `{"policy":{"version":"v1","provider_order":["provider_a"],"token":"secret"},"candidates":[]}`
	_, err := loadRoutingConfig("test", raw)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("loadRoutingConfig() error = %v, want unknown-field error", err)
	}
}
