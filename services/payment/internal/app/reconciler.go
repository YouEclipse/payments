package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
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
	now := s.now().UTC()
	lease := staleAge
	if minimumLease := 2 * s.upstreamTimeout; minimumLease > lease {
		lease = minimumLease
	}
	operations, err := s.store.ClaimRecoverableOperations(ctx, now, staleAge, lease, 50)
	if err != nil {
		s.logger.Error("list recoverable provider operations", slog.String("error", err.Error()))
		return
	}
	for _, recoverable := range operations {
		if ctx.Err() != nil {
			return
		}
		aggregate, err := s.store.GetAggregateByOperation(ctx, recoverable.OperationID)
		if err != nil {
			s.logger.Error("load recoverable provider operation", slog.String("operation_id", recoverable.OperationID), slog.String("error", err.Error()))
			continue
		}
		var outcome domain.ProviderOutcome
		switch recoverable.Status {
		case domain.OperationCreated, domain.OperationRetryScheduled:
			aggregate, err = s.store.MarkOperationSending(ctx, recoverable.OperationID, s.now().UTC())
			if err != nil {
				if !errors.Is(err, domain.ErrInvalidTransition) {
					s.logger.Error("claim provider operation", slog.String("operation_id", recoverable.OperationID), slog.String("error", err.Error()))
				}
				continue
			}
			outcome = s.executeGateway(ctx, aggregate)
		default:
			// A stale SENDING operation may have reached the provider before the
			// process died. Querying is safe; re-creating it is not.
			outcome = s.queryGateway(ctx, aggregate)
		}

		applied, applyErr := s.applyOutcome(ctx, outcome)
		if applyErr != nil && !errors.Is(applyErr, domain.ErrConflictingOutcome) {
			s.logger.Error("reconcile provider outcome", slog.String("operation_id", recoverable.OperationID), slog.String("error", applyErr.Error()))
			continue
		}
		if applied.Rejected {
			s.logger.Error("reconciled outcome was quarantined", slog.String("operation_id", recoverable.OperationID))
		}
	}
}
