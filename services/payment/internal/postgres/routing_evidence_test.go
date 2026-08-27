package postgres

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func routingEvidenceFixture(t *testing.T, suffix string, now time.Time) (domain.PaymentIntent, domain.PaymentAttempt) {
	t.Helper()
	intent, err := domain.NewIntent("pi_"+suffix, "merchant_"+suffix, "order_"+suffix, 2599, "USD", "pm_sim_approved", now)
	if err != nil {
		t.Fatal(err)
	}
	attempt := domain.PaymentAttempt{ID: "att_" + suffix, PaymentIntentID: intent.ID, AttemptNumber: 1,
		ProviderConnectionID: "simulator_a_us_primary", ProviderConnectionRevision: "simulator_a_us_primary_v1",
		ProviderNamespaceID: "simulator_a", Provider: "simulator_a", CreatedAt: now, UpdatedAt: now}
	builder, err := routingcontext.NewBuilder([]routingcontext.MerchantProfile{{MerchantID: intent.MerchantID, EntityID: "entity_us", Country: "US", Version: "merchant_config.v1", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}, routingcontext.SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := builder.Build(t.Context(), routingcontext.Input{PaymentID: intent.ID, AttemptID: attempt.ID, MerchantID: intent.MerchantID, AmountMinor: intent.AmountMinor, Currency: intent.Currency, CaptureMethod: string(intent.CaptureMethod), PaymentMethodToken: intent.PaymentMethodToken}, now)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	decision := clients.RoutingDecision{SelectedCandidate: clients.RouteCandidate{CandidateID: attempt.ProviderConnectionID, ConnectionRevision: attempt.ProviderConnectionRevision, ProviderNamespaceID: attempt.ProviderNamespaceID, Provider: attempt.Provider, MerchantAccount: "simulator_merchant", Connector: "simulator", Region: "us"}, CandidateOrder: []string{attempt.ProviderConnectionID}, Exclusions: []clients.CandidateExclusion{}, PolicyVersion: "rules.v1", ContextSchemaVersion: routingcontext.SchemaVersion, ContextDigest: digest, ExecutableUntil: now.Add(time.Minute).UTC().Truncate(time.Microsecond), RuleSetVersion: "rules.v1", RuleSetDigest: "sha256:" + strings.Repeat("1", 64), CapabilityCatalogVersion: "catalog.v1", CapabilityCatalogDigest: "sha256:" + strings.Repeat("2", 64), RuleHits: []clients.RuleHit{}, Eligibility: []clients.EligibilityEvidence{{CandidateID: attempt.ProviderConnectionID, Status: "eligible", Reasons: []clients.ExclusionReason{}}}, ReasonCodes: []string{}}
	decision.DecisionID = clients.VersionedDecisionContentID(decision)
	attempt.RoutingDecisionID = decision.DecisionID
	executableUntil := decision.ExecutableUntil.UTC()
	attempt.RoutingExecutableUntil = &executableUntil
	contextJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decisionJSON, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	attempt.RoutingContextSnapshot, attempt.RoutingDecisionSnapshot = string(contextJSON), string(decisionJSON)
	return intent, attempt
}

func TestRoutingEvidenceBindsLockedIntentAndSelectedExecutionIdentity(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	intent, attempt := routingEvidenceFixture(t, "evidence", now)
	if err := validateRoutingEvidence(attempt, intent, now); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(attempt.RoutingContextSnapshot+attempt.RoutingDecisionSnapshot, intent.PaymentMethodToken) {
		t.Fatal("routing evidence contains credential")
	}
	for _, test := range []struct {
		name string
		edit func(*domain.PaymentIntent, *domain.PaymentAttempt)
	}{
		{"amount", func(i *domain.PaymentIntent, _ *domain.PaymentAttempt) { i.AmountMinor++ }},
		{"merchant", func(i *domain.PaymentIntent, _ *domain.PaymentAttempt) { i.MerchantID = "merchant_different" }},
		{"currency", func(i *domain.PaymentIntent, _ *domain.PaymentAttempt) { i.Currency = "EUR" }},
		{"capture", func(i *domain.PaymentIntent, _ *domain.PaymentAttempt) { i.CaptureMethod = "manual" }},
		{"connection", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) {
			a.ProviderConnectionID = "simulator_a_eu_backup"
		}},
		{"revision", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) {
			a.ProviderConnectionRevision = "replacement_v2"
		}},
		{"namespace", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.ProviderNamespaceID = "other_account" }},
		{"provider", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.Provider = "other_provider" }},
		{"attempt", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.ID = "att_other" }},
		{"decision id", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.RoutingDecisionID = "route_other" }},
		{"deadline column", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) {
			value := a.RoutingExecutableUntil.Add(time.Second)
			a.RoutingExecutableUntil = &value
		}},
		{"no context", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.RoutingContextSnapshot = "" }},
		{"no decision", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.RoutingDecisionSnapshot = "" }},
		{"extra JSON", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) { a.RoutingContextSnapshot += `{}` }},
		{"unknown field", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) {
			a.RoutingContextSnapshot = strings.Replace(a.RoutingContextSnapshot, `"base":`, `"token":"forbidden","base":`, 1)
		}},
		{"digest drift", func(_ *domain.PaymentIntent, a *domain.PaymentAttempt) {
			a.RoutingDecisionSnapshot = strings.Replace(a.RoutingDecisionSnapshot, `"context_digest":"sha256:`, `"context_digest":"invalid:`, 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			copyIntent, copyAttempt := intent, attempt
			test.edit(&copyIntent, &copyAttempt)
			if err := validateRoutingEvidence(copyAttempt, copyIntent, now); !errors.Is(err, ErrStateConflict) {
				t.Fatalf("mismatched evidence accepted: %v", err)
			}
		})
	}
}

func TestLegacyAttemptDoesNotInventHistoricalRoutingEvidence(t *testing.T) {
	intent, attempt := routingEvidenceFixture(t, "legacy", time.Now().UTC())
	attempt.RoutingContextSnapshot, attempt.RoutingDecisionSnapshot = "", ""
	attempt.RoutingExecutableUntil = nil
	if err := validateRoutingEvidence(attempt, intent, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	orphan := time.Now().UTC().Add(time.Minute)
	attempt.RoutingExecutableUntil = &orphan
	if err := validateRoutingEvidence(attempt, intent, time.Now().UTC()); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("orphan routing deadline accepted: %v", err)
	}
}

func TestRoutingEvidenceExpiresBeforeFirstProviderDispatch(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	intent, attempt := routingEvidenceFixture(t, "expired_authority", now)
	if err := validateRoutingEvidence(attempt, intent, now.Add(time.Minute)); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("exclusive executable_until boundary accepted: %v", err)
	}
}
