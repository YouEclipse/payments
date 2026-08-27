package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func developmentV3ConfigForTest() decision.Config {
	config := developmentV2Config()
	optimization := developmentOptimizationConfig(config, testfixture.Now)
	config.Optimization = &optimization
	return config
}

func TestDevelopmentV3IsRiskGatedAndUsesExactLocalConnections(t *testing.T) {
	engine, err := loadV3Engine("local", "", testfixture.Now)
	if err != nil || engine == nil || !engine.OptimizationConfigured() || !engine.ReadyAt(testfixture.Now) {
		t.Fatalf("local v3 is not ready: %v", err)
	}
	context := testfixture.RiskContext(riskdecision.Allow)
	result, err := engine.Decide(context)
	if err != nil || result.SelectedCandidate == nil || result.SelectedCandidate.ID != "simulator_a_us_primary" ||
		result.Risk == nil || !result.Risk.Continue || result.Optimization == nil || result.Optimization.Scoring.Status != "scored" {
		t.Fatalf("incomplete optimized decision: %+v, %v", result, err)
	}
	blocked := testfixture.RiskContext(riskdecision.Block)
	result, err = engine.Decide(blocked)
	if !errors.Is(err, routing.ErrNoEligibleProvider) || result.SelectedCandidate != nil || len(result.CandidateOrder) != 0 ||
		result.Risk == nil || result.Risk.Outcome != "blocked" || result.Optimization != nil {
		t.Fatalf("risk block created an executable route: %+v, %v", result, err)
	}
}

func TestV2AndV3ConfigurationReadersCannotBeConfused(t *testing.T) {
	if engine, err := loadV3Engine("production", "", testfixture.Now); err != nil || engine != nil {
		t.Fatal("production invented local v3 configuration")
	}
	v2JSON, err := json.Marshal(developmentV2Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadV3Engine("production", string(v2JSON), testfixture.Now); err == nil {
		t.Fatal("v3 accepted a config without optimization")
	}
	v3JSON, err := json.Marshal(developmentV3ConfigForTest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadV2Engine("production", string(v3JSON)); err == nil {
		t.Fatal("v2 accepted a v3 optimization config")
	}
	if engine, err := loadV3Engine("production", string(v3JSON), testfixture.Now); err != nil || engine == nil || !engine.OptimizationConfigured() {
		t.Fatalf("explicit v3 config failed: %v", err)
	}
	for name, payload := range map[string]string{
		"unknown field":   strings.TrimSuffix(string(v3JSON), "}") + `,"risk_model":"allow_all"}`,
		"duplicate field": strings.Replace(string(v3JSON), `"version":"local_rules_v1"`, `"version":"other","version":"local_rules_v1"`, 1),
		"second object":   string(v3JSON) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadV3Engine("production", payload, testfixture.Now); err == nil {
				t.Fatal("accepted ambiguous v3 configuration")
			}
		})
	}
}
