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
	t.Setenv("PROVIDER_CONNECTION_PROVIDERS", "provider_a=provider_a")
	t.Setenv("PROVIDER_CONNECTION_REVISIONS", "provider_a=provider_a_v1")
	t.Setenv("PROVIDER_NAMESPACE_IDS", "provider_a=provider_a")
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
	if cfg.ProviderConclusiveAbsence["provider_a"] {
		t.Fatal("production provider gained conclusive-absence authority without explicit configuration")
	}
}

func TestGatewayConfigUsesDefaultsOnlyForDevelopment(t *testing.T) {
	clearGatewaySettings(t)
	cfg, err := loadGatewayConfig("local")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalToken != "local-internal-token" || cfg.ProviderEndpoints["simulator_a_us_primary"] == "" || cfg.WebhookSecrets["simulator_a_us_primary"] == "" {
		t.Fatalf("local defaults are incomplete: %+v", cfg)
	}
	if cfg.ProviderEndpoints["simulator_a"] != cfg.ProviderEndpoints["simulator_a_us_primary"] ||
		cfg.ProviderKinds["simulator_a"] != "simulator_a" ||
		cfg.ProviderConnectionRevisions["simulator_a"] != "legacy_v1" ||
		cfg.ProviderNamespaceIDs["simulator_a"] != "simulator_a" ||
		cfg.ProviderRecoveryAliases["simulator_a"] != "simulator_a_us_primary" ||
		cfg.WebhookSecrets["simulator_a"] != cfg.WebhookSecrets["simulator_a_us_primary"] ||
		!cfg.ProviderConclusiveAbsence["simulator_a"] || !cfg.ProviderConclusiveAbsence["simulator_a_us_primary"] {
		t.Fatalf("local defaults do not preserve the legacy simulator connection alias: %+v", cfg)
	}
}

func TestGatewayConfigDoesNotInjectLegacyAliasOutsideDevelopment(t *testing.T) {
	clearGatewaySettings(t)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "explicit-internal-token")
	t.Setenv("PROVIDER_ENDPOINTS", "provider_a_us_primary=https://provider.example")
	t.Setenv("PROVIDER_CONNECTION_PROVIDERS", "provider_a_us_primary=provider_a")
	t.Setenv("PROVIDER_CONNECTION_REVISIONS", "provider_a_us_primary=provider_a_us_primary_v1")
	t.Setenv("PROVIDER_NAMESPACE_IDS", "provider_a_us_primary=provider_a")
	t.Setenv("PROVIDER_WEBHOOK_SECRETS", "provider_a_us_primary=explicit-webhook-secret")
	t.Setenv("GATEWAY_WEBHOOK_BASE_URL", "https://gateway.example")
	t.Setenv("PAYMENT_OUTCOME_URL", "https://payment.internal/provider-outcomes")
	cfg, err := loadGatewayConfig("production")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := cfg.ProviderEndpoints["simulator_a"]; exists {
		t.Fatalf("production config unexpectedly injected simulator alias: %+v", cfg)
	}
}

func TestGatewayConfigCompletesExistingDevelopmentRecoveryAliasIdentity(t *testing.T) {
	clearGatewaySettings(t)
	t.Setenv("PROVIDER_ENDPOINTS", "simulator_a=http://simulator:8083,simulator_a_us_primary=http://simulator:8083,simulator_a_eu_backup=http://simulator:8083")
	t.Setenv("PROVIDER_CONNECTION_PROVIDERS", "simulator_a=simulator_a,simulator_a_us_primary=simulator_a,simulator_a_eu_backup=simulator_a")
	t.Setenv("PROVIDER_CONNECTION_REVISIONS", "simulator_a_us_primary=simulator_a_us_primary_v1,simulator_a_eu_backup=simulator_a_eu_backup_v1")
	t.Setenv("PROVIDER_NAMESPACE_IDS", "simulator_a_us_primary=simulator_a,simulator_a_eu_backup=simulator_a")
	t.Setenv("PROVIDER_WEBHOOK_SECRETS", "simulator_a=secret,simulator_a_us_primary=secret,simulator_a_eu_backup=secret")
	cfg, err := loadGatewayConfig("local")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderConnectionRevisions["simulator_a"] != "legacy_v1" ||
		cfg.ProviderNamespaceIDs["simulator_a"] != "simulator_a" ||
		cfg.ProviderRecoveryAliases["simulator_a"] != "simulator_a_us_primary" {
		t.Fatalf("existing recovery alias identity was not completed: %+v", cfg)
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

func TestParseOptionalBoolMapIsExplicitAndRejectsAmbiguity(t *testing.T) {
	parsed, err := parseOptionalBoolMap("provider_a=true,provider_b=false")
	if err != nil || !parsed["provider_a"] || parsed["provider_b"] {
		t.Fatalf("parseOptionalBoolMap() = %+v, %v", parsed, err)
	}
	for _, input := range []string{"provider", "provider=maybe", "provider=true,provider=false"} {
		if _, err := parseOptionalBoolMap(input); err == nil {
			t.Fatalf("parseOptionalBoolMap(%q) unexpectedly succeeded", input)
		}
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
		"PROVIDER_CONNECTION_PROVIDERS",
		"PROVIDER_CONNECTION_REVISIONS",
		"PROVIDER_NAMESPACE_IDS",
		"PROVIDER_RECOVERY_ALIASES",
		"PROVIDER_CONCLUSIVE_ABSENCE",
		"PROVIDER_WEBHOOK_SECRETS",
		"GATEWAY_WEBHOOK_BASE_URL",
		"PAYMENT_OUTCOME_URL",
		"PROVIDER_REQUEST_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}
