package main

import (
	"strings"
	"testing"
)

func TestGatewayConfigFailsClosedOutsideDevelopment(t *testing.T) {
	clearGatewaySettings(t)
	if _, err := loadGatewayConfig("production"); err == nil || !strings.Contains(err.Error(), "INTERNAL_SERVICE_TOKEN") {
		t.Fatalf("production defaults error = %v", err)
	}

	t.Setenv("INTERNAL_SERVICE_TOKEN", "explicit-internal-token")
	t.Setenv("PROVIDER_ENDPOINTS", "provider_a=https://provider.example")
	t.Setenv("PROVIDER_WEBHOOK_SECRETS", "provider_a=explicit-webhook-secret")
	t.Setenv("GATEWAY_WEBHOOK_BASE_URL", "https://gateway.example")
	t.Setenv("PAYMENT_OUTCOME_URL", "https://payment.internal/provider-outcomes")
	cfg, err := loadGatewayConfig("production")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalToken != "explicit-internal-token" || cfg.ProviderEndpoints["provider_a"] != "https://provider.example" {
		t.Fatalf("explicit production configuration was not preserved: %+v", cfg)
	}
}

func TestGatewayConfigUsesDefaultsOnlyForDevelopment(t *testing.T) {
	clearGatewaySettings(t)
	cfg, err := loadGatewayConfig("local")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalToken != "local-internal-token" || cfg.ProviderEndpoints["simulator_a"] == "" || cfg.WebhookSecrets["simulator_a"] == "" {
		t.Fatalf("local defaults are incomplete: %+v", cfg)
	}
}

func TestParseMapRejectsMalformedOrDuplicateConfiguration(t *testing.T) {
	for _, input := range []string{"", "provider", "=value", "provider=", "provider=a,provider=b"} {
		if _, err := parseMap(input); err == nil {
			t.Fatalf("parseMap(%q) unexpectedly succeeded", input)
		}
	}
	parsed, err := parseMap("provider_a=https://a.example, provider_b=https://b.example")
	if err != nil || len(parsed) != 2 {
		t.Fatalf("valid provider map = %+v, %v", parsed, err)
	}
}

func TestBoolEnvRejectsInvalidMigrationSetting(t *testing.T) {
	t.Setenv("MIGRATE_ON_START", "definitely")
	if _, err := boolEnv("MIGRATE_ON_START", false); err == nil {
		t.Fatal("invalid boolean unexpectedly succeeded")
	}
	t.Setenv("MIGRATE_ON_START", " TRUE ")
	value, err := boolEnv("MIGRATE_ON_START", false)
	if err != nil || !value {
		t.Fatalf("valid boolean value=%v err=%v", value, err)
	}
}

func clearGatewaySettings(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"INTERNAL_SERVICE_TOKEN",
		"PROVIDER_ENDPOINTS",
		"PROVIDER_WEBHOOK_SECRETS",
		"GATEWAY_WEBHOOK_BASE_URL",
		"PAYMENT_OUTCOME_URL",
		"PROVIDER_REQUEST_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}
