package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YouEclipse/payments/tools/provider-simulator/internal/simulator"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := simulator.Config{
		Environment:   env("PAYMENTS_ENVIRONMENT", "local"),
		ProviderID:    env("PROVIDER_ID", "simulator_a"),
		WebhookSecret: env("PROVIDER_WEBHOOK_SECRET", "dev-webhook-secret"),
		WebhookDelay:  durationEnv("PROVIDER_WEBHOOK_DELAY", 150*time.Millisecond),
		TimeoutDelay:  durationEnv("PROVIDER_TIMEOUT_DELAY", 2*time.Second),
	}
	server, err := simulator.New(cfg, log)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              env("HTTP_ADDR", ":8083"),
		Handler:           server.Handler(),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Info("provider simulator listening", "address", httpServer.Addr, "provider", cfg.ProviderID)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("provider simulator stopped", "error", err)
		os.Exit(1)
	}
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
