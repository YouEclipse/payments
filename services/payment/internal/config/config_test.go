package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func TestDevelopmentAliasUsesOnlyLocalDefaults(t *testing.T) {
	for _, name := range []string{
		"DATABASE_URL", "PAYMENT_API_KEYS", "INTERNAL_SERVICE_TOKEN",
		"ROUTER_BASE_URL", "PROVIDER_GATEWAY_BASE_URL",
		"PAYMENT_ROUTER_API_VERSION", "PAYMENT_MERCHANT_PROFILES_JSON",
		"PAYMENT_RISK_MODE", "PAYMENT_MAX_CLOCK_SKEW",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", "postgres://test.invalid/payments")
	t.Setenv("PAYMENTS_ENVIRONMENT", " DEV ")
	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.InternalToken != "local-internal-token" || settings.MerchantAPIKeys["merchant_demo"] != "local-api-key" {
		t.Fatalf("development defaults were not applied: %+v", settings)
	}
}

func TestV3DefaultsUseOnlyTheSingleLocalDemoProfile(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "local")
	t.Setenv("PAYMENT_API_KEYS", "merchant_demo=local-api-key")
	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.RouterAPIVersion != "v3" || settings.RiskMode != "local_fixture" || len(settings.MerchantProfiles) != 1 || settings.MerchantProfiles[0].MerchantID != "merchant_demo" {
		t.Fatalf("unexpected default routing profile: %+v", settings.MerchantProfiles)
	}
	for _, environment := range []string{"local", "production"} {
		t.Run(environment+" custom merchant", func(t *testing.T) {
			t.Setenv("PAYMENTS_ENVIRONMENT", environment)
			t.Setenv("PAYMENT_API_KEYS", "merchant_other=explicit-test-key")
			if _, err := Load(); err == nil {
				t.Fatal("invented a routing profile for a merchant")
			}
		})
	}
}

func TestV1IsExplicitAndDoesNotRequireV2Profiles(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	t.Setenv("PAYMENT_ROUTER_API_VERSION", "v1")
	settings, err := Load()
	if err != nil || settings.RouterAPIVersion != "v1" || len(settings.MerchantProfiles) != 0 {
		t.Fatalf("legacy reader-first configuration: %+v, %v", settings, err)
	}
	t.Setenv("PAYMENT_ROUTER_API_VERSION", "auto")
	if _, err := Load(); err == nil {
		t.Fatal("accepted an automatic fallback mode")
	}
}

func TestV2RequiresFreshProfilesForEveryAuthenticatedMerchant(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	t.Setenv("PAYMENT_ROUTER_API_VERSION", "v2")
	now := time.Now().UTC()
	valid := routingcontext.MerchantProfile{MerchantID: "merchant_other", EntityID: "entity_us", Country: "US", Version: "profile.v1", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	for _, test := range []struct {
		name      string
		edit      func(*routingcontext.MerchantProfile)
		wantError bool
	}{
		{"valid", func(*routingcontext.MerchantProfile) {}, false},
		{"missing merchant", func(p *routingcontext.MerchantProfile) { p.MerchantID = "merchant_unrelated" }, true},
		{"future observation", func(p *routingcontext.MerchantProfile) { p.ObservedAt = now.Add(time.Minute) }, true},
		{"expired", func(p *routingcontext.MerchantProfile) { p.ExpiresAt = now.Add(-time.Minute) }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := valid
			test.edit(&profile)
			payload, err := json.Marshal([]routingcontext.MerchantProfile{profile})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PAYMENT_MERCHANT_PROFILES_JSON", string(payload))
			_, err = Load()
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestProductionRetainsTheV2ReaderFirstDefault(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	now := time.Now().UTC()
	profile, err := json.Marshal([]routingcontext.MerchantProfile{{
		MerchantID: "merchant_other", EntityID: "entity_us", Country: "US", Version: "profile.v1",
		ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYMENT_MERCHANT_PROFILES_JSON", string(profile))
	settings, err := Load()
	if err != nil || settings.RouterAPIVersion != "v2" || settings.RiskMode != "unavailable" {
		t.Fatalf("existing nonlocal rollout was silently upgraded: %+v, %v", settings, err)
	}
}

func TestMalformedAPIKeyConfigurationDoesNotEchoCredential(t *testing.T) {
	const secret = "sensitive-test-key-without-merchant"
	if _, err := parseMerchantKeys(secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error exposed input: %v", err)
	}
}

func routingEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PAYMENT_ROUTER_API_VERSION", "PAYMENT_MERCHANT_PROFILES_JSON", "PAYMENT_RISK_MODE", "PAYMENT_UPSTREAM_TIMEOUT", "PAYMENT_RECONCILE_INTERVAL", "PAYMENT_RECONCILE_STALE_AGE"} {
		t.Setenv(key, "")
	}
	t.Setenv("PAYMENT_MAX_CLOCK_SKEW", "500ms")
	t.Setenv("DATABASE_URL", "postgres://test.invalid/payments")
	t.Setenv("PAYMENT_API_KEYS", "merchant_other=explicit-test-key")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "explicit-test-internal-token")
	t.Setenv("ROUTER_BASE_URL", "https://router.internal")
	t.Setenv("PROVIDER_GATEWAY_BASE_URL", "https://provider-gateway.internal")
}

func TestProductionRequiresAnExplicitPositiveClockSkewBudget(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	t.Setenv("PAYMENT_ROUTER_API_VERSION", "v1")
	for _, value := range []string{"", "0s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PAYMENT_MAX_CLOCK_SKEW", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAYMENT_MAX_CLOCK_SKEW") {
				t.Fatalf("clock-skew budget %q was accepted: %v", value, err)
			}
		})
	}
}

func TestProductionRejectsUnauthenticatedInternalHTTP(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	t.Setenv("PAYMENT_ROUTER_API_VERSION", "v1")
	for _, variable := range []string{"ROUTER_BASE_URL", "PROVIDER_GATEWAY_BASE_URL"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "http://internal.example")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("unauthenticated endpoint accepted: %v", err)
			}
		})
	}
}

func TestRiskFixtureCannotBeEnabledOutsideDevelopment(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENT_ROUTER_API_VERSION", "v1") // Even an unused fixture setting must not be accepted.
	for _, environment := range []string{"production", "live", "staging", "unknown"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("PAYMENTS_ENVIRONMENT", environment)
			t.Setenv("PAYMENT_RISK_MODE", "local_fixture")
			if _, err := Load(); err == nil {
				t.Fatal("accepted local risk fixture outside development")
			}
			t.Setenv("PAYMENT_RISK_MODE", "")
			settings, err := Load()
			if err != nil || settings.RiskMode != "unavailable" {
				t.Fatalf("missing risk did not fail closed: %+v, %v", settings, err)
			}
		})
	}
	t.Setenv("PAYMENTS_ENVIRONMENT", "test")
	t.Setenv("PAYMENT_RISK_MODE", "allow_all")
	if _, err := Load(); err == nil {
		t.Fatal("accepted an implicit allow-all risk source")
	}
}

func TestProductionRequiresExplicitCredentials(t *testing.T) {
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	t.Setenv("DATABASE_URL", "postgres://test.invalid/payments")
	t.Setenv("PAYMENT_API_KEYS", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("production configuration unexpectedly used development credentials")
	}
}
