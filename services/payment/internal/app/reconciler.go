package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/postgres"
)

// RunReconciler closes the crash window between a durable operation and a
// synchronous gateway response. It never creates a second operation or uses a
// different provider idempotency key.
func (s *Service) RunReconciler(ctx context.Context, pollInterval, staleAge time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileBatch(ctx, staleAge)
		}
	}
}

func (s *Service) reconcileBatch(ctx context.Context, staleAge time.Duration) {
	lease := staleAge
	if minimumLease := 2 * s.upstreamTimeout; minimumLease > lease {
		lease = minimumLease
	}
	// Reserve one call just before executing it. Leasing a sequential batch
	// of fifty would spend budgets while later entries expire in our queue.
	for completed := 0; completed < 50; completed++ {
		if ctx.Err() != nil {
			return
		}
		if err := s.store.Ready(ctx, s.maxClockSkew); err != nil {
			s.logger.Error("recovery paused because database clock witness is unavailable", slog.String("error", err.Error()))
			return
		}
		operations, err := s.store.ClaimRecoverableOperations(ctx, s.now().UTC(), staleAge, lease, 1)
		if err != nil {
			s.logger.Error("claim recoverable provider operation", slog.String("error", err.Error()))
			return
		}
		if len(operations) == 0 {
			return
		}
		if err := s.executeRecovery(ctx, operations[0]); err != nil && !errors.Is(err, postgres.ErrRecoveryClaimLost) {
			s.logger.Error("recover provider operation", slog.String("operation_id", operations[0].OperationID), slog.String("error", err.Error()))
		}
	}
}
