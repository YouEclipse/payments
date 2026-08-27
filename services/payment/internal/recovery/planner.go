// Package recovery plans recovery of one already-pinned funds operation.
// It has no network, persistence, routing, or clock side effects.
package recovery

import (
	"encoding/binary"
	"hash/fnv"
	"strings"
	"time"
)

type Action string

const (
	Query           Action = "query"
	Wait            Action = "wait"
	SameKeyResubmit Action = "same_key_resubmit"
	Stop            Action = "stop"
)

// ClaimBasis is persisted with a recovery generation. It prevents a caller
// from reinterpreting a provider-proven resubmit as a first-Gateway-I/O claim
// (or vice versa) merely by changing an in-memory claim envelope.
type ClaimBasis string

const (
	ClaimNone             ClaimBasis = "none"
	ClaimQuery            ClaimBasis = "query"
	ClaimFirstGatewayIO   ClaimBasis = "first_gateway_io"
	ClaimProviderRecovery ClaimBasis = "provider_recovery"
)

const (
	PolicyVersion = "local_sale_recovery.v1"
	Window        = 24 * time.Hour
	InitialBudget = 32
)

// Window and InitialBudget are this repository's local SALE recovery policy,
// not a claim about any production provider's idempotency retention period.

type Identity struct {
	OperationID        string `json:"operation_id"`
	AttemptID          string `json:"attempt_id"`
	ConnectionID       string `json:"provider_connection_id"`
	ConnectionRevision string `json:"provider_connection_revision"`
	NamespaceID        string `json:"provider_namespace_id"`
	Provider           string `json:"provider"`
	Key                string `json:"provider_idempotency_key"`
	PayloadHash        string `json:"payload_hash"`
}

func (identity Identity) Valid() bool {
	for _, value := range []string{
		identity.OperationID, identity.AttemptID, identity.ConnectionID,
		identity.ConnectionRevision, identity.NamespaceID, identity.Provider,
		identity.Key, identity.PayloadHash,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
			return false
		}
	}
	return true
}

type AbsenceKind string

const (
	GatewayOperationAbsent   AbsenceKind = "gateway_operation_absent"
	GatewayNeverDispatched   AbsenceKind = "gateway_never_dispatched"
	ProviderConclusiveAbsent AbsenceKind = "provider_conclusive_absent"
)

// AbsenceEvidence is admitted only by the authenticated Gateway client's
// structured-response validator, never by a merchant command or webhook.
// Its half-open validity window is independent from an operation's deadline.
type AbsenceEvidence struct {
	Kind       AbsenceKind `json:"kind"`
	Identity   Identity    `json:"identity"`
	ObservedAt time.Time   `json:"observed_at"`
	ValidUntil time.Time   `json:"valid_until"`
}

func (e AbsenceEvidence) ValidFor(identity Identity, now time.Time) bool {
	return e.kindValid() && identity.Valid() && e.Identity == identity &&
		!e.ObservedAt.IsZero() && e.ValidUntil.After(e.ObservedAt) &&
		!now.Before(e.ObservedAt) && now.Before(e.ValidUntil)
}

func (e AbsenceEvidence) kindValid() bool {
	return e.Kind == GatewayOperationAbsent || e.Kind == GatewayNeverDispatched || e.Kind == ProviderConclusiveAbsent
}

type State struct {
	Identity          Identity
	Status            string
	Certainty         string
	PolicyVersion     string
	FirstDispatchedAt *time.Time
	DeadlineAt        time.Time
	BudgetRemaining   int
	NextRetryAt       *time.Time
	LeaseUntil        *time.Time
	Absence           *AbsenceEvidence
}

type Plan struct {
	Action         Action
	Basis          ClaimBasis
	Reason         string
	RequiresReview bool
	NotBefore      time.Time
	CallTimeout    time.Duration
}

func (p Plan) ConsumesBudget() bool { return p.Action == Query || p.Action == SameKeyResubmit }

// Decide does not grant an external exactly-once or lease fencing guarantee.
// An expired worker can still reach HTTP. A resubmit remains safe only under
// the original provider key/payload and the adapter's idempotency contract.
func Decide(state State, now time.Time, maxCallDuration time.Duration) Plan {
	switch state.Status {
	case "succeeded", "failed":
		return Plan{Action: Stop, Basis: ClaimNone, Reason: "terminal_operation"}
	case "review_required":
		return Plan{Action: Stop, Basis: ClaimNone, Reason: "manual_review_required"}
	case "created", "sending", "pending", "unknown", "retry_scheduled":
	default:
		return review("invalid_recovery_state")
	}
	if !state.Identity.Valid() || state.PolicyVersion != PolicyVersion ||
		state.DeadlineAt.IsZero() || now.IsZero() || state.BudgetRemaining < 0 ||
		maxCallDuration <= 0 || (state.Certainty != "none" && state.Certainty != "possible" && state.Certainty != "confirmed") {
		return review("invalid_recovery_state")
	}
	// Let the last reserved call finish before declaring its exhausted budget.
	// Completion is fenced separately by the durable claim generation.
	if state.LeaseUntil != nil && now.Before(*state.LeaseUntil) {
		return Plan{Action: Wait, Basis: ClaimNone, Reason: "recovery_lease_active", NotBefore: *state.LeaseUntil}
	}
	if !now.Before(state.DeadlineAt) {
		return review("recovery_deadline_exhausted")
	}
	if state.BudgetRemaining == 0 {
		return review("recovery_budget_exhausted")
	}
	if state.NextRetryAt != nil && now.Before(*state.NextRetryAt) {
		return Plan{Action: Wait, Basis: ClaimNone, Reason: "recovery_backoff", NotBefore: minTime(*state.NextRetryAt, state.DeadlineAt)}
	}
	callTimeout := min(maxCallDuration, state.DeadlineAt.Sub(now))
	if state.Status == "created" && state.Certainty == "none" && state.FirstDispatchedAt == nil {
		return Plan{Action: SameKeyResubmit, Basis: ClaimFirstGatewayIO, Reason: "durable_never_dispatched", CallTimeout: callTimeout}
	}
	if state.Absence != nil {
		evidence := *state.Absence
		if !evidence.kindValid() || evidence.Identity != state.Identity ||
			evidence.ObservedAt.IsZero() || !evidence.ValidUntil.After(evidence.ObservedAt) || now.Before(evidence.ObservedAt) {
			return review("recovery_evidence_invalid")
		}
		if evidence.ValidFor(state.Identity, now) {
			basis := ClaimProviderRecovery
			if evidence.Kind == GatewayOperationAbsent || evidence.Kind == GatewayNeverDispatched {
				basis = ClaimFirstGatewayIO
			}
			return Plan{Action: SameKeyResubmit, Basis: basis, Reason: string(evidence.Kind), CallTimeout: min(callTimeout, evidence.ValidUntil.Sub(now))}
		}
	}
	return Plan{Action: Query, Basis: ClaimQuery, Reason: "side_effect_not_disproved", CallTimeout: callTimeout}
}

func review(reason string) Plan {
	return Plan{Action: Stop, Basis: ClaimNone, Reason: reason, RequiresReview: true}
}

// NextAttemptAt uses deterministic bounded jitter, so restarting a worker
// neither invents a new schedule nor synchronizes every operation at a power
// of two. The stored timestamp, not this function, is the scheduling authority.
func NextAttemptAt(operationID string, ordinal int, now, deadline time.Time) time.Time {
	ordinal = max(ordinal, 1)
	ceiling := 2 * time.Second * time.Duration(uint64(1)<<min(ordinal-1, 7))
	floor := ceiling / 2
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(operationID))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(ordinal))
	_, _ = hash.Write(encoded[:])
	delay := floor + time.Duration(hash.Sum64()%uint64(ceiling-floor+1))
	return minTime(now.Add(delay), deadline)
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
