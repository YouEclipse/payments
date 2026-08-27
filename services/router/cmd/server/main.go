package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/httpapi"
	"github.com/YouEclipse/payments/services/router/internal/routing"
)

const defaultHTTPAddress = ":8081"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	environment := environmentValue("PAYMENTS_ENVIRONMENT", "local")
	internalToken := settingValue("INTERNAL_SERVICE_TOKEN", "local-internal-token")
	if !isDevelopmentEnvironment(environment) && strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN")) == "" {
		logger.Error("INTERNAL_SERVICE_TOKEN must be explicitly configured outside development environments")
		os.Exit(1)
	}

	config, err := loadRoutingConfig(
		environment,
		os.Getenv("ROUTER_CONFIG_JSON"),
	)
	if err != nil {
		logger.Error("load router configuration", "error", err)
		os.Exit(1)
	}
	engine, err := routing.NewEngine(config)
	if err != nil {
		logger.Error("validate router configuration", "error", err)
		os.Exit(1)
	}
	v2, err := loadV2Engine(environment, os.Getenv("ROUTER_V2_CONFIG_JSON"))
	if err != nil {
		logger.Error("validate Router v2 configuration", "error", err)
		os.Exit(1)
	}
	v3, err := loadV3Engine(environment, os.Getenv("ROUTER_V3_CONFIG_JSON"), time.Now().UTC())
	if err != nil {
		logger.Error("validate Router v3 configuration", "error", err)
		os.Exit(1)
	}
	requiredVersion := settingValue("ROUTER_REQUIRED_API_VERSION", "v1")
	if requiredVersion != "v1" && requiredVersion != "v2" && requiredVersion != "v3" {
		logger.Error("ROUTER_REQUIRED_API_VERSION must be v1, v2 or v3")
		os.Exit(1)
	}
	handler := httpapi.NewHandler(engine, logger).WithV2(v2).WithV3(v3).WithRequiredVersion(requiredVersion)
	mux := http.NewServeMux()
	handler.Register(mux)

	server := &http.Server{
		Addr:              httpAddress(),
		Handler:           internalAuthentication(mux, internalToken),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("router service listening", "address", server.Addr)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("router service stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("router service shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("router service shutdown failed", "error", err)
		os.Exit(1)
	}
}

func internalAuthentication(next http.Handler, expected string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/livez" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		provided := r.Header.Get("X-Internal-Token")
		if expected == "" || len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"internal authentication failed"}}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func httpAddress() string {
	if address := strings.TrimSpace(os.Getenv("ROUTER_HTTP_ADDR")); address != "" {
		return address
	}
	return defaultHTTPAddress
}

func environmentValue(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return strings.ToLower(value)
	}
	return fallback
}

func settingValue(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func loadRoutingConfig(environment, rawJSON string) (routing.Config, error) {
	if strings.TrimSpace(rawJSON) == "" {
		if !isDevelopmentEnvironment(environment) {
			return routing.Config{}, fmt.Errorf("ROUTER_CONFIG_JSON is required when PAYMENTS_ENVIRONMENT is %q", environment)
		}
		return developmentRoutingConfig(), nil
	}

	var config routing.Config
	decoder := json.NewDecoder(strings.NewReader(rawJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return routing.Config{}, fmt.Errorf("decode ROUTER_CONFIG_JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return routing.Config{}, errors.New("decode ROUTER_CONFIG_JSON: expected exactly one JSON object")
		}
		return routing.Config{}, fmt.Errorf("decode ROUTER_CONFIG_JSON: %w", err)
	}
	return config, nil
}

func isDevelopmentEnvironment(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "local", "dev", "development", "test":
		return true
	default:
		return false
	}
}

func developmentRoutingConfig() routing.Config {
	return routing.Config{
		Policy: routing.Policy{
			Version: "local_static_v2",
			CandidateOrder: []string{
				"simulator_a_us_primary",
				"simulator_a_eu_backup",
			},
		},
		Candidates: []routing.Candidate{
			{
				ID:                      "simulator_a_us_primary",
				ConnectionRevision:      "simulator_a_us_primary_v1",
				ProviderNamespaceID:     "simulator_a",
				Provider:                "simulator_a",
				MerchantAccount:         "merchant_demo_us",
				Connector:               "simulator",
				Region:                  "us-east-1",
				Enabled:                 true,
				SupportedCurrencies:     []string{"USD", "EUR", "BRL"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
			{
				ID:                      "simulator_a_eu_backup",
				ConnectionRevision:      "simulator_a_eu_backup_v1",
				ProviderNamespaceID:     "simulator_a",
				Provider:                "simulator_a",
				MerchantAccount:         "merchant_demo_eu",
				Connector:               "simulator",
				Region:                  "eu-west-1",
				Enabled:                 true,
				SupportedCurrencies:     []string{"USD", "EUR", "BRL"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
		},
	}
}
