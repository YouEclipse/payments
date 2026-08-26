package routingcontext_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/strictjson"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func TestContextGoldenMatchesPaymentWireOrder(t *testing.T) {
	var context routingcontext.Context
	if err := strictjson.Decode(testfixture.ContextGolden, &context); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, bytes.TrimSpace(testfixture.ContextGolden)) {
		t.Fatal("Context wire order no longer matches the Payment golden")
	}
	digest, err := context.Digest()
	if err != nil {
		t.Fatal(err)
	}
	const expected = "sha256:c82626bdd259b045998a4c1146bb09a33d066f62fa1e0c51cf833a5dd104d274"
	if digest != expected {
		t.Fatalf("Context golden digest = %s", digest)
	}
	var unordered map[string]any
	if err := json.Unmarshal(testfixture.ContextGolden, &unordered); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(unordered)
	if err != nil {
		t.Fatal(err)
	}
	var same routingcontext.Context
	if err := strictjson.Decode(reordered, &same); err != nil {
		t.Fatal(err)
	}
	actual, err := same.Digest()
	if err != nil || actual != expected {
		t.Fatal("object field order changed semantic Context digest", err)
	}
}

func TestContextRejectsMalformedStatesAndUnsafeRerouting(t *testing.T) {
	cases := []struct {
		name   string
		change func(*routingcontext.Context)
	}{
		{"schema", func(c *routingcontext.Context) { c.SchemaVersion = "routing_context.v99" }},
		{"amount", func(c *routingcontext.Context) { c.Base.AmountMinor = 0 }},
		{"currency", func(c *routingcontext.Context) { c.Base.Currency = "usd" }},
		{"method mismatch", func(c *routingcontext.Context) { c.PaymentMethod.Category = "wallet" }},
		{"capture reroute", func(c *routingcontext.Context) { c.Base.Operation = "capture" }},
		{"refund reroute", func(c *routingcontext.Context) { c.Base.Operation = "refund" }},
		{"sale manual", func(c *routingcontext.Context) { c.Base.CaptureMethod = "manual" }},
		{"missing state", func(c *routingcontext.Context) { c.PaymentMethod.Network.State = "" }},
		{"unknown with value", func(c *routingcontext.Context) {
			c.PaymentMethod.Network.Value = testfixture.String("private-token-never-echo")
		}},
		{"known no value", func(c *routingcontext.Context) { c.Merchant.Country.Value = nil }},
		{"known no expiry", func(c *routingcontext.Context) { c.Merchant.Country.ExpiresAt = nil }},
		{"future observation", func(c *routingcontext.Context) { c.Merchant.Country.ObservedAt = c.BuiltAt.Add(time.Second) }},
		{"expired known", func(c *routingcontext.Context) { expiry := c.BuiltAt; c.Merchant.Country.ExpiresAt = &expiry }},
		{"fresh stale", func(c *routingcontext.Context) { c.PaymentMethod.CredentialDomain.State = "stale" }},
		{"unknown merchant", func(c *routingcontext.Context) {
			c.Merchant.Country = routingcontext.Unknown("config", "v1", c.BuiltAt)
		}},
		{"unknown network", func(c *routingcontext.Context) {
			c.PaymentMethod.Network = routingcontext.Known("invented", "source", "v1", c.BuiltAt, c.BuiltAt.Add(time.Hour))
		}},
		{"issuer casing", func(c *routingcontext.Context) {
			c.PaymentMethod.IssuerCountry = routingcontext.Known("us", "source", "v1", c.BuiltAt, c.BuiltAt.Add(time.Hour))
		}},
		{"timestamp offset", func(c *routingcontext.Context) { c.BuiltAt = c.BuiltAt.In(time.FixedZone("offset", 3600)) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context := testfixture.Context()
			test.change(&context)
			err := context.Validate()
			if !errors.Is(err, routingcontext.ErrInvalidContext) || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("expected sanitized invalid Context, got %v", err)
			}
		})
	}
}

func TestContextObservedStatesNeverCoerceToKnown(t *testing.T) {
	for _, state := range []string{"unknown", "not_applicable", "redacted", "stale"} {
		t.Run(state, func(t *testing.T) {
			context := testfixture.Context()
			observation := routingcontext.Unknown("vault", "v1", context.BuiltAt.Add(-time.Hour))
			observation.State = state
			if state == "stale" {
				observation.Value = testfixture.String("simulator_a")
				expiry := context.BuiltAt
				observation.ExpiresAt = &expiry
			}
			context.PaymentMethod.CredentialDomain = observation
			if err := context.Validate(); err != nil {
				t.Fatal(err)
			}
			value := context.Lookup("payment_method_context.credential_domain")
			if value.State != routingcontext.StateUnknown || value.String != "" {
				t.Fatal("non-current observation became a routing fact")
			}
		})
	}
}

func TestContextOnlineFreshnessDoesNotChangeReplayDigest(t *testing.T) {
	context := testfixture.Context()
	before, _ := context.Digest()
	if err := context.ValidateOnline(context.BuiltAt.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{context.BuiltAt.Add(-time.Nanosecond), context.BuiltAt.Add(5 * time.Minute), context.BuiltAt.Add(time.Hour)} {
		if context.ValidateOnline(now, 5*time.Minute) == nil {
			t.Fatal("expired or future snapshot admitted")
		}
	}
	after, _ := context.Digest()
	if before != after {
		t.Fatal("online admission mutated Context")
	}
}

func TestContextAuthorizeIsOnlyManualAndNoCredentialsInRegistry(t *testing.T) {
	context := testfixture.Context()
	context.Base.Operation = "authorize"
	context.Base.CaptureMethod = "manual"
	if err := context.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"token", "payment_method_token", "risk.allow", "provider", "country", "cost", "health"} {
		if _, found := routingcontext.FieldKind(field); found {
			t.Fatalf("out-of-scope field %s is in registry", field)
		}
	}
}
