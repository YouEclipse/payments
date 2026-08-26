package routingcontext

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Builder, Input, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	builder, err := NewBuilder([]MerchantProfile{{MerchantID: "merchant_demo", EntityID: "merchant_demo_us", Country: "US",
		Version: "merchant_config.v1", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}, SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	return builder, Input{PaymentID: "pi_example", AttemptID: "att_example", MerchantID: "merchant_demo",
		AmountMinor: 5200, Currency: "USD", CaptureMethod: "automatic", PaymentMethodToken: "pm_sim_approved"}, now
}

func TestSnapshotIsDeterministicAndContainsNoCredentials(t *testing.T) {
	builder, input, now := fixture(t)
	first, err := builder.Build(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	input.PaymentMethodToken = "pm_sim_timeout_then_succeeded"
	second, err := builder.Build(t.Context(), input, now.In(time.FixedZone("offset", 8*3600)))
	if err != nil {
		t.Fatal(err)
	}
	a, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 71 {
		t.Fatalf("unstable snapshot digests: %q %q", a, b)
	}
	payload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pm_sim_", "payment_method_token", `"pan"`, `"cvv"`, `"authorization"`} {
		if strings.Contains(strings.ToLower(string(payload)), forbidden) {
			t.Fatalf("credential in snapshot: %s", forbidden)
		}
	}
	if first.PaymentMethod.Network.State != "unknown" || first.PaymentMethod.IssuerCountry.State != "unknown" {
		t.Fatal("builder invented card metadata")
	}
}

func TestSnapshotMatchesTheSharedRouterWireGolden(t *testing.T) {
	builder, input, now := fixture(t)
	snapshot, err := builder.Build(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("../../../../contracts/internal/fixtures/routing-context-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(golden)) != string(payload) {
		t.Fatal("Payment Context JSON differs from shared Router wire golden")
	}
	digest, err := snapshot.Digest()
	if err != nil || digest != "sha256:c82626bdd259b045998a4c1146bb09a33d066f62fa1e0c51cf833a5dd104d274" {
		t.Fatalf("canonical digest drift: %s %v", digest, err)
	}
}

func TestBuilderRejectsMissingStaleAndInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Input, *time.Time)
	}{
		{"missing merchant", func(i *Input, _ *time.Time) { i.MerchantID = "unconfigured" }},
		{"zero amount", func(i *Input, _ *time.Time) { i.AmountMinor = 0 }},
		{"currency casing", func(i *Input, _ *time.Time) { i.Currency = "usd" }},
		{"manual unsupported", func(i *Input, _ *time.Time) { i.CaptureMethod = "manual" }},
		{"stale merchant", func(_ *Input, n *time.Time) { *n = n.Add(time.Hour) }},
		{"future merchant", func(_ *Input, n *time.Time) { *n = n.Add(-2 * time.Hour) }},
		{"not a simulator token", func(i *Input, _ *time.Time) { i.PaymentMethodToken = "private-token-do-not-log" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder, input, now := fixture(t)
			test.mutate(&input, &now)
			if _, err := builder.Build(t.Context(), input, now); !errors.Is(err, ErrInvalidContext) || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("expected sanitized validation failure, got %v", err)
			}
		})
	}
}

type metadataFunc func(context.Context, string, time.Time) (Metadata, error)

func (f metadataFunc) Lookup(c context.Context, token string, now time.Time) (Metadata, error) {
	return f(c, token, now)
}

func TestBuilderDoesNotAliasMutableMetadataOrProfileInput(t *testing.T) {
	_, input, now := fixture(t)
	profiles := []MerchantProfile{{MerchantID: "merchant_demo", EntityID: "entity_us", Country: "US", Version: "v1", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}
	domain := Known("simulator_a", "vault", "v1", now, now.Add(time.Hour))
	u := Unknown("vault", "v1", now)
	builder, err := NewBuilder(profiles, metadataFunc(func(context.Context, string, time.Time) (Metadata, error) {
		return Metadata{Network: u, IssuerCountry: u, CredentialDomain: domain}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	profiles[0].Country = "GB"
	snapshot, err := builder.Build(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	*domain.Value = "other_account"
	*domain.ExpiresAt = now.Add(-time.Hour)
	if *snapshot.Merchant.Country.Value != "US" || *snapshot.PaymentMethod.CredentialDomain.Value != "simulator_a" || !snapshot.PaymentMethod.CredentialDomain.ExpiresAt.After(now) {
		t.Fatal("snapshot changed through an alias")
	}
}

func TestUnavailableMetadataDoesNotGuessCredentialCompatibility(t *testing.T) {
	builder, input, now := fixture(t)
	builder.metadata = UnavailableMetadata{}
	snapshot, err := builder.Build(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PaymentMethod.CredentialDomain.State != "unknown" || snapshot.PaymentMethod.CredentialDomain.Value != nil {
		t.Fatal("missing metadata became known")
	}
}

func TestObservedCannotMasqueradeAsAnotherState(t *testing.T) {
	builder, input, now := fixture(t)
	original, err := builder.Build(t.Context(), input, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*Context){
		func(c *Context) { c.SchemaVersion = "routing_context.v99" },
		func(c *Context) { c.PaymentMethod.Category = "wallet" },
		func(c *Context) { c.PaymentMethod.Network.State = "missing" },
		func(c *Context) { v := "visa"; c.PaymentMethod.Network.Value = &v },
		func(c *Context) { c.PaymentMethod.CredentialDomain.ExpiresAt = nil },
		func(c *Context) { c.PaymentMethod.CredentialDomain.ObservedAt = now.Add(time.Second) },
		func(c *Context) { c.PaymentMethod.CredentialDomain.State = "stale" },
	} {
		copy := original
		mutation(&copy)
		if copy.Validate() == nil {
			t.Fatal("invalid context was accepted")
		}
	}
}
