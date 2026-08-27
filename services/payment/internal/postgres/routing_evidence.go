package postgres

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

// validateRoutingEvidence executes inside the prepare transaction, against the
// locked Intent. It does not load current routing configuration to reinterpret
// a previously computed decision.
func validateRoutingEvidence(attempt domain.PaymentAttempt, intent domain.PaymentIntent, now time.Time) error {
	if len(attempt.RoutingContextSnapshot) == 0 && len(attempt.RoutingDecisionSnapshot) == 0 {
		if attempt.RoutingExecutableUntil == nil {
			return nil
		}
		return fmt.Errorf("%w: routing deadline has no snapshots", ErrStateConflict)
	}
	invalid := fmt.Errorf("%w: routing evidence does not bind the prepared attempt", ErrStateConflict)
	decoder := json.NewDecoder(bytes.NewBufferString(attempt.RoutingContextSnapshot))
	decoder.DisallowUnknownFields()
	var snapshot routingcontext.Context
	if err := decoder.Decode(&snapshot); err != nil {
		return invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid
	}
	digest, err := snapshot.Digest()
	contextUntil, authorityErr := snapshot.OnlineAuthorityUntil(routingcontext.OnlineMaxAge)
	if err != nil || authorityErr != nil || snapshot.ValidateOnline(now, routingcontext.OnlineMaxAge) != nil || snapshot.Base.PaymentID != intent.ID || snapshot.Base.AttemptID != attempt.ID ||
		snapshot.Base.MerchantID != intent.MerchantID || snapshot.Base.AmountMinor != intent.AmountMinor ||
		snapshot.Base.Currency != intent.Currency || snapshot.Base.CaptureMethod != string(intent.CaptureMethod) {
		return invalid
	}
	decisionDecoder := json.NewDecoder(bytes.NewBufferString(attempt.RoutingDecisionSnapshot))
	decisionDecoder.DisallowUnknownFields()
	var decision clients.RoutingDecision
	if err := decisionDecoder.Decode(&decision); err != nil {
		return invalid
	}
	if err := decisionDecoder.Decode(new(any)); err != io.EOF {
		return invalid
	}
	selected := decision.SelectedCandidate
	if decision.DecisionID != attempt.RoutingDecisionID || decision.ContextSchemaVersion != snapshot.SchemaVersion || decision.ContextDigest != digest ||
		attempt.RoutingExecutableUntil == nil || decision.ExecutableUntil.IsZero() ||
		!decision.ExecutableUntil.Equal(decision.ExecutableUntil.Truncate(time.Microsecond)) ||
		!attempt.RoutingExecutableUntil.UTC().Equal(decision.ExecutableUntil) || !now.Before(decision.ExecutableUntil) ||
		decision.ExecutableUntil.After(contextUntil) || !clients.ValidVersionedDecisionID(decision) ||
		selected.CandidateID != attempt.ProviderConnectionID || selected.ConnectionRevision != attempt.ProviderConnectionRevision ||
		selected.ProviderNamespaceID != attempt.ProviderNamespaceID || selected.Provider != attempt.Provider ||
		len(decision.CandidateOrder) == 0 || decision.CandidateOrder[0] != selected.CandidateID {
		return invalid
	}
	if snapshot.SchemaVersion == routingcontext.SchemaVersion {
		if decision.Risk != nil || len(decision.Optimization) != 0 {
			return invalid
		}
		return nil
	}
	if err := clients.ValidateOptimizedDecision(snapshot, decision, http.StatusOK); err != nil {
		return invalid
	}
	return nil
}
