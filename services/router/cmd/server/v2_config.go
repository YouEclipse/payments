package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/capability"
	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/rules"
	"github.com/YouEclipse/payments/services/router/internal/strictjson"
)

func loadV2Engine(environment, rawJSON string) (*decision.Engine, error) {
	if strings.TrimSpace(rawJSON) == "" {
		if !isDevelopmentEnvironment(environment) {
			return nil, nil
		}
		return decision.NewEngine(developmentV2Config())
	}
	var config decision.Config
	if err := strictjson.Decode([]byte(rawJSON), &config); err != nil {
		return nil, fmt.Errorf("decode ROUTER_V2_CONFIG_JSON: %w", err)
	}
	return decision.NewEngine(config)
}

// The long-lived interval is explicitly a deterministic local simulator
// fixture, not a production freshness default. Production requires its own
// expiring, independently versioned Router-owned artifacts.
func developmentV2Config() decision.Config {
	published := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	legacy := developmentRoutingConfig()
	candidates := make([]routing.CandidateRef, 0, len(legacy.Candidates))
	descriptors := make([]capability.Descriptor, 0, len(legacy.Candidates))
	known := func(values ...string) capability.Set { return capability.Set{State: "known", Values: values} }
	for index, candidate := range legacy.Candidates {
		candidates = append(candidates, routing.CandidateRef{ID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision,
			ProviderNamespaceID: candidate.ProviderNamespaceID, Provider: candidate.Provider, MerchantAccount: candidate.MerchantAccount,
			Connector: candidate.Connector, Region: candidate.Region})
		currencies := known("USD", "BRL")
		if index == 1 {
			currencies = known("EUR")
		}
		descriptors = append(descriptors, capability.Descriptor{
			CandidateID: candidate.ID, Enabled: "yes", Currencies: currencies, CountryField: "merchant.country", Countries: known("US"),
			PaymentMethods: known("card"), Operations: known("sale"), CaptureMethods: known("automatic"), CredentialDomains: known("simulator_a"),
			Source: "local_simulator_config", SourceVersion: "local_capabilities.v1", ObservedAt: published, EffectiveFrom: published, ExpiresAt: expires,
		})
	}
	return decision.Config{
		RuleSet: rules.Artifact{SchemaVersion: rules.SchemaVersion, Version: "local_rules_v1", PublishedAt: published, EffectiveFrom: published, ExpiresAt: expires,
			DefaultCandidateOrder: append([]string{}, legacy.Policy.CandidateOrder...), Rules: []rules.Rule{}},
		CapabilityCatalog: capability.Artifact{SchemaVersion: capability.SchemaVersion, Version: "local_capabilities_v1", PublishedAt: published,
			EffectiveFrom: published, ExpiresAt: expires, Candidates: candidates, Descriptors: descriptors},
	}
}
