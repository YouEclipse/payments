// Package testfixture contains non-secret fixtures shared by Router tests.
package testfixture

import (
	"encoding/json"
	"os"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/capability"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/rules"
)

// These are the checked-in cross-service contract fixtures. Router generates
// the v3 response golden from its real engine/HTTP encoder, while Payment reads
// the same bytes through its independent strict client. Go tests run in their
// package directories, so paths are relative to this package.
var (
	ContextGolden          = readGolden("routing-context-v1.json")
	RiskContextGolden      = readGolden("routing-context-v2.json")
	RoutingV3SuccessGolden = readGolden("routing-decision-v3-success.json")
)

func readGolden(name string) []byte {
	payload, err := os.ReadFile("../../../../contracts/internal/fixtures/" + name)
	if err != nil {
		panic(err)
	}
	return payload
}

var Now = time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)

func Context() routingcontext.Context {
	var context routingcontext.Context
	if err := json.Unmarshal(ContextGolden, &context); err != nil {
		panic(err)
	}
	return context
}

func Known(values ...string) capability.Set { return capability.Set{State: "known", Values: values} }

func Catalog() capability.Artifact {
	published, expires := Now.Add(-time.Hour), Now.Add(time.Hour)
	candidates := []routing.CandidateRef{
		{ID: "conn_us", ConnectionRevision: "conn_us_v1", ProviderNamespaceID: "simulator_a", Provider: "simulator_a", MerchantAccount: "merchant_demo_us", Connector: "simulator", Region: "us-east-1"},
		{ID: "conn_eu", ConnectionRevision: "conn_eu_v1", ProviderNamespaceID: "simulator_a", Provider: "simulator_a", MerchantAccount: "merchant_demo_eu", Connector: "simulator", Region: "eu-west-1"},
	}
	descriptors := []capability.Descriptor{}
	for index, candidate := range candidates {
		currencies := Known("USD", "BRL")
		if index == 1 {
			currencies = Known("EUR")
		}
		descriptors = append(descriptors, capability.Descriptor{CandidateID: candidate.ID, Enabled: "yes", Currencies: currencies,
			CountryField: "merchant.country", Countries: Known("US"), PaymentMethods: Known("card"), Operations: Known("sale"),
			CaptureMethods: Known("automatic"), CredentialDomains: Known("simulator_a"), Source: "fixture_catalog", SourceVersion: "v1",
			ObservedAt: published, EffectiveFrom: published, ExpiresAt: expires})
	}
	return capability.Artifact{SchemaVersion: capability.SchemaVersion, Version: "catalog_v1", PublishedAt: published, EffectiveFrom: published,
		ExpiresAt: expires, Candidates: candidates, Descriptors: descriptors}
}

func RuleSet() rules.Artifact {
	return rules.Artifact{SchemaVersion: rules.SchemaVersion, Version: "rules_v1", PublishedAt: Now.Add(-time.Hour), EffectiveFrom: Now.Add(-time.Hour),
		ExpiresAt: Now.Add(time.Hour), DefaultCandidateOrder: []string{"conn_us", "conn_eu"}, Rules: []rules.Rule{}}
}

func String(value string) *string { return &value }
func Int(value int64) *int64      { return &value }

func Rule(id, action string, priority int, ids ...string) rules.Rule {
	return rules.Rule{ID: id, Priority: priority, Condition: rules.Condition{Op: "all"}, Action: rules.Action{Type: action, CandidateIDs: ids}, ReasonCode: id + "_matched"}
}
