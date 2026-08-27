// Package cost produces immutable, replayable price scenarios for candidates
// that have already passed eligibility. It performs no routing, payment state
// transition, provider call, currency discovery, or wall-clock read.
package cost

import (
	"errors"
	"time"
)

const SchemaVersion = "fee_catalog.v1"

const (
	StateKnown   = "known"
	StateUnknown = "unknown"
	StateStale   = "stale"
	StateInvalid = "invalid"
)

const (
	FactKnown         = "known"
	FactUnknown       = "unknown"
	FactStale         = "stale"
	FactRedacted      = "redacted"
	FactNotApplicable = "not_applicable"
)

var (
	ErrInvalidArtifact = errors.New("invalid cost artifact")
	ErrInvalidQuery    = errors.New("invalid cost query")
	ErrCostOverflow    = errors.New("cost arithmetic overflow")
)

type RoundingMode string

const (
	RoundDown   RoundingMode = "down"
	RoundHalfUp RoundingMode = "half_up"
	RoundUp     RoundingMode = "up"
)

type Candidate struct {
	ID                 string `json:"id"`
	ConnectionRevision string `json:"connection_revision"`
}

// Exponent is the number of minor-unit decimal places in one major unit. The
// catalog is supplied by the control plane; this package does not assume USD
// precision for another currency or claim to maintain ISO 4217 reference data.
type Currency struct {
	Code     string `json:"code"`
	Exponent uint8  `json:"exponent"`
}

// Fee is a conditional component, not a prediction of whether it will occur.
// FixedMicros uses FeeSchedule.FixedCurrency. The BPS components use the payment
// amount and PaymentCurrency. All micros are one millionth of a MAJOR unit.
type Fee struct {
	FixedMicros    int64 `json:"fixed_micros"`
	VariableBPS    int64 `json:"variable_bps"`
	CrossBorderBPS int64 `json:"cross_border_bps"`
}

type FeeSchedule struct {
	ID                     string       `json:"id"`
	Version                string       `json:"version"`
	CandidateID            string       `json:"candidate_id"`
	ConnectionRevision     string       `json:"connection_revision"`
	Operation              string       `json:"operation"`
	PaymentCurrency        string       `json:"payment_currency"`
	FixedCurrency          string       `json:"fixed_currency"`
	EffectiveFrom          time.Time    `json:"effective_from"`
	EffectiveUntil         time.Time    `json:"effective_until"`
	PercentageRound        RoundingMode `json:"percentage_round"`
	CrossBorderRuleVersion string       `json:"cross_border_rule_version,omitempty"`
	Attempt                Fee          `json:"attempt"`
	Success                Fee          `json:"success"`
	Failure                Fee          `json:"failure"`
}

// FXQuote means one BaseCurrency MAJOR unit buys Numerator/Denominator
// QuoteCurrency MAJOR units. Conversion never guesses an inverse or a 1:1 rate.
// Percentage fees are first rounded in their source currency's micros; FX then
// applies its own declared rounding to quote-currency micros, per component.
type FXQuote struct {
	Version       string       `json:"version"`
	Source        string       `json:"source"`
	BaseCurrency  string       `json:"base_currency"`
	QuoteCurrency string       `json:"quote_currency"`
	Numerator     int64        `json:"numerator"`
	Denominator   int64        `json:"denominator"`
	AsOf          time.Time    `json:"as_of"`
	ExpiresAt     time.Time    `json:"expires_at"`
	Rounding      RoundingMode `json:"rounding"`
}

type Artifact struct {
	SchemaVersion           string        `json:"schema_version"`
	Version                 string        `json:"version"`
	CurrencyMetadataVersion string        `json:"currency_metadata_version"`
	PublishedAt             time.Time     `json:"published_at"`
	Candidates              []Candidate   `json:"candidates"`
	Currencies              []Currency    `json:"currencies"`
	Schedules               []FeeSchedule `json:"schedules"`
	FXQuotes                []FXQuote     `json:"fx_quotes"`
}

// CrossBorderFact must come from an approved, versioned classification of
// named geographical facts. Country inequality and payment credentials are
// deliberately not inputs to this package.
type CrossBorderFact struct {
	State       string `json:"state"`
	Value       bool   `json:"value"`
	RuleVersion string `json:"rule_version"`
}

type Query struct {
	CandidateID        string          `json:"candidate_id"`
	ConnectionRevision string          `json:"connection_revision"`
	Operation          string          `json:"operation"`
	AmountMinor        int64           `json:"amount_minor"`
	PaymentCurrency    string          `json:"payment_currency"`
	QuoteCurrency      string          `json:"quote_currency"`
	CrossBorder        CrossBorderFact `json:"cross_border"`
}

type BreakdownItem struct {
	Scenario               string       `json:"scenario"`
	Kind                   string       `json:"kind"`
	Basis                  string       `json:"basis"`
	SourceCurrency         string       `json:"source_currency"`
	SourceMicros           int64        `json:"source_micros"`
	QuoteCurrency          string       `json:"quote_currency"`
	AmountMicros           int64        `json:"amount_micros"`
	PercentageRound        RoundingMode `json:"percentage_round,omitempty"`
	CrossBorderRuleVersion string       `json:"cross_border_rule_version,omitempty"`
	FXVersion              string       `json:"fx_version,omitempty"`
	FXSource               string       `json:"fx_source,omitempty"`
	FXNumerator            int64        `json:"fx_numerator,omitempty"`
	FXDenominator          int64        `json:"fx_denominator,omitempty"`
	FXRounding             RoundingMode `json:"fx_rounding,omitempty"`
	FXAsOf                 *time.Time   `json:"fx_as_of,omitempty"`
	FXExpiresAt            *time.Time   `json:"fx_expires_at,omitempty"`
}

// SucceededCostMicros and FailedCostMicros each INCLUDE AttemptCostMicros.
// Success-only and failure-only fees are never added to each other. There is
// no implicit success probability, retry count, fallback fee, or Actual Cost.
type Estimate struct {
	QuoteCurrency           string          `json:"quote_currency"`
	AttemptCostMicros       int64           `json:"attempt_cost_micros"`
	SucceededCostMicros     int64           `json:"succeeded_cost_micros"`
	FailedCostMicros        int64           `json:"failed_cost_micros"`
	Breakdown               []BreakdownItem `json:"breakdown"`
	ScheduleID              string          `json:"schedule_id"`
	ScheduleVersion         string          `json:"schedule_version"`
	CurrencyMetadataVersion string          `json:"currency_metadata_version"`
	CrossBorderRuleVersion  string          `json:"cross_border_rule_version,omitempty"`
	EvaluatedAt             time.Time       `json:"evaluated_at"`
}

// Estimate is nil unless State is known, including missing/stale/overflow
// paths. Callers must use the state, not an integer zero value, for comparison.
type Result struct {
	CandidateID        string    `json:"candidate_id"`
	ConnectionRevision string    `json:"connection_revision"`
	State              string    `json:"state"`
	ReasonCodes        []string  `json:"reason_codes"`
	ArtifactVersion    string    `json:"artifact_version"`
	ArtifactDigest     string    `json:"artifact_digest"`
	Estimate           *Estimate `json:"estimate,omitempty"`
}
