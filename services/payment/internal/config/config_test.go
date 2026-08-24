package config

import "testing"

func TestDevelopmentAliasUsesOnlyLocalDefaults(t *testing.T) {
	for _, name := range []string{
		"DATABASE_URL", "PAYMENT_API_KEYS", "INTERNAL_SERVICE_TOKEN",
		"ROUTER_BASE_URL", "PROVIDER_GATEWAY_BASE_URL",
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

func TestProductionRequiresExplicitCredentials(t *testing.T) {
	t.Setenv("PAYMENTS_ENVIRONMENT", "production")
	t.Setenv("DATABASE_URL", "postgres://test.invalid/payments")
	t.Setenv("PAYMENT_API_KEYS", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("production configuration unexpectedly used development credentials")
	}
}
