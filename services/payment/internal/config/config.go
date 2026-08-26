package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

type Config struct {
	Environment       string
	HTTPAddr          string
	DatabaseURL       string
	RouterBaseURL     string
	GatewayBaseURL    string
	InternalToken     string
	MerchantAPIKeys   map[string]string
	UpstreamTimeout   time.Duration
	ReconcileInterval time.Duration
	ReconcileStaleAge time.Duration
	RouterAPIVersion  string
	MerchantProfiles  []routingcontext.MerchantProfile
}

func Load() (Config, error) {
	environment := valueOrDefault("PAYMENTS_ENVIRONMENT", "local")
	config := Config{
		Environment:       environment,
		HTTPAddr:          valueOrDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		RouterBaseURL:     valueOrDefault("ROUTER_BASE_URL", "http://127.0.0.1:8081"),
		GatewayBaseURL:    valueOrDefault("PROVIDER_GATEWAY_BASE_URL", "http://127.0.0.1:8082"),
		InternalToken:     os.Getenv("INTERNAL_SERVICE_TOKEN"),
		UpstreamTimeout:   3 * time.Second,
		ReconcileInterval: 2 * time.Second,
		ReconcileStaleAge: 5 * time.Second,
		RouterAPIVersion:  valueOrDefault("PAYMENT_ROUTER_API_VERSION", "v2"),
	}
	if config.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	var err error
	if config.UpstreamTimeout, err = duration("PAYMENT_UPSTREAM_TIMEOUT", config.UpstreamTimeout); err != nil {
		return Config{}, err
	}
	if config.ReconcileInterval, err = duration("PAYMENT_RECONCILE_INTERVAL", config.ReconcileInterval); err != nil {
		return Config{}, err
	}
	if config.ReconcileStaleAge, err = duration("PAYMENT_RECONCILE_STALE_AGE", config.ReconcileStaleAge); err != nil {
		return Config{}, err
	}

	keySpec := os.Getenv("PAYMENT_API_KEYS")
	if keySpec == "" && isLocal(environment) {
		keySpec = "merchant_demo=local-api-key"
	}
	config.MerchantAPIKeys, err = parseMerchantKeys(keySpec)
	if err != nil {
		return Config{}, err
	}
	if config.InternalToken == "" && isLocal(environment) {
		config.InternalToken = "local-internal-token"
	}
	if config.InternalToken == "" {
		return Config{}, errors.New("INTERNAL_SERVICE_TOKEN is required outside local/test")
	}
	if config.RouterAPIVersion != "v1" && config.RouterAPIVersion != "v2" {
		return Config{}, errors.New("PAYMENT_ROUTER_API_VERSION must be v1 or v2")
	}
	if config.RouterAPIVersion == "v2" {
		spec := os.Getenv("PAYMENT_MERCHANT_PROFILES_JSON")
		if spec == "" && isLocal(environment) && len(config.MerchantAPIKeys) == 1 && config.MerchantAPIKeys["merchant_demo"] != "" {
			now := time.Now().UTC()
			config.MerchantProfiles = []routingcontext.MerchantProfile{{MerchantID: "merchant_demo", EntityID: "merchant_demo_us",
				Country: "US", Version: "local_demo.v1", ObservedAt: now, ExpiresAt: now.Add(24 * time.Hour)}}
		} else {
			config.MerchantProfiles, err = routingcontext.DecodeProfiles(spec)
			if err != nil {
				return Config{}, errors.New("PAYMENT_MERCHANT_PROFILES_JSON must contain explicit valid merchant profiles for v2")
			}
		}
		known := make(map[string]bool, len(config.MerchantProfiles))
		now := time.Now().UTC()
		for _, profile := range config.MerchantProfiles {
			if profile.ObservedAt.After(now) || !now.Before(profile.ExpiresAt) {
				return Config{}, errors.New("merchant profile is future-dated or expired")
			}
			known[profile.MerchantID] = true
		}
		for merchant := range config.MerchantAPIKeys {
			if !known[merchant] {
				return Config{}, errors.New("authenticated merchant has no routing profile")
			}
		}
	}
	return config, nil
}

func parseMerchantKeys(spec string) (map[string]string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, errors.New("PAYMENT_API_KEYS must contain at least one merchant_id=api_key pair")
	}
	result := make(map[string]string)
	seenKeys := make(map[string]struct{})
	for _, item := range strings.Split(spec, ",") {
		merchant, key, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || merchant == "" || key == "" {
			return nil, errors.New("PAYMENT_API_KEYS entry must contain nonempty merchant_id=api_key")
		}
		if _, exists := result[merchant]; exists {
			return nil, fmt.Errorf("merchant %q appears more than once in PAYMENT_API_KEYS", merchant)
		}
		if _, exists := seenKeys[key]; exists {
			return nil, errors.New("the same API key cannot authenticate multiple merchants")
		}
		result[merchant] = key
		seenKeys[key] = struct{}{}
	}
	return result, nil
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return value, nil
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func isLocal(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "local", "dev", "development", "test":
		return true
	default:
		return false
	}
}
