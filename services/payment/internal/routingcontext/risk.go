package routingcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
)

type RiskInput struct {
	SubjectDigest string
	MerchantID    string
	AmountMinor   int64
	Currency      string
}

type RiskSource interface {
	Evaluate(context.Context, RiskInput, time.Time) (riskdecision.Decision, error)
}

// WithRisk returns another builder sharing only immutable profiles/metadata.
// No source means the retained Context v1 writer; callers choose explicitly.
func (b *Builder) WithRisk(source RiskSource) (*Builder, error) {
	if b == nil || source == nil {
		return nil, ErrInvalidContext
	}
	result := *b
	result.risk = source
	return &result, nil
}

// RiskSubjectDigest excludes the risk result and envelope construction time,
// avoiding a circular digest. It binds the entire approved business projection,
// including payment/attempt, amount/currency and field provenance. The full
// Context digest separately binds BuiltAt and the returned Risk Decision.
func (c Context) RiskSubjectDigest() (string, error) {
	payload, err := json.Marshal(struct {
		SchemaVersion string        `json:"schema_version"`
		Base          Base          `json:"base"`
		Merchant      Merchant      `json:"merchant"`
		PaymentMethod PaymentMethod `json:"payment_method_context"`
	}{"risk_subject.v1", c.Base, c.Merchant, c.PaymentMethod})
	if err != nil {
		return "", ErrInvalidContext
	}
	hash := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}

// DevelopmentRisk is an explicit local/test fixture, not a fraud model or
// production authorization. No payment token is accepted by this interface.
type DevelopmentRisk struct{}

func (DevelopmentRisk) Evaluate(ctx context.Context, input RiskInput, at time.Time) (riskdecision.Decision, error) {
	if err := ctx.Err(); err != nil {
		return riskdecision.Decision{}, err
	}
	if !riskdecision.ValidDigest(input.SubjectDigest) {
		return riskdecision.Decision{}, riskdecision.ErrInvalid
	}
	action, reason := riskdecision.Allow, "local_fixture_allow"
	switch input.AmountMinor {
	case 49001:
		action, reason = riskdecision.Block, "local_fixture_block"
	case 49002:
		action, reason = riskdecision.Require3DS, "local_fixture_authentication"
	case 49003:
		action, reason = riskdecision.ManualReview, "local_fixture_review"
	}
	decision := riskdecision.Decision{SchemaVersion: riskdecision.SchemaVersion,
		SubjectDigest: input.SubjectDigest, Action: action, ReasonCodes: []string{reason}, Version: "local_risk_v1",
		Source: "local_risk_fixture", EvaluatedAt: at.UTC(), ExpiresAt: at.UTC().Add(5 * time.Minute)}
	canonical, err := json.Marshal(decision)
	if err != nil {
		return riskdecision.Decision{}, riskdecision.ErrInvalid
	}
	hash := sha256.Sum256(canonical)
	decision.DecisionID = "risk_" + hex.EncodeToString(hash[:16])
	return decision, nil
}

// UnavailableRisk never turns absent external fraud evidence into ALLOW.
type UnavailableRisk struct{}

func (UnavailableRisk) Evaluate(context.Context, RiskInput, time.Time) (riskdecision.Decision, error) {
	return riskdecision.Decision{}, errors.New("risk source unavailable")
}
