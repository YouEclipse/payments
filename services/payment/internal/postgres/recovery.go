package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/recovery"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryClaimLost = errors.New("recovery claim is no longer current")

type RecoverableOperation struct {
	OperationID                  string
	Status                       domain.OperationStatus
	UpdatedAt                    time.Time
	Action                       recovery.Action
	Basis                        recovery.ClaimBasis
	Reason                       string
	Generation                   int64
	Identity                     recovery.Identity
	LeaseUntil                   time.Time
	CallDeadline                 time.Time
	FirstDispatchExecutableUntil *time.Time
}

// ClaimRecoverableOperations reserves calls, not money outcomes. Planning,
// budget debit, dispatch marker (when needed), and lease are one transaction.
// A crash after this commit can waste one call allowance; it cannot refund it.
func (s *Store) ClaimRecoverableOperations(ctx context.Context, now time.Time, staleAge, lease time.Duration, limit int) ([]RecoverableOperation, error) {
	if now.IsZero() || staleAge < 0 || lease <= 0 || limit <= 0 || limit > 100 {
		return nil, errors.New("invalid recovery claim bounds")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin recoverable operation claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id FROM provider_operations
		WHERE status IN ('created','sending','pending','unknown','retry_scheduled')
		  AND (recovery_lease_until IS NULL OR recovery_lease_until <= $2)
		  AND (
		      (next_retry_at IS NULL AND updated_at <= $1)
		      OR next_retry_at <= $2
		      OR recovery_deadline_at <= $2
		      OR recovery_budget_remaining = 0
		  )
		ORDER BY COALESCE(next_retry_at,updated_at),id
		FOR UPDATE SKIP LOCKED LIMIT $3`, now.Add(-staleAge), now, limit)
	if err != nil {
		return nil, fmt.Errorf("select recoverable operations for claim: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var claims []RecoverableOperation
	for _, id := range ids {
		aggregate, err := getAggregateByOperationTx(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		op := &aggregate.Operation
		// STOP is a durable authority boundary. If an old writer or a late
		// nonterminal observation ever leaves status/action inconsistent, restore
		// review rather than silently reactivating automatic Provider I/O.
		if op.RecoveryAction == recovery.Stop {
			reason := op.RecoveryStopReason
			if reason == "" {
				reason = "manual_review_required"
			}
			if err := stopRecoveryTx(ctx, tx, &aggregate, reason, now); err != nil {
				return nil, err
			}
			continue
		}
		plan := recovery.Decide(recovery.State{
			Identity: op.RecoveryIdentity(), Status: string(op.Status), Certainty: string(op.SideEffectCertainty),
			PolicyVersion: op.RecoveryPolicyVersion, FirstDispatchedAt: op.FirstDispatchedAt,
			DeadlineAt: op.RecoveryDeadlineAt, BudgetRemaining: op.RecoveryBudgetRemaining,
			NextRetryAt: op.NextRetryAt, LeaseUntil: op.RecoveryLeaseUntil, Absence: op.RecoveryEvidence,
		}, now, lease/2)
		if plan.Action == recovery.Stop {
			if plan.RequiresReview {
				if err := stopRecoveryTx(ctx, tx, &aggregate, plan.Reason, now); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !plan.ConsumesBudget() {
			continue
		}
		neverDispatched := plan.Action == recovery.SameKeyResubmit && op.Status == domain.OperationCreated &&
			op.SideEffectCertainty == domain.SideEffectNone && op.FirstDispatchedAt == nil
		gatewayProvedNoDispatch := plan.Action == recovery.SameKeyResubmit && plan.Basis == recovery.ClaimFirstGatewayIO && !neverDispatched
		var firstDispatchExecutableUntil *time.Time
		if neverDispatched || gatewayProvedNoDispatch {
			versionedRouting := aggregate.Attempt.RoutingContextSnapshot != "" || aggregate.Attempt.RoutingDecisionSnapshot != ""
			if validateRoutingEvidence(aggregate.Attempt, aggregate.Intent, now) != nil ||
				(versionedRouting && (aggregate.Attempt.RoutingExecutableUntil == nil ||
					!now.Before(aggregate.Attempt.RoutingExecutableUntil.UTC()))) {
				reason := "routing_authority_expired_before_first_dispatch"
				if gatewayProvedNoDispatch {
					reason = "routing_authority_expired_before_first_gateway_io"
				}
				if err := stopRecoveryTx(ctx, tx, &aggregate, reason, now); err != nil {
					return nil, err
				}
				continue
			}
			if versionedRouting {
				executableUntil := aggregate.Attempt.RoutingExecutableUntil.UTC()
				firstDispatchExecutableUntil = &executableUntil
			}
		}
		previousVersion := op.Version
		if plan.Action == recovery.SameKeyResubmit {
			if err := domain.MarkRecoverySending(&aggregate.Intent, &aggregate.Attempt, op, now); err != nil {
				return nil, err
			}
		} else {
			// Expired evidence can only lead to another query, never be revived.
			op.RecoveryEvidence = nil
		}
		leaseUntil := now.Add(lease)
		if leaseUntil.After(op.RecoveryDeadlineAt) {
			leaseUntil = op.RecoveryDeadlineAt
		}
		op.RecoveryBudgetRemaining--
		op.RecoveryClaimGeneration++
		op.RecoveryClaimBasis = plan.Basis
		op.RecoveryAction = plan.Action
		op.RecoveryStopReason = ""
		op.RecoveryLeaseUntil = &leaseUntil
		next := recovery.NextAttemptAt(op.ID, recovery.InitialBudget-op.RecoveryBudgetRemaining, now, op.RecoveryDeadlineAt)
		op.NextRetryAt = &next
		op.Version = previousVersion + 1
		op.UpdatedAt = now.UTC()
		if err := updateAggregateTx(ctx, tx, aggregate); err != nil {
			return nil, err
		}
		callDeadline := now.Add(plan.CallTimeout)
		if firstDispatchExecutableUntil != nil && firstDispatchExecutableUntil.Before(callDeadline) {
			callDeadline = *firstDispatchExecutableUntil
		}
		claim := RecoverableOperation{
			OperationID: op.ID, Status: op.Status, UpdatedAt: op.UpdatedAt,
			Action: plan.Action, Basis: plan.Basis, Reason: plan.Reason, Generation: op.RecoveryClaimGeneration,
			Identity: op.RecoveryIdentity(), LeaseUntil: leaseUntil, CallDeadline: callDeadline,
			FirstDispatchExecutableUntil: firstDispatchExecutableUntil,
		}
		claims = append(claims, claim)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit recoverable operation claim: %w", err)
	}
	return claims, nil
}

// LoadRecoveryClaim is a last pre-I/O check, not an external HTTP fence.
// Provider idempotency and the Gateway's own recheck remain mandatory.
func (s *Store) LoadRecoveryClaim(ctx context.Context, claim RecoverableOperation, now time.Time) (Aggregate, error) {
	aggregate, err := s.GetAggregateByOperation(ctx, claim.OperationID)
	if err != nil {
		return Aggregate{}, err
	}
	if !claimMatches(aggregate.Operation, claim, now) || !now.Before(claim.CallDeadline) {
		return Aggregate{}, ErrRecoveryClaimLost
	}
	return aggregate, nil
}

// StopRecoveryForReview closes the narrow crash window in which a durable
// never-dispatched claim recorded its dispatch marker, but routing authority
// expired before the Gateway call began. The generation/identity/lease CAS is
// still required; an expired call deadline alone does not grant this method
// authority over a newer worker or over an already-dispatched recovery.
func (s *Store) StopRecoveryForReview(ctx context.Context, claim RecoverableOperation, now time.Time) (Aggregate, error) {
	return retryTransaction(ctx, func() (Aggregate, error) {
		return s.stopRecoveryForReviewOnce(ctx, claim, now)
	})
}

func (s *Store) stopRecoveryForReviewOnce(ctx context.Context, claim RecoverableOperation, now time.Time) (Aggregate, error) {
	if claim.Action != recovery.SameKeyResubmit || claim.Basis != recovery.ClaimFirstGatewayIO ||
		claim.FirstDispatchExecutableUntil == nil ||
		now.Before(claim.FirstDispatchExecutableUntil.UTC()) {
		return Aggregate{}, ErrRecoveryClaimLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Aggregate{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	aggregate, err := getAggregateByOperationTx(ctx, tx, claim.OperationID, true)
	if err != nil {
		return Aggregate{}, err
	}
	deadline := aggregate.Attempt.RoutingExecutableUntil
	if !claimOwnershipMatches(aggregate.Operation, claim, now) ||
		aggregate.Operation.Status != domain.OperationSending || aggregate.Operation.FirstDispatchedAt == nil ||
		aggregate.Operation.RecoveryClaimBasis != recovery.ClaimFirstGatewayIO ||
		deadline == nil || !deadline.UTC().Equal(claim.FirstDispatchExecutableUntil.UTC()) || now.Before(deadline.UTC()) ||
		aggregate.Attempt.RoutingContextSnapshot == "" || aggregate.Attempt.RoutingDecisionSnapshot == "" {
		return Aggregate{}, ErrRecoveryClaimLost
	}
	if err := stopRecoveryTx(ctx, tx, &aggregate, "routing_authority_expired_before_first_gateway_io", now); err != nil {
		return Aggregate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Aggregate{}, err
	}
	return aggregate, nil
}

// StopInitialDispatchForReview is the synchronous-path equivalent. The
// dispatch marker is intentionally retained: after the atomic sending
// transition the absence of an HTTP call cannot be represented as a provider
// failure, so the operation remains unresolved and quarantined for review.
func (s *Store) StopInitialDispatchForReview(ctx context.Context, operationID string, expectedVersion int64, now time.Time) (Aggregate, error) {
	return retryTransaction(ctx, func() (Aggregate, error) {
		return s.stopInitialDispatchForReviewOnce(ctx, operationID, expectedVersion, now)
	})
}

func (s *Store) stopInitialDispatchForReviewOnce(ctx context.Context, operationID string, expectedVersion int64, now time.Time) (Aggregate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Aggregate{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	aggregate, err := getAggregateByOperationTx(ctx, tx, operationID, true)
	if err != nil {
		return Aggregate{}, err
	}
	deadline := aggregate.Attempt.RoutingExecutableUntil
	if aggregate.Operation.Version != expectedVersion || aggregate.Operation.Status != domain.OperationSending ||
		aggregate.Operation.FirstDispatchedAt == nil || deadline == nil || now.Before(deadline.UTC()) ||
		aggregate.Attempt.RoutingContextSnapshot == "" || aggregate.Attempt.RoutingDecisionSnapshot == "" {
		return Aggregate{}, ErrRecoveryClaimLost
	}
	if err := stopRecoveryTx(ctx, tx, &aggregate, "routing_authority_expired_before_first_gateway_io", now); err != nil {
		return Aggregate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Aggregate{}, err
	}
	return aggregate, nil
}

// ReleaseRecoveryClaim never resets a budget or deadline. A stale completion
// cannot clear a newer lease. Provider observations are persisted separately:
// a late captured result is still evidence even if its worker lost the lease.
func (s *Store) ReleaseRecoveryClaim(ctx context.Context, claim RecoverableOperation, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE provider_operations
		SET recovery_lease_until=NULL,recovery_claim_basis='none',recovery_action='wait',version=version+1,updated_at=$5
		WHERE id=$1 AND recovery_claim_generation=$2 AND recovery_action=$3
		  AND recovery_claim_basis=$4 AND recovery_lease_until > $5`, claim.OperationID, claim.Generation, claim.Action, claim.Basis, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrRecoveryClaimLost
	}
	return nil
}

// RecordRecoveryEvidence is used only after the authenticated Gateway client
// validated a structured proof against the original command. There is no
// merchant/webhook endpoint for this transition. It schedules a separately
// charged future action; the query that obtained it never executes a sale.
func (s *Store) RecordRecoveryEvidence(ctx context.Context, claim RecoverableOperation, evidence recovery.AbsenceEvidence, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	aggregate, err := getAggregateByOperationTx(ctx, tx, claim.OperationID, true)
	if err != nil {
		return err
	}
	if claim.Action != recovery.Query || !claimMatches(aggregate.Operation, claim, now) {
		return ErrRecoveryClaimLost
	}
	if !evidence.ValidFor(aggregate.Operation.RecoveryIdentity(), now) || !now.Before(aggregate.Operation.RecoveryDeadlineAt) {
		return errors.New("recovery evidence does not bind a fresh original operation")
	}
	aggregate.Operation.RecoveryEvidence = &evidence
	aggregate.Operation.RecoveryLeaseUntil = nil
	aggregate.Operation.RecoveryClaimBasis = recovery.ClaimNone
	aggregate.Operation.RecoveryAction = recovery.Wait
	aggregate.Operation.NextRetryAt = &now
	aggregate.Operation.Version++
	aggregate.Operation.UpdatedAt = now.UTC()
	if err := updateAggregateTx(ctx, tx, aggregate); err != nil {
		return err
	}
	if err := insertOutbox(ctx, tx, "provider_operation", claim.OperationID, aggregate.Operation.Version,
		"provider_operation.recovery_evidence_recorded", map[string]any{
			"operation_id": claim.OperationID, "claim_generation": claim.Generation, "evidence": evidence,
		}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func claimMatches(operation domain.ProviderOperation, claim RecoverableOperation, now time.Time) bool {
	return claimOwnershipMatches(operation, claim, now) && now.Before(claim.CallDeadline)
}

func claimOwnershipMatches(operation domain.ProviderOperation, claim RecoverableOperation, now time.Time) bool {
	return operation.RecoveryIdentity() == claim.Identity && operation.RecoveryClaimGeneration == claim.Generation &&
		operation.RecoveryClaimBasis == claim.Basis && operation.RecoveryAction == claim.Action &&
		operation.RecoveryLeaseUntil != nil && now.Before(*operation.RecoveryLeaseUntil) &&
		operation.Status != domain.OperationSucceeded && operation.Status != domain.OperationFailed && operation.Status != domain.OperationReviewRequired
}

func stopRecoveryTx(ctx context.Context, tx pgx.Tx, aggregate *Aggregate, reason string, now time.Time) error {
	apply, err := requireAggregateReviewTx(ctx, tx, aggregate, reason, "automatic recovery stopped; the funds outcome remains unresolved", now)
	if err != nil || !apply.Changed {
		return err
	}
	return insertOutbox(ctx, tx, "provider_operation", aggregate.Operation.ID, aggregate.Operation.Version,
		"provider_operation.recovery_stopped", map[string]any{
			"operation_id": aggregate.Operation.ID, "reason": reason,
			"recovery_deadline_at":      aggregate.Operation.RecoveryDeadlineAt,
			"recovery_budget_remaining": aggregate.Operation.RecoveryBudgetRemaining,
		}, now)
}

func marshalRecoveryEvidence(evidence *recovery.AbsenceEvidence) ([]byte, error) {
	if evidence == nil {
		return nil, nil
	}
	return json.Marshal(evidence)
}
