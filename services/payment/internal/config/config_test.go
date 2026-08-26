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

func TestV2UsesOnlyTheSingleLocalDemoProfile(t *testing.T) {
	routingEnvironment(t)
	t.Setenv("PAYMENTS_ENVIRONMENT", "local")
	t.Setenv("PAYMENT_API_KEYS", "merchant_demo=local-api-key")
	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.RouterAPIVersion != "v2" || len(settings.MerchantProfiles) != 1 || settings.MerchantProfiles[0].MerchantID != "merchant_demo" {
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

func TestMalformedAPIKeyConfigurationDoesNotEchoCredential(t *testing.T) {
	const secret = "sensitive-test-key-without-merchant"
	if _, err := parseMerchantKeys(secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error exposed input: %v", err)
	}
}

func routingEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PAYMENT_ROUTER_API_VERSION", "PAYMENT_MERCHANT_PROFILES_JSON", "PAYMENT_UPSTREAM_TIMEOUT", "PAYMENT_RECONCILE_INTERVAL", "PAYMENT_RECONCILE_STALE_AGE"} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_URL", "postgres://test.invalid/payments")
	t.Setenv("PAYMENT_API_KEYS", "merchant_other=explicit-test-key")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "explicit-test-internal-token")
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
