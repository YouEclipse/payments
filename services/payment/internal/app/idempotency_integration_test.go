//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTwentyConcurrentConfirmationsUseOneDurableFundsPath(t *testing.T) {
	store, pool, ctx := isolatedAppStore(t)
	router := &concurrentRouter{}
	gateway := &countingGateway{}
	service := NewService(store, router, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	created, err := service.CreateIntent(ctx, "merchant_concurrent", "create_concurrent", CreateIntentInput{
		MerchantReference: "order_concurrent", AmountMinor: 2599, Currency: "USD",
		PaymentMethodToken: "pm_concurrent", CaptureMethod: string(domain.CaptureAutomatic),
	})
	if err != nil || created.Status != http.StatusCreated {
		t.Fatalf("create result=%+v err=%v", created, err)
	}

	const callers = 20
	start := make(chan struct{})
	results := make(chan Result, callers)
	errorsSeen := make(chan error, callers)
	var workers sync.WaitGroup
	for range callers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := service.ConfirmIntent(ctx, "merchant_concurrent", created.Intent.ID, "confirm_concurrent")
			results <- result
			errorsSeen <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent confirmation failed: %v", err)
		}
	}
	for result := range results {
		if result.Intent.ID != "" && result.Intent.ID != created.Intent.ID {
			t.Errorf("caller associated with another intent: %+v", result)
		}
		if result.Status != http.StatusOK && result.Status != http.StatusAccepted {
			t.Errorf("caller received non-replayable status: %+v", result)
		}
	}
	if gateway.executes.Load() != 1 {
		t.Fatalf("gateway funds executions=%d, want 1", gateway.executes.Load())
	}
	var attemptCount, operationCount, distinctAttempt, distinctOperation int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(DISTINCT id) FROM payment_attempts WHERE payment_intent_id=$1`, created.Intent.ID,
	).Scan(&attemptCount, &distinctAttempt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(DISTINCT id) FROM provider_operations WHERE payment_intent_id=$1`, created.Intent.ID,
	).Scan(&operationCount, &distinctOperation); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 || distinctAttempt != 1 || operationCount != 1 || distinctOperation != 1 {
		t.Fatalf("multiple funds paths: attempts=%d/%d operations=%d/%d", attemptCount, distinctAttempt, operationCount, distinctOperation)
	}
	stored, err := service.GetIntent(ctx, "merchant_concurrent", created.Intent.ID)
	if err != nil || stored.Intent.Status != domain.IntentSucceeded {
		t.Fatalf("winning payment did not converge: result=%+v err=%v", stored, err)
	}

	second, err := service.CreateIntent(ctx, "merchant_concurrent", "create_second", CreateIntentInput{
		MerchantReference: "order_second", AmountMinor: 2599, Currency: "USD",
		PaymentMethodToken: "pm_second", CaptureMethod: string(domain.CaptureAutomatic),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmIntent(ctx, "merchant_concurrent", second.Intent.ID, "confirm_concurrent"); !errors.Is(err, postgres.ErrIdempotencyConflict) {
		t.Fatalf("same key on another target error=%v, want idempotency conflict", err)
	}
	if gateway.executes.Load() != 1 {
		t.Fatalf("changed command reached funds execution: %d", gateway.executes.Load())
	}
	// A reconstructed service must replay the same durable command without
	// Router or Gateway execution; no in-memory deduplication is involved.
	restarted := NewService(postgres.NewStore(pool), router, gateway, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	replay, err := restarted.ConfirmIntent(ctx, "merchant_concurrent", created.Intent.ID, "confirm_concurrent")
	if err != nil || !replay.Replay || gateway.executes.Load() != 1 {
		t.Fatalf("restart replay=%+v executes=%d err=%v", replay, gateway.executes.Load(), err)
	}
}

type concurrentRouter struct{ decisions atomic.Int32 }

func (r *concurrentRouter) Decide(_ context.Context, request clients.RoutingRequest) (clients.RoutingDecision, error) {
	ordinal := r.decisions.Add(1)
	return clients.RoutingDecision{
		DecisionID: fmt.Sprintf("route_concurrent_%d", ordinal),
		SelectedCandidate: clients.RouteCandidate{
			CandidateID: "simulator_a_us_primary", ConnectionRevision: "simulator_a_us_primary_v1",
			ProviderNamespaceID: "simulator_a", Provider: "simulator_a",
			MerchantAccount: "local_account", Connector: "simulator", Region: "us",
		},
		CandidateOrder: []string{"simulator_a_us_primary"}, PolicyVersion: "local.v1",
	}, nil
}

type countingGateway struct{ executes atomic.Int32 }

func (g *countingGateway) Execute(_ context.Context, request clients.GatewayOperationRequest) (domain.ProviderOutcome, error) {
	g.executes.Add(1)
	return domain.ProviderOutcome{
		OperationID: request.OperationID, ProviderIdempotencyKey: request.OperationID,
		ProviderConnectionID: request.ProviderConnectionID, ProviderConnectionRevision: request.ProviderConnectionRevision,
		ProviderNamespaceID: request.ProviderNamespaceID, Provider: request.Provider,
		Status: domain.OutcomeSucceeded, SideEffectCertainty: domain.SideEffectConfirmed,
		ProviderPaymentReference:   "psp_payment_" + request.OperationID,
		ProviderOperationReference: "psp_operation_" + request.OperationID,
	}, nil
}

func (*countingGateway) Query(context.Context, clients.GatewayOperationIdentity) (domain.ProviderOutcome, error) {
	return domain.ProviderOutcome{}, fmt.Errorf("query was not expected")
}

func isolatedAppStore(t *testing.T) (*postgres.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("payment_idempotency_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+identifier+" CASCADE")
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return postgres.NewStore(pool), pool, ctx
}
