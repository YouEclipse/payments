package postgres

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

// validateRoutingEvidence executes inside the prepare transaction, against the
// locked Intent. It does not load current routing configuration to reinterpret
// a previously computed decision.
func validateRoutingEvidence(attempt domain.PaymentAttempt, intent domain.PaymentIntent) error {
	if len(attempt.RoutingContextSnapshot) == 0 && len(attempt.RoutingDecisionSnapshot) == 0 {
		return nil
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
	if err != nil || snapshot.Base.PaymentID != intent.ID || snapshot.Base.AttemptID != attempt.ID ||
		snapshot.Base.MerchantID != intent.MerchantID || snapshot.Base.AmountMinor != intent.AmountMinor ||
		snapshot.Base.Currency != intent.Currency || snapshot.Base.CaptureMethod != string(intent.CaptureMethod) {
		return invalid
	}
	var decision struct {
		ID       string `json:"decision_id"`
		Schema   string `json:"context_schema_version"`
		Digest   string `json:"context_digest"`
		Selected struct {
			ID        string `json:"candidate_id"`
			Revision  string `json:"connection_revision"`
			Namespace string `json:"provider_namespace_id"`
			Provider  string `json:"provider"`
		} `json:"selected_candidate"`
	}
	if err := json.Unmarshal([]byte(attempt.RoutingDecisionSnapshot), &decision); err != nil {
		return invalid
	}
	if decision.ID != attempt.RoutingDecisionID || decision.Schema != routingcontext.SchemaVersion || decision.Digest != digest ||
		decision.Selected.ID != attempt.ProviderConnectionID || decision.Selected.Revision != attempt.ProviderConnectionRevision ||
		decision.Selected.Namespace != attempt.ProviderNamespaceID || decision.Selected.Provider != attempt.Provider {
		return invalid
	}
	return nil
}
