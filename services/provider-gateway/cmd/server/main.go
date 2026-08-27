package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YouEclipse/payments/services/provider-gateway/internal/gateway"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	environment, err := gateway.NormalizeEnvironment(env("PAYMENTS_ENVIRONMENT", "local"))
	if err != nil {
		log.Error("invalid gateway environment", "error", err)
		os.Exit(1)
	}
	cfg, err := loadGatewayConfig(environment)
	if err != nil {
		log.Error("invalid provider gateway configuration", "error", err)
		os.Exit(1)
	}

	var store gateway.Store
	var closeStore func()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL != "" {
		migrateOnStart, err := boolEnv("MIGRATE_ON_START", false)
		if err != nil {
			log.Error("invalid provider gateway configuration", "error", err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		postgres, err := gateway.OpenPostgres(ctx, databaseURL, migrateOnStart)
		if err != nil {
			log.Error("open provider gateway database", "error", err)
			os.Exit(1)
		}
		store = postgres
		closeStore = postgres.Close
	} else {
		if !gateway.IsDevelopmentEnvironment(environment) {
			log.Error("DATABASE_URL is required outside local, development, and test")
			os.Exit(1)
		}
		log.Warn("using volatile provider gateway storage", "environment", environment)
		store = gateway.NewMemoryStore()
		closeStore = func() {}
	}
	defer closeStore()

	service, err := gateway.NewService(cfg, store, log)
	if err != nil {
		log.Error("invalid provider gateway configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go service.RunOutbox(ctx)

	httpServer := &http.Server{
		Addr:              env("HTTP_ADDR", ":8082"),
		Handler:           gateway.NewHTTPHandler(service, store, cfg.InternalToken),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Info("provider gateway listening", "address", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("provider gateway stopped", "error", err)
		os.Exit(1)
	}
}

func boolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func loadGatewayConfig(environment string) (gateway.Config, error) {
	internalToken, err := requiredSetting(environment, "INTERNAL_SERVICE_TOKEN", "local-internal-token")
	if err != nil {
		return gateway.Config{}, err
	}
	providerSpec, err := requiredSetting(environment, "PROVIDER_ENDPOINTS", "simulator_a_us_primary=http://127.0.0.1:8083,simulator_a_eu_backup=http://127.0.0.1:8083")
	if err != nil {
		return gateway.Config{}, err
	}
	providers, err := parseMap(providerSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_ENDPOINTS: %w", err)
	}
	kindSpec, err := requiredSetting(environment, "PROVIDER_CONNECTION_PROVIDERS", "simulator_a_us_primary=simulator_a,simulator_a_eu_backup=simulator_a")
	if err != nil {
		return gateway.Config{}, err
	}
	providerKinds, err := parseMap(kindSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_CONNECTION_PROVIDERS: %w", err)
	}
	revisionSpec, err := requiredSetting(environment, "PROVIDER_CONNECTION_REVISIONS", "simulator_a_us_primary=simulator_a_us_primary_v1,simulator_a_eu_backup=simulator_a_eu_backup_v1")
	if err != nil {
		return gateway.Config{}, err
	}
	connectionRevisions, err := parseMap(revisionSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_CONNECTION_REVISIONS: %w", err)
	}
	namespaceSpec, err := requiredSetting(environment, "PROVIDER_NAMESPACE_IDS", "simulator_a_us_primary=simulator_a,simulator_a_eu_backup=simulator_a")
	if err != nil {
		return gateway.Config{}, err
	}
	namespaceIDs, err := parseMap(namespaceSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_NAMESPACE_IDS: %w", err)
	}
	secretSpec, err := requiredSetting(environment, "PROVIDER_WEBHOOK_SECRETS", "simulator_a_us_primary=dev-webhook-secret,simulator_a_eu_backup=dev-webhook-secret")
	if err != nil {
		return gateway.Config{}, err
	}
	secrets, err := parseMap(secretSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_WEBHOOK_SECRETS: %w", err)
	}
	recoveryAliases, err := parseOptionalMap(os.Getenv("PROVIDER_RECOVERY_ALIASES"))
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_RECOVERY_ALIASES: %w", err)
	}
	conclusiveAbsenceSpec := os.Getenv("PROVIDER_CONCLUSIVE_ABSENCE")
	if strings.TrimSpace(conclusiveAbsenceSpec) == "" && gateway.IsDevelopmentEnvironment(environment) {
		var defaults []string
		for _, connectionID := range []string{"simulator_a_us_primary", "simulator_a_eu_backup"} {
			if _, configured := providers[connectionID]; configured {
				defaults = append(defaults, connectionID+"=true")
			}
		}
		conclusiveAbsenceSpec = strings.Join(defaults, ",")
	}
	conclusiveAbsence, err := parseOptionalBoolMap(conclusiveAbsenceSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_CONCLUSIVE_ABSENCE: %w", err)
	}
	addLegacyDevelopmentConnections(environment, providers, providerKinds, connectionRevisions, namespaceIDs, secrets, recoveryAliases, conclusiveAbsence)
	webhookBaseURL, err := requiredSetting(environment, "GATEWAY_WEBHOOK_BASE_URL", "http://127.0.0.1:8082")
	if err != nil {
		return gateway.Config{}, err
	}
	paymentOutcomeURL, err := requiredSetting(environment, "PAYMENT_OUTCOME_URL", "")
	if err != nil {
		return gateway.Config{}, err
	}
	return gateway.Config{
		Environment:                 environment,
		ProviderKinds:               providerKinds,
		ProviderConnectionRevisions: connectionRevisions,
		ProviderNamespaceIDs:        namespaceIDs,
		ProviderRecoveryAliases:     recoveryAliases,
		ProviderConclusiveAbsence:   conclusiveAbsence,
		ProviderEndpoints:           providers,
		WebhookBaseURL:              webhookBaseURL,
		WebhookSecrets:              secrets,
		ProviderTimeout:             durationEnv("PROVIDER_REQUEST_TIMEOUT", 750*time.Millisecond),
		PaymentOutcomeURL:           paymentOutcomeURL,
		InternalToken:               internalToken,
	}, nil
}

func addLegacyDevelopmentConnections(environment string, endpoints, providerKinds, connectionRevisions, namespaceIDs, secrets, recoveryAliases map[string]string, conclusiveAbsence map[string]bool) {
	if !gateway.IsDevelopmentEnvironment(environment) {
		return
	}
	const (
		legacyConnection  = "simulator_a"
		primaryConnection = "simulator_a_us_primary"
		providerKind      = "simulator_a"
	)
	endpoint, endpointOK := endpoints[primaryConnection]
	secret, secretOK := secrets[primaryConnection]
	if !endpointOK || !secretOK || providerKinds[primaryConnection] != providerKind || namespaceIDs[primaryConnection] != providerKind {
		return
	}
	// 0002 can only recover the historical provider string as a connection ID.
	// Keep that exact local/test alias resolvable while legacy UNKNOWN operations
	// and their previously issued webhook URLs drain.
	if _, exists := endpoints[legacyConnection]; !exists {
		endpoints[legacyConnection] = endpoint
	}
	if _, exists := providerKinds[legacyConnection]; !exists {
		providerKinds[legacyConnection] = providerKind
	}
	if _, exists := connectionRevisions[legacyConnection]; !exists {
		connectionRevisions[legacyConnection] = "legacy_v1"
	}
	if _, exists := namespaceIDs[legacyConnection]; !exists {
		namespaceIDs[legacyConnection] = providerKind
	}
	if _, exists := secrets[legacyConnection]; !exists {
		secrets[legacyConnection] = secret
	}
	if _, exists := recoveryAliases[legacyConnection]; !exists {
		recoveryAliases[legacyConnection] = primaryConnection
	}
	if _, exists := conclusiveAbsence[legacyConnection]; !exists {
		conclusiveAbsence[legacyConnection] = conclusiveAbsence[primaryConnection]
	}
}

func requiredSetting(environment, name, developmentDefault string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, nil
	}
	if gateway.IsDevelopmentEnvironment(environment) {
		return developmentDefault, nil
	}
	return "", fmt.Errorf("%s must be explicitly configured outside local, development, and test", name)
}

func parseMap(value string) (map[string]string, error) {
	result := make(map[string]string)
	for index, pair := range strings.Split(value, ",") {
		key, item, ok := strings.Cut(strings.TrimSpace(pair), "=")
		key = strings.TrimSpace(key)
		item = strings.TrimSpace(item)
		if !ok || key == "" || item == "" {
			return nil, fmt.Errorf("entry %d must be key=value", index)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("key %q appears more than once", key)
		}
		result[key] = item
	}
	if len(result) == 0 {
		return nil, errors.New("at least one key=value entry is required")
	}
	return result, nil
}

func parseOptionalBoolMap(value string) (map[string]bool, error) {
	result := make(map[string]bool)
	if strings.TrimSpace(value) == "" {
		return result, nil
	}
	for index, pair := range strings.Split(value, ",") {
		key, item, ok := strings.Cut(strings.TrimSpace(pair), "=")
		key = strings.TrimSpace(key)
		item = strings.TrimSpace(item)
		if !ok || key == "" || item == "" {
			return nil, fmt.Errorf("entry %d must be key=boolean", index)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("key %q appears more than once", key)
		}
		parsed, err := strconv.ParseBool(item)
		if err != nil {
			return nil, fmt.Errorf("value for %q must be a boolean: %w", key, err)
		}
		result[key] = parsed
	}
	return result, nil
}

func parseOptionalMap(value string) (map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return map[string]string{}, nil
	}
	return parseMap(value)
}
