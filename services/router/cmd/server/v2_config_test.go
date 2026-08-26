package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func TestDevelopmentV2UsesDeclaredConnectionsAndCurrencySupport(t *testing.T) {
	engine, err := loadV2Engine("local", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ currency, id string }{{"USD", "simulator_a_us_primary"}, {"BRL", "simulator_a_us_primary"}, {"EUR", "simulator_a_eu_backup"}, {"GBP", ""}} {
		context := testfixture.Context()
		context.Base.Currency = test.currency
		result, err := engine.Decide(context)
		if test.id == "" {
			if !errors.Is(err, routing.ErrNoEligibleProvider) || result.SelectedCandidate != nil {
				t.Fatal("GBP acquired an undeclared connection")
			}
			continue
		}
		if err != nil || result.SelectedCandidate == nil || result.SelectedCandidate.ID != test.id || result.SelectedCandidate.ConnectionRevision != test.id+"_v1" || result.SelectedCandidate.ProviderNamespaceID != "simulator_a" {
			t.Fatalf("%s selected %+v: %v", test.currency, result.SelectedCandidate, err)
		}
	}
	if !engine.ReadyAt(testfixture.Now) {
		t.Fatal("development snapshot is inactive")
	}
}

func TestV2ProductionIsExplicitlyUnconfiguredOrStrictlyCompiled(t *testing.T) {
	engine, err := loadV2Engine("production", "")
	if err != nil || engine != nil {
		t.Fatal("production used development or legacy configuration")
	}
	encoded, err := json.Marshal(developmentV2Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadV2Engine("production", string(encoded)); err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{
		"unknown field":   strings.TrimSuffix(string(encoded), "}") + `,"policy":{}}`,
		"duplicate field": strings.Replace(string(encoded), `"version":"local_rules_v1"`, `"version":"overridden","version":"local_rules_v1"`, 1),
		"wrong schema":    strings.Replace(string(encoded), `routing_rules.v1`, `routing_rules.v99`, 1),
		"null config":     "null",
		"second object":   string(encoded) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadV2Engine("production", payload); err == nil {
				t.Fatal("accepted ambiguous or unsupported v2 config")
			}
		})
	}
}

func TestInternalAuthenticationProtectsV2(t *testing.T) {
	called := false
	handler := internalAuthentication(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) }), "internal-test-token")
	for _, token := range []string{"", "wrong", "internal-test-token"} {
		called = false
		request := httptest.NewRequest(http.MethodPost, "/internal/v2/routing-decisions", nil)
		request.Header.Set("X-Internal-Token", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if token == "internal-test-token" {
			if !called || response.Code != 200 {
				t.Fatal("authenticated v2 not forwarded")
			}
		} else if called || response.Code != 401 {
			t.Fatal("unauthenticated v2 forwarded")
		}
	}
}
