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

	"github.com/YouEclipse/payments/services/payment/internal/app"
	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/config"
	"github.com/YouEclipse/payments/services/payment/internal/httpapi"
	paymentpostgres "github.com/YouEclipse/payments/services/payment/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("payment service stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	rootContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poolConfig, err := pgxpool.ParseConfig(settings.DatabaseURL)
	if err != nil {
		return err
	}
	poolConfig.MaxConns = 20
	poolConfig.MinConns = 2
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(rootContext, poolConfig)
	if err != nil {
		return err
	}
	defer pool.Close()
	startupContext, cancelStartup := context.WithTimeout(rootContext, 15*time.Second)
	defer cancelStartup()
	if err := pool.Ping(startupContext); err != nil {
		return err
	}
	if err := paymentpostgres.Migrate(startupContext, pool); err != nil {
		return err
	}

	httpClient := &http.Client{
		Timeout: settings.UpstreamTimeout + time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: settings.UpstreamTimeout,
		},
	}
	router, err := clients.NewRouterClient(settings.RouterBaseURL, settings.InternalToken, httpClient)
	if err != nil {
		return err
	}
	gateway, err := clients.NewGatewayClient(settings.GatewayBaseURL, settings.InternalToken, httpClient)
	if err != nil {
		return err
	}
	store := paymentpostgres.NewStore(pool)
	service := app.NewService(store, router, gateway, logger, settings.UpstreamTimeout)
	authenticator := httpapi.NewAuthenticator(settings.MerchantAPIKeys, settings.InternalToken)
	handler := httpapi.NewHandler(service, store, authenticator, logger)

	reconcilerContext, stopReconciler := context.WithCancel(rootContext)
	defer stopReconciler()
	go service.RunReconciler(reconcilerContext, settings.ReconcileInterval, settings.ReconcileStaleAge)

	server := &http.Server{
		Addr:              settings.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("payment service listening", slog.String("address", settings.HTTPAddr), slog.String("environment", settings.Environment))
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-rootContext.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
