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
	providerSpec, err := requiredSetting(environment, "PROVIDER_ENDPOINTS", "simulator_a=http://127.0.0.1:8083")
	if err != nil {
		return gateway.Config{}, err
	}
	providers, err := parseMap(providerSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_ENDPOINTS: %w", err)
	}
	secretSpec, err := requiredSetting(environment, "PROVIDER_WEBHOOK_SECRETS", "simulator_a=dev-webhook-secret")
	if err != nil {
		return gateway.Config{}, err
	}
	secrets, err := parseMap(secretSpec)
	if err != nil {
		return gateway.Config{}, fmt.Errorf("PROVIDER_WEBHOOK_SECRETS: %w", err)
	}
	webhookBaseURL, err := requiredSetting(environment, "GATEWAY_WEBHOOK_BASE_URL", "http://127.0.0.1:8082")
	if err != nil {
		return gateway.Config{}, err
	}
	paymentOutcomeURL, err := requiredSetting(environment, "PAYMENT_OUTCOME_URL", "")
	if err != nil {
		return gateway.Config{}, err
	}
	return gateway.Config{
		Environment:       environment,
		ProviderEndpoints: providers,
		WebhookBaseURL:    webhookBaseURL,
		WebhookSecrets:    secrets,
		ProviderTimeout:   durationEnv("PROVIDER_REQUEST_TIMEOUT", 750*time.Millisecond),
		PaymentOutcomeURL: paymentOutcomeURL,
		InternalToken:     internalToken,
	}, nil
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
