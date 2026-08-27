package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/YouEclipse/payments/services/payment/internal/postgres"
	"github.com/YouEclipse/payments/services/payment/internal/recovery"
)

func (s *Service) executeRecovery(ctx context.Context, claim postgres.RecoverableOperation) error {
	now := s.now().UTC()
	if firstDispatchAuthorityExpired(claim, now) {
		return s.stopExpiredFirstDispatchRecovery(ctx, claim, now)
	}
	aggregate, err := s.store.LoadRecoveryClaim(ctx, claim, now)
	if err != nil {
		return err
	}
	command, err := pinnedGatewayCommand(aggregate)
	if err != nil {
		return s.completeRecoveryObservation(ctx, claim, gatewayFailureOutcome(aggregate, "recovery_invalid_", "gateway_execution_identity_conflict", err))
	}
	now = s.now().UTC()
	if !now.Before(claim.CallDeadline) {
		if firstDispatchAuthorityExpired(claim, now) {
			return s.stopExpiredFirstDispatchRecovery(ctx, claim, now)
		}
		return postgres.ErrRecoveryClaimLost
	}
	callDeadline := claim.CallDeadline
	if upstreamDeadline := now.Add(s.upstreamTimeout); upstreamDeadline.Before(callDeadline) {
		callDeadline = upstreamDeadline
	}
	// Use the persisted absolute boundary. Reconstructing it as a duration
	// would extend routing/proof authority by the time spent preparing the call.
	callContext, cancel := context.WithDeadline(ctx, callDeadline)
	defer cancel()
	var outcome domain.ProviderOutcome
	switch claim.Action {
	case recovery.Query:
		outcome, err = s.gateway.Query(callContext, clients.GatewayOperationIdentity{
			OperationID: command.OperationID, AttemptID: command.AttemptID,
			ProviderConnectionID: command.ProviderConnectionID, ProviderConnectionRevision: command.ProviderConnectionRevision,
			ProviderNamespaceID: command.ProviderNamespaceID, Provider: command.Provider,
			ProviderIdempotencyKey: aggregate.Operation.ProviderIdempotencyKey, RequestHash: clients.GatewayRequestHashV2(command),
		})
		var required *clients.GatewayRecoveryRequired
		if errors.As(err, &required) {
			evidence, evidenceErr := required.EvidenceFor(aggregate.Operation.RecoveryIdentity())
			if evidenceErr != nil {
				return s.completeRecoveryObservation(ctx, claim, gatewayFailureOutcome(aggregate, "recovery_proof_invalid_", "gateway_execution_identity_conflict", evidenceErr))
			}
			// Only this validated internal-client error may create durable
			// absence evidence; neither ReceiveProviderOutcome nor a 404 can.
			persistContext, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer persistCancel()
			return s.store.RecordRecoveryEvidence(persistContext, claim, evidence, s.now().UTC())
		}
	case recovery.SameKeyResubmit:
		now = s.now().UTC()
		if firstDispatchAuthorityExpired(claim, now) {
			cancel()
			return s.stopExpiredFirstDispatchRecovery(ctx, claim, now)
		}
		if err := s.store.Ready(callContext, s.maxClockSkew); err != nil {
			return err
		}
		now = s.now().UTC()
		if !now.Before(claim.CallDeadline) || firstDispatchAuthorityExpired(claim, now) {
			if firstDispatchAuthorityExpired(claim, now) {
				cancel()
				return s.stopExpiredFirstDispatchRecovery(ctx, claim, now)
			}
			return postgres.ErrRecoveryClaimLost
		}
		outcome, err = s.gateway.Execute(callContext, command)
	default:
		return errors.New("a non-executable recovery action was claimed")
	}
	if err != nil {
		outcome = gatewayFailureOutcome(aggregate, "recovery_unknown_", "gateway_recovery_unknown", err)
	} else {
		outcome.EventID = newBestEffortEventID("recovery_")
	}
	return s.completeRecoveryObservation(ctx, claim, outcome)
}

func firstDispatchAuthorityExpired(claim postgres.RecoverableOperation, now time.Time) bool {
	return claim.Basis == recovery.ClaimFirstGatewayIO && claim.FirstDispatchExecutableUntil != nil &&
		!now.Before(claim.FirstDispatchExecutableUntil.UTC())
}

func (s *Service) stopExpiredFirstDispatchRecovery(ctx context.Context, claim postgres.RecoverableOperation, now time.Time) error {
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err := s.store.StopRecoveryForReview(persistContext, claim, now)
	return err
}

func (s *Service) completeRecoveryObservation(ctx context.Context, claim postgres.RecoverableOperation, outcome domain.ProviderOutcome) error {
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	// The observation remains evidence even when a worker lost its lease.
	// Domain conflict/review rules, never worker ownership, decide funds state.
	result, err := s.applyOutcome(persistContext, outcome)
	if err != nil && !errors.Is(err, domain.ErrConflictingOutcome) {
		return err
	}
	if result.Aggregate.Operation.Status == domain.OperationSucceeded || result.Aggregate.Operation.Status == domain.OperationFailed || result.Aggregate.Operation.Status == domain.OperationReviewRequired {
		return nil
	}
	return s.store.ReleaseRecoveryClaim(persistContext, claim, s.now().UTC())
}

func pinnedGatewayCommand(aggregate postgres.Aggregate) (clients.GatewayOperationRequest, error) {
	op, attempt, intent := aggregate.Operation, aggregate.Attempt, aggregate.Intent
	if op.PaymentIntentID != intent.ID || op.PaymentAttemptID != attempt.ID || attempt.PaymentIntentID != intent.ID ||
		op.ProviderConnectionID != attempt.ProviderConnectionID || op.ProviderConnectionRevision != attempt.ProviderConnectionRevision ||
		op.ProviderNamespaceID != attempt.ProviderNamespaceID || op.Provider != attempt.Provider ||
		op.ProviderIdempotencyKey != op.ID || op.AmountMinor != intent.AmountMinor || op.Currency != intent.Currency ||
		op.Kind != domain.OperationSale || intent.CaptureMethod != domain.CaptureAutomatic {
		return clients.GatewayOperationRequest{}, fmt.Errorf("%w: durable payment aggregate identity differs", clients.ErrGatewayIdentityMismatch)
	}
	command := clients.GatewayOperationRequest{
		OperationID: op.ID, AttemptID: attempt.ID, ProviderConnectionID: op.ProviderConnectionID,
		ProviderConnectionRevision: op.ProviderConnectionRevision, ProviderNamespaceID: op.ProviderNamespaceID,
		Provider: op.Provider, AmountMinor: op.AmountMinor, Currency: op.Currency,
		PaymentMethodToken: intent.PaymentMethodToken, CaptureMethod: string(intent.CaptureMethod),
	}
	encoded, err := json.Marshal(command)
	if err != nil || !idempotency.Equal(op.RequestHash, idempotency.Fingerprint("provider_sale", encoded)) {
		return clients.GatewayOperationRequest{}, fmt.Errorf("%w: original request fingerprint differs", clients.ErrGatewayIdentityMismatch)
	}
	return command, nil
}
