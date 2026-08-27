// Package scoring combines complete, already-allowed candidate evidence. It
// cannot add a candidate, rerun eligibility, override a risk action, or execute
// a provider operation. All evaluation times are explicit inputs.
package scoring

import (
	"errors"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/performance"
)

const (
	SchemaVersion              = "scoring_profile.v1"
	PrimaryFull                = "full_v1"
	StaticOrder                = "static_order"
	Reject                     = "reject"
	StatusScored               = "scored"
	StatusStaticFallback       = "static_fallback"
	StatusRejected             = "rejected"
	PointScale           int64 = 10_000
	RoundingVersion            = "success_half_up_cost_floor_contribution_floor.v1"
	ObjectiveVersion           = "conservative_success_and_success_scenario_cost.v1"
)

var (
	ErrInvalidProfile      = errors.New("invalid scoring profile")
	ErrProfileNotEffective = errors.New("scoring profile is not effective")
	ErrInvalidBatch        = errors.New("invalid scoring batch")
)

// PrimaryFull always requires BOTH feature stages, even if one weight is zero.
// OnUnavailable is a whole-batch action, never a per-candidate fallback.
type Profile struct {
	SchemaVersion     string    `json:"schema_version"`
	Version           string    `json:"version"`
	PublishedAt       time.Time `json:"published_at"`
	EffectiveFrom     time.Time `json:"effective_from"`
	ExpiresAt         time.Time `json:"expires_at"`
	QuoteCurrency     string    `json:"quote_currency"`
	SuccessWeightBPS  int64     `json:"success_weight_bps"`
	CostWeightBPS     int64     `json:"cost_weight_bps"`
	CostFloorMicros   int64     `json:"cost_floor_micros"`
	CostCeilingMicros int64     `json:"cost_ceiling_micros"`
	Primary           string    `json:"primary"`
	OnUnavailable     string    `json:"on_unavailable"`
}

// Candidate is admitted by the preceding Context/eligibility/rule/risk gates.
// RuleRank is the stable rank from that same frozen rule evaluation. Equal
// ranks are allowed and are resolved by canonical candidate ID, not input order.
type Candidate struct {
	ID                 string `json:"id"`
	ConnectionRevision string `json:"connection_revision"`
	RuleRank           int    `json:"rule_rank"`
}

type Batch struct {
	Allowed     []Candidate          `json:"allowed"`
	Costs       []cost.Result        `json:"costs"`
	Performance []performance.Result `json:"performance"`
}

// Numbers contains only bounded integer projections and contributions. Micros
// are quote-currency MAJOR-unit millionths. The two raw objectives remain
// separate: this weighted score is not expected utility or cost per success.
type Numbers struct {
	SucceededCostMicros int64  `json:"succeeded_cost_micros"`
	QuoteCurrency       string `json:"quote_currency"`
	ConservativePPM     int64  `json:"conservative_ppm"`
	CostFloorMicros     int64  `json:"cost_floor_micros"`
	CostCeilingMicros   int64  `json:"cost_ceiling_micros"`
	ClampedCostMicros   int64  `json:"clamped_cost_micros"`
	SuccessWeightBPS    int64  `json:"success_weight_bps"`
	CostWeightBPS       int64  `json:"cost_weight_bps"`
	SuccessPoints       int64  `json:"success_points"`
	CostPoints          int64  `json:"cost_points"`
	SuccessContribution int64  `json:"success_contribution"`
	CostContribution    int64  `json:"cost_contribution"`
	TotalPoints         int64  `json:"total_points"`
	RoundingVersion     string `json:"rounding_version"`
}

type ScoreBreakdown struct {
	CandidateID        string             `json:"candidate_id"`
	ConnectionRevision string             `json:"connection_revision"`
	RuleRank           int                `json:"rule_rank"`
	ProfileVersion     string             `json:"profile_version"`
	ProfileDigest      string             `json:"profile_digest"`
	Cost               cost.Result        `json:"cost"`
	Performance        performance.Result `json:"performance"`
	Numbers            *Numbers           `json:"numbers,omitempty"`
}

type Result struct {
	Status           string           `json:"status"`
	Mode             string           `json:"mode"`
	CandidateOrder   []string         `json:"candidate_order"`
	ReasonCodes      []string         `json:"reason_codes"`
	ProfileVersion   string           `json:"profile_version"`
	ProfileDigest    string           `json:"profile_digest"`
	ObjectiveVersion string           `json:"objective_version"`
	EvaluatedAt      time.Time        `json:"evaluated_at"`
	Breakdown        []ScoreBreakdown `json:"breakdown"`
}
