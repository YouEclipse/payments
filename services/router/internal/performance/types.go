// Package performance reads immutable historical-policy evidence. It does not
// collect events, infer provider outcomes from HTTP status, estimate causal
// uplift, select a candidate, or authorize any funds operation.
package performance

import (
	"errors"
	"time"
)

const (
	SchemaVersion         = "performance.v1"
	SegmentSchemaVersion  = "candidate_country_network_currency.v1"
	EstimatorVersion      = "parent_shrinkage_wilson95.v1"
	IntervalMethodVersion = "observed_binomial_wilson95.v1"
	DenominatorVersion    = "finalized_logical_operation.v1"
	SaleOutcome           = "sale_first_attempt_completion.v1"
	AuthorizationOutcome  = "authorization_approval.v1"
	ProbabilityScale      = int64(1_000_000)
	ObservationalOnly     = "historical_policy_conditional_not_causal"
)

const (
	CountryMerchant = "merchant.country"
	CountryIssuer   = "payment_method_context.issuer_country"
	Wildcard        = "*"
)

const (
	StateKnown         = "known"
	StatePrior         = "prior"
	StateUnknown       = "unknown"
	StateStale         = "stale"
	StateInvalid       = "invalid"
	StateRedacted      = "redacted"
	StateNotApplicable = "not_applicable"
)

var (
	ErrInvalidArtifact = errors.New("invalid performance artifact")
	ErrInvalidQuery    = errors.New("invalid performance query")
	ErrInvalidEvidence = errors.New("invalid performance evidence")
)

type Candidate struct {
	ID                 string `json:"id"`
	ConnectionRevision string `json:"connection_revision"`
}

// Cohort is not just a label: only these two first-attempt card definitions
// are supported. SALE completion counts finalized technical failures as
// failures. Authorization approval counts only definitive issuer/network
// approvals and declines; technical errors are outside that denominator.
// Neither definition counts local exclusions, repeated transport deliveries,
// follow-on Capture/Void/Refund, or UNKNOWN as a finalized failure.
type Cohort struct {
	PaymentMethod      string `json:"payment_method"`
	Operation          string `json:"operation"`
	CaptureMethod      string `json:"capture_method"`
	AttemptKind        string `json:"attempt_kind"`
	OutcomeDefinition  string `json:"outcome_definition"`
	DenominatorVersion string `json:"denominator_version"`
}

// CountryValue always has Artifact.CountryField semantics. Wildcards are
// allowed ONLY along the fixed L0 -> L1 -> L2 -> L3 hierarchy. Revision is never
// a wildcard, even for the candidate-only bucket.
type SegmentKey struct {
	CandidateID        string `json:"candidate_id"`
	ConnectionRevision string `json:"connection_revision"`
	CountryValue       string `json:"country_value"`
	NetworkValue       string `json:"network_value"`
	Currency           string `json:"currency"`
}

// SampleSize means finalized, deduplicated logical operations in the declared
// cohort. PendingCount reports unresolved observations, not extra failures.
// The producer owns event normalization and deduplication; Compile verifies
// aggregate consistency but cannot prove that a supplied count is truthful.
type Bucket struct {
	Segment      SegmentKey `json:"segment"`
	Successes    uint64     `json:"successes"`
	SampleSize   uint64     `json:"sample_size"`
	PendingCount uint64     `json:"pending_count"`
}

type Prior struct {
	Probability float64 `json:"probability"`
	Strength    float64 `json:"strength"`
}

type Artifact struct {
	SchemaVersion        string        `json:"schema_version"`
	Version              string        `json:"version"`
	SegmentSchemaVersion string        `json:"segment_schema_version"`
	EstimatorVersion     string        `json:"estimator_version"`
	CountryField         string        `json:"country_field"`
	Cohort               Cohort        `json:"cohort"`
	WindowStart          time.Time     `json:"window_start"`
	WindowEnd            time.Time     `json:"window_end"`
	ObservedAt           time.Time     `json:"observed_at"`
	ProducedAt           time.Time     `json:"produced_at"`
	MaxAge               time.Duration `json:"max_age"`
	MaxWindowAge         time.Duration `json:"max_window_age"`
	MinSamples           []uint64      `json:"min_samples"`
	MinCoveragePPM       uint32        `json:"min_coverage_ppm"`
	Prior                Prior         `json:"prior"`
	Candidates           []Candidate   `json:"candidates"`
	Buckets              []Bucket      `json:"buckets"`
}

// Query is a credential-free slice of a validated first-attempt Context.
// unknown/redacted/not_applicable carry no value. A stale value can be retained
// for audit, but is NEVER used as a known key. CountryField must match the
// snapshot; issuer geography cannot silently become merchant geography.
type Query struct {
	CandidateID        string `json:"candidate_id"`
	ConnectionRevision string `json:"connection_revision"`
	CountryField       string `json:"country_field"`
	CountryState       string `json:"country_state"`
	CountryValue       string `json:"country_value,omitempty"`
	NetworkState       string `json:"network_state"`
	NetworkValue       string `json:"network_value,omitempty"`
	Currency           string `json:"currency"`
	Operation          string `json:"operation"`
	CaptureMethod      string `json:"capture_method"`
}

// ObservedLower95/Upper95 are the raw binomial sample's Wilson interval, NOT a
// posterior interval for the shrunk point estimate and NOT a causal interval.
// ConservativeProbability is min(Probability, ObservedLower95). It is a
// declared conservative scoring feature, not a guaranteed approval rate.
type Estimate struct {
	Probability             float64 `json:"probability"`
	ProbabilityPPM          int64   `json:"probability_ppm"`
	ConservativeProbability float64 `json:"conservative_probability"`
	ConservativePPM         int64   `json:"conservative_ppm"`
	ObservedLower95         float64 `json:"observed_lower_95"`
	ObservedUpper95         float64 `json:"observed_upper_95"`
	Successes               uint64  `json:"successes"`
	SampleSize              uint64  `json:"sample_size"`
	PendingCount            uint64  `json:"pending_count"`
	CoveragePPM             uint32  `json:"coverage_ppm"`
	Confidence              string  `json:"confidence"`
	ParentProbability       float64 `json:"parent_probability"`
	PriorStrength           float64 `json:"prior_strength"`
	EstimatorVersion        string  `json:"estimator_version"`
	IntervalMethodVersion   string  `json:"interval_method_version"`
}

type Result struct {
	CandidateID        string      `json:"candidate_id"`
	ConnectionRevision string      `json:"connection_revision"`
	State              string      `json:"state"`
	ReasonCodes        []string    `json:"reason_codes"`
	Interpretation     string      `json:"interpretation"`
	SnapshotVersion    string      `json:"snapshot_version"`
	SnapshotDigest     string      `json:"snapshot_digest"`
	CountryField       string      `json:"country_field"`
	Cohort             Cohort      `json:"cohort"`
	Source             string      `json:"source"`
	SourceSegment      *SegmentKey `json:"source_segment,omitempty"`
	ParentSegment      *SegmentKey `json:"parent_segment,omitempty"`
	FallbackLevel      int         `json:"fallback_level"`
	WindowStart        time.Time   `json:"window_start"`
	WindowEnd          time.Time   `json:"window_end"`
	ObservedAt         time.Time   `json:"observed_at"`
	ProducedAt         time.Time   `json:"produced_at"`
	EvaluatedAt        time.Time   `json:"evaluated_at"`
	Estimate           *Estimate   `json:"estimate,omitempty"`
}
