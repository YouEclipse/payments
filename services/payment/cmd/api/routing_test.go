package main

import (
	"testing"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/payment/internal/config"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func TestRoutingBuilderWiresExplicitVersionAndRiskBoundary(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	settings := config.Config{Environment: "local", RouterAPIVersion: "v3", RiskMode: "local_fixture",
		MerchantProfiles: []routingcontext.MerchantProfile{{MerchantID: "merchant_demo", EntityID: "entity_us", Country: "US", Version: "profile.v1", ObservedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour)}}}
	input := routingcontext.Input{PaymentID: "pi_test", AttemptID: "att_test", MerchantID: "merchant_demo", AmountMinor: 5200, Currency: "USD", CaptureMethod: "automatic", PaymentMethodToken: "pm_sim_approved"}
	for _, test := range []struct {
		version string
		schema  string
	}{
		{"v2", routingcontext.SchemaVersion},
		{"v3", routingcontext.RiskSchemaVersion},
	} {
		t.Run(test.version, func(t *testing.T) {
			settings.RouterAPIVersion = test.version
			builder, err := routingContextBuilder(settings)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := builder.Build(t.Context(), input, at)
			if err != nil || snapshot.SchemaVersion != test.schema {
				t.Fatalf("snapshot=%+v, err=%v", snapshot, err)
			}
			if test.version == "v3" && (snapshot.Risk == nil || snapshot.Risk.Action != riskdecision.Allow) {
				t.Fatalf("v3 omitted the configured local risk evidence: %+v", snapshot.Risk)
			}
			if test.version == "v2" && snapshot.Risk != nil {
				t.Fatal("silently changed the legacy Context contract")
			}
		})
	}
	settings.RouterAPIVersion = "v1"
	if builder, err := routingContextBuilder(settings); err != nil || builder != nil {
		t.Fatalf("v1 unexpectedly builds a Context: %v, %v", builder, err)
	}
	settings.RouterAPIVersion = "v3"
	settings.RiskMode = "unavailable"
	builder, err := routingContextBuilder(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(t.Context(), input, at); err == nil {
		t.Fatal("unavailable risk was converted to an allow")
	}
	settings.RiskMode = "local_fixture"
	settings.Environment = "production"
	if _, err := routingContextBuilder(settings); err == nil {
		t.Fatal("bypassed the environment guard through direct builder wiring")
	}
}
