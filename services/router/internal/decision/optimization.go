package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/performance"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/scoring"
)

const OptimizationSchemaVersion = "routing_optimization.v1"

var ErrInvalidOptimization = errors.New("invalid routing optimization configuration")

// OptimizationConfig is one immutable Router-owned comparison snapshot. The
// feature manifests must cover the exact capability-catalog identities; they
// cannot introduce a candidate or silently move one to another revision.
type OptimizationConfig struct {
	SchemaVersion       string               `json:"schema_version"`
	Version             string               `json:"version"`
	FeeCatalog          cost.Artifact        `json:"fee_catalog"`
	PerformanceSnapshot performance.Artifact `json:"performance_snapshot"`
	ScoringProfile      scoring.Profile      `json:"scoring_profile"`
	CrossBorderRules    []CrossBorderRule    `json:"cross_border_rules"`
}

// CrossBorderRule gives an exact connection revision a named geographical
// contract. Country inequality is evaluated here, never guessed by cost.
type CrossBorderRule struct {
	CandidateID         string    `json:"candidate_id"`
	ConnectionRevision  string    `json:"connection_revision"`
	RuleVersion         string    `json:"rule_version"`
	CountryField        string    `json:"country_field"`
	CounterpartyCountry string    `json:"counterparty_country"`
	Source              string    `json:"source"`
	SourceVersion       string    `json:"source_version"`
	ObservedAt          time.Time `json:"observed_at"`
	ExpiresAt           time.Time `json:"expires_at"`
}

// CrossBorderEvidence retains the named input and rule provenance. Unknown or
// stale inputs deliberately omit Value, so absence can never become domestic.
type CrossBorderEvidence struct {
	CandidateID        string                   `json:"candidate_id"`
	ConnectionRevision string                   `json:"connection_revision"`
	State              string                   `json:"state"`
	Value              *bool                    `json:"value,omitempty"`
	Rule               *CrossBorderRule         `json:"rule,omitempty"`
	Country            *routingcontext.Observed `json:"country,omitempty"`
	EvaluatedAt        time.Time                `json:"evaluated_at"`
	ReasonCodes        []string                 `json:"reason_codes"`
}

type OptimizationResult struct {
	ConfigVersion   string                `json:"config_version"`
	ConfigDigest    string                `json:"config_digest"`
	Scoring         scoring.Result        `json:"scoring"`
	Classifications []CrossBorderEvidence `json:"classifications"`
}

type candidateIdentity struct {
	id       string
	revision string
}

type feeScheduleKey struct {
	candidate string
	revision  string
	operation string
	currency  string
}

type feeEvidenceKey struct {
	candidate string
	revision  string
	id        string
	version   string
}

type compiledOptimization struct {
	version                 string
	digest                  string
	profileQuoteCurrency    string
	performanceCountryField string
	fees                    *cost.Evaluator
	performance             *performance.Snapshot
	scorer                  *scoring.Scorer
	rules                   map[candidateIdentity]CrossBorderRule
	schedules               map[feeScheduleKey][]cost.FeeSchedule
	scheduleExpires         map[feeEvidenceKey]time.Time
	profileExpiresAt        time.Time
	performanceMaxAge       time.Duration
	performanceMaxWindowAge time.Duration
}

// optimizationValidity is process-local authority used only by ExecutableAt.
// A JSON round trip intentionally loses it and therefore fails closed.
type optimizationValidity struct {
	configDigest string
	validUntil   time.Time
}

func compileOptimization(input OptimizationConfig, catalogCandidates []routing.CandidateRef) (*compiledOptimization, error) {
	bad := func(message string) (*compiledOptimization, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidOptimization, message)
	}
	if input.SchemaVersion != OptimizationSchemaVersion || !routingcontext.ValidIdentifier(input.Version) {
		return bad("unsupported schema or invalid version")
	}
	fees, err := cost.Compile(input.FeeCatalog)
	if err != nil {
		return nil, fmt.Errorf("%w: compile fee catalog: %v", ErrInvalidOptimization, err)
	}
	performanceSnapshot, err := performance.Compile(input.PerformanceSnapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: compile performance snapshot: %v", ErrInvalidOptimization, err)
	}
	scorer, err := scoring.Compile(input.ScoringProfile)
	if err != nil {
		return nil, fmt.Errorf("%w: compile scoring profile: %v", ErrInvalidOptimization, err)
	}
	if input.PerformanceSnapshot.Cohort.Operation != "sale" || input.PerformanceSnapshot.Cohort.CaptureMethod != "automatic" {
		return bad("the current optimization slice requires the sale/automatic cohort")
	}
	if !sameCostManifest(catalogCandidates, fees.Candidates()) || !samePerformanceManifest(catalogCandidates, performanceSnapshot.Candidates()) {
		return bad("feature candidate manifests must equal the capability catalog")
	}
	feeCurrencies := make(map[string]bool, len(input.FeeCatalog.Currencies))
	for _, currency := range input.FeeCatalog.Currencies {
		feeCurrencies[currency.Code] = true
	}
	if !feeCurrencies[input.ScoringProfile.QuoteCurrency] {
		return bad("scoring quote currency lacks fee-catalog metadata")
	}
	for _, bucket := range input.PerformanceSnapshot.Buckets {
		if bucket.Segment.Currency != performance.Wildcard && !feeCurrencies[bucket.Segment.Currency] {
			return bad("performance currency lacks fee-catalog metadata")
		}
	}

	optimization := &compiledOptimization{
		version: input.Version, profileQuoteCurrency: input.ScoringProfile.QuoteCurrency, performanceCountryField: input.PerformanceSnapshot.CountryField,
		fees: fees, performance: performanceSnapshot, scorer: scorer,
		rules: make(map[candidateIdentity]CrossBorderRule), schedules: make(map[feeScheduleKey][]cost.FeeSchedule),
		scheduleExpires: make(map[feeEvidenceKey]time.Time), profileExpiresAt: input.ScoringProfile.ExpiresAt.UTC(),
		performanceMaxAge: input.PerformanceSnapshot.MaxAge, performanceMaxWindowAge: input.PerformanceSnapshot.MaxWindowAge,
	}
	manifest := make(map[candidateIdentity]bool, len(catalogCandidates))
	for _, candidate := range catalogCandidates {
		manifest[candidateIdentity{candidate.ID, candidate.ConnectionRevision}] = true
	}
	rules := append([]CrossBorderRule(nil), input.CrossBorderRules...)
	if len(rules) > len(catalogCandidates) {
		return bad("cross-border rule manifest is unbounded or duplicated")
	}
	for i := range rules {
		rule := &rules[i]
		key := candidateIdentity{rule.CandidateID, rule.ConnectionRevision}
		if !manifest[key] || optimization.rules[key].CandidateID != "" ||
			!routingcontext.ValidIdentifier(rule.RuleVersion) ||
			(rule.CountryField != performance.CountryMerchant && rule.CountryField != performance.CountryIssuer) ||
			!routingcontext.ValidFieldString(rule.CountryField, rule.CounterpartyCountry) ||
			!routingcontext.ValidIdentifier(rule.Source) || !routingcontext.ValidIdentifier(rule.SourceVersion) ||
			!utcInstant(rule.ObservedAt) || !utcInstant(rule.ExpiresAt) || !rule.ObservedAt.Before(rule.ExpiresAt) {
			return bad("cross-border rule has invalid identity, definition or provenance")
		}
		rule.ObservedAt, rule.ExpiresAt = rule.ObservedAt.UTC(), rule.ExpiresAt.UTC()
		optimization.rules[key] = *rule
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].CandidateID != rules[j].CandidateID {
			return rules[i].CandidateID < rules[j].CandidateID
		}
		return rules[i].ConnectionRevision < rules[j].ConnectionRevision
	})

	for _, schedule := range input.FeeCatalog.Schedules {
		schedule.EffectiveFrom, schedule.EffectiveUntil = schedule.EffectiveFrom.UTC(), schedule.EffectiveUntil.UTC()
		key := feeScheduleKey{schedule.CandidateID, schedule.ConnectionRevision, schedule.Operation, schedule.PaymentCurrency}
		optimization.schedules[key] = append(optimization.schedules[key], schedule)
		optimization.scheduleExpires[feeEvidenceKey{schedule.CandidateID, schedule.ConnectionRevision, schedule.ID, schedule.Version}] = schedule.EffectiveUntil
		if scheduleNeedsClassification(schedule) {
			if rule, exists := optimization.rules[candidateIdentity{schedule.CandidateID, schedule.ConnectionRevision}]; exists && rule.RuleVersion != schedule.CrossBorderRuleVersion {
				return bad("cross-border rule version does not match its fee schedule")
			}
		}
	}
	for key := range optimization.schedules {
		sort.Slice(optimization.schedules[key], func(i, j int) bool {
			return optimization.schedules[key][i].EffectiveFrom.Before(optimization.schedules[key][j].EffectiveFrom)
		})
	}
	canonical := struct {
		SchemaVersion      string            `json:"schema_version"`
		Version            string            `json:"version"`
		FeeVersion         string            `json:"fee_version"`
		FeeDigest          string            `json:"fee_digest"`
		PerformanceVersion string            `json:"performance_version"`
		PerformanceDigest  string            `json:"performance_digest"`
		ScoringVersion     string            `json:"scoring_version"`
		ScoringDigest      string            `json:"scoring_digest"`
		CrossBorderRules   []CrossBorderRule `json:"cross_border_rules"`
	}{OptimizationSchemaVersion, input.Version, fees.Version(), fees.Digest(), performanceSnapshot.Version(), performanceSnapshot.Digest(), scorer.Version(), scorer.Digest(), rules}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return bad("configuration cannot be canonically encoded")
	}
	digest := sha256.Sum256(payload)
	optimization.digest = "sha256:" + hex.EncodeToString(digest[:])
	return optimization, nil
}

func sameCostManifest(catalog []routing.CandidateRef, features []cost.Candidate) bool {
	if len(catalog) != len(features) {
		return false
	}
	want := make(map[string]string, len(catalog))
	for _, candidate := range catalog {
		want[candidate.ID] = candidate.ConnectionRevision
	}
	for _, candidate := range features {
		if want[candidate.ID] != candidate.ConnectionRevision {
			return false
		}
		delete(want, candidate.ID)
	}
	return len(want) == 0
}

func samePerformanceManifest(catalog []routing.CandidateRef, features []performance.Candidate) bool {
	if len(catalog) != len(features) {
		return false
	}
	want := make(map[string]string, len(catalog))
	for _, candidate := range catalog {
		want[candidate.ID] = candidate.ConnectionRevision
	}
	for _, candidate := range features {
		if want[candidate.ID] != candidate.ConnectionRevision {
			return false
		}
		delete(want, candidate.ID)
	}
	return len(want) == 0
}

func (o *compiledOptimization) readyAt(at time.Time) bool {
	return o != nil && o.digest != "" && o.scorer.ValidAt(at)
}

func (o *compiledOptimization) evaluate(context routingcontext.Context, candidates map[string]routing.CandidateRef, allowed []string) (OptimizationResult, *optimizationValidity, error) {
	at := context.BuiltAt
	batch := scoring.Batch{Allowed: []scoring.Candidate{}, Costs: []cost.Result{}, Performance: []performance.Result{}}
	classifications := make([]CrossBorderEvidence, 0, len(allowed))
	for rank, candidateID := range allowed {
		candidate, exists := candidates[candidateID]
		if !exists {
			return OptimizationResult{}, nil, fmt.Errorf("%w: rule output contains an unknown candidate", ErrInvalidOptimization)
		}
		batch.Allowed = append(batch.Allowed, scoring.Candidate{ID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, RuleRank: rank})
		schedule, hasSchedule := o.activeSchedule(candidate, context, at)
		requiresClassification := hasSchedule && scheduleNeedsClassification(schedule)
		classification := o.classify(context, candidate, requiresClassification)
		classifications = append(classifications, classification)
		price, err := o.fees.Estimate(at, cost.Query{
			CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, Operation: context.Base.Operation,
			AmountMinor: context.Base.AmountMinor, PaymentCurrency: context.Base.Currency,
			QuoteCurrency: o.quoteCurrency(), CrossBorder: crossBorderFact(classification),
		})
		if err != nil {
			return OptimizationResult{}, nil, fmt.Errorf("evaluate cost for %s: %w", candidate.ID, err)
		}
		observed, err := o.performance.Lookup(at, performance.Query{
			CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision,
			CountryField: o.countryField(), CountryState: observationState(o.countryObservation(context)), CountryValue: observationValue(o.countryObservation(context)),
			NetworkState: observationState(context.PaymentMethod.Network), NetworkValue: observationValue(context.PaymentMethod.Network),
			Currency: context.Base.Currency, Operation: context.Base.Operation, CaptureMethod: context.Base.CaptureMethod,
		})
		if err != nil {
			return OptimizationResult{}, nil, fmt.Errorf("evaluate performance for %s: %w", candidate.ID, err)
		}
		batch.Costs, batch.Performance = append(batch.Costs, price), append(batch.Performance, observed)
	}
	score, err := o.scorer.Score(at, batch)
	if err != nil {
		return OptimizationResult{}, nil, fmt.Errorf("score allowed candidates: %w", err)
	}
	result := OptimizationResult{ConfigVersion: o.version, ConfigDigest: o.digest, Scoring: score, Classifications: classifications}
	validity, err := o.validity(context, result)
	if err != nil {
		return OptimizationResult{}, nil, err
	}
	return result, validity, nil
}

func (o *compiledOptimization) quoteCurrency() string {
	return o.profileQuoteCurrency
}

func (o *compiledOptimization) countryField() string { return o.performanceCountryField }

func (o *compiledOptimization) activeSchedule(candidate routing.CandidateRef, context routingcontext.Context, at time.Time) (cost.FeeSchedule, bool) {
	for _, schedule := range o.schedules[feeScheduleKey{candidate.ID, candidate.ConnectionRevision, context.Base.Operation, context.Base.Currency}] {
		if !at.Before(schedule.EffectiveFrom) && at.Before(schedule.EffectiveUntil) {
			return schedule, true
		}
	}
	return cost.FeeSchedule{}, false
}

func scheduleNeedsClassification(schedule cost.FeeSchedule) bool {
	return schedule.Attempt.CrossBorderBPS != 0 || schedule.Success.CrossBorderBPS != 0 || schedule.Failure.CrossBorderBPS != 0
}

func (o *compiledOptimization) classify(context routingcontext.Context, candidate routing.CandidateRef, required bool) CrossBorderEvidence {
	result := CrossBorderEvidence{CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, State: cost.FactNotApplicable, EvaluatedAt: context.BuiltAt, ReasonCodes: []string{"cross_border_not_required"}}
	if !required {
		return result
	}
	result.State, result.ReasonCodes = cost.FactUnknown, []string{"cross_border_rule_missing"}
	rule, exists := o.rules[candidateIdentity{candidate.ID, candidate.ConnectionRevision}]
	if !exists {
		return result
	}
	ruleCopy := rule
	result.Rule = &ruleCopy
	country := countryObservationForField(context, rule.CountryField)
	countryCopy := cloneObserved(country)
	result.Country = &countryCopy
	if context.BuiltAt.Before(rule.ObservedAt) {
		result.ReasonCodes = []string{"cross_border_rule_not_yet_observed"}
		return result
	}
	if !context.BuiltAt.Before(rule.ExpiresAt) {
		result.State, result.ReasonCodes = cost.FactStale, []string{"cross_border_rule_stale"}
		return result
	}
	if country.State == routingcontext.StateStale || (country.ExpiresAt != nil && !context.BuiltAt.Before(*country.ExpiresAt)) {
		result.State, result.ReasonCodes = cost.FactStale, []string{"cross_border_country_stale"}
		return result
	}
	if country.State != routingcontext.StateKnown || country.Value == nil {
		result.ReasonCodes = []string{"cross_border_country_" + country.State}
		return result
	}
	value := *country.Value != rule.CounterpartyCountry
	result.State, result.Value, result.ReasonCodes = cost.FactKnown, &value, []string{"cross_border_classified"}
	return result
}

func crossBorderFact(value CrossBorderEvidence) cost.CrossBorderFact {
	result := cost.CrossBorderFact{State: value.State}
	if value.Rule != nil {
		result.RuleVersion = value.Rule.RuleVersion
	}
	if value.State == cost.FactKnown && value.Value != nil {
		result.Value = *value.Value
	}
	return result
}

func (o *compiledOptimization) countryObservation(context routingcontext.Context) routingcontext.Observed {
	return countryObservationForField(context, o.countryField())
}

func countryObservationForField(context routingcontext.Context, field string) routingcontext.Observed {
	if field == performance.CountryIssuer {
		return context.PaymentMethod.IssuerCountry
	}
	return context.Merchant.Country
}

func observationState(value routingcontext.Observed) string { return value.State }
func observationValue(value routingcontext.Observed) string {
	if value.Value == nil {
		return ""
	}
	return *value.Value
}

func cloneObserved(value routingcontext.Observed) routingcontext.Observed {
	if value.Value != nil {
		copy := *value.Value
		value.Value = &copy
	}
	if value.ExpiresAt != nil {
		copy := value.ExpiresAt.UTC()
		value.ExpiresAt = &copy
	}
	value.ObservedAt = value.ObservedAt.UTC()
	return value
}

func (o *compiledOptimization) validity(context routingcontext.Context, result OptimizationResult) (*optimizationValidity, error) {
	validUntil := o.profileExpiresAt
	validUntil = earlier(validUntil, context.Risk.ExpiresAt)
	if result.Scoring.Status != scoring.StatusScored {
		return &optimizationValidity{configDigest: o.digest, validUntil: validUntil}, nil
	}
	for _, row := range result.Scoring.Breakdown {
		if row.Cost.Estimate == nil || row.Performance.Estimate == nil {
			return nil, fmt.Errorf("%w: scored evidence omitted a feature", ErrInvalidOptimization)
		}
		expires, exists := o.scheduleExpires[feeEvidenceKey{row.CandidateID, row.ConnectionRevision, row.Cost.Estimate.ScheduleID, row.Cost.Estimate.ScheduleVersion}]
		if !exists {
			return nil, fmt.Errorf("%w: scored fee evidence is not in the compiled catalog", ErrInvalidOptimization)
		}
		validUntil = earlier(validUntil, expires)
		for _, item := range row.Cost.Estimate.Breakdown {
			if item.FXExpiresAt != nil {
				validUntil = earlier(validUntil, *item.FXExpiresAt)
			}
		}
		validUntil = earlier(validUntil, row.Performance.ProducedAt.Add(o.performanceMaxAge))
		validUntil = earlier(validUntil, row.Performance.WindowEnd.Add(o.performanceMaxWindowAge))
		if row.Cost.Estimate.CrossBorderRuleVersion != "" {
			classification, exists := classificationFor(result.Classifications, row.CandidateID, row.ConnectionRevision)
			if !exists || classification.State != cost.FactKnown || classification.Rule == nil || classification.Country == nil || classification.Country.ExpiresAt == nil {
				return nil, fmt.Errorf("%w: scored cross-border fee lacks fresh classification", ErrInvalidOptimization)
			}
			validUntil = earlier(validUntil, classification.Rule.ExpiresAt)
			validUntil = earlier(validUntil, *classification.Country.ExpiresAt)
		}
	}
	return &optimizationValidity{configDigest: o.digest, validUntil: validUntil}, nil
}

func classificationFor(values []CrossBorderEvidence, candidate, revision string) (CrossBorderEvidence, bool) {
	for _, value := range values {
		if value.CandidateID == candidate && value.ConnectionRevision == revision {
			return value, true
		}
	}
	return CrossBorderEvidence{}, false
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func utcInstant(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}
