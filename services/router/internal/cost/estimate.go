package cost

import "time"

// Estimate evaluates one already eligible identity at an explicitly frozen
// instant. Unavailable evidence never removes a candidate or selects a winner;
// a separately versioned policy owns abstention and whole-batch degradation.
func (e *Evaluator) Estimate(at time.Time, q Query) (Result, error) {
	result := Result{
		CandidateID: q.CandidateID, ConnectionRevision: q.ConnectionRevision,
		State: StateInvalid, ReasonCodes: []string{}, ArtifactVersion: e.Version(), ArtifactDigest: e.Digest(),
	}
	fail := func(state, reason string, err error) (Result, error) {
		result.State, result.ReasonCodes = state, []string{reason}
		return result, err
	}
	if e == nil || e.digest == "" {
		return fail(StateInvalid, "cost_invalid_artifact", ErrInvalidArtifact)
	}
	if !validQuery(q) || !utcInstant(at) {
		return fail(StateInvalid, "cost_invalid_query", ErrInvalidQuery)
	}
	if at.Before(e.artifact.PublishedAt) {
		return fail(StateUnknown, "cost_artifact_not_yet_published", nil)
	}
	revision, exists := e.candidates[q.CandidateID]
	if !exists {
		return fail(StateUnknown, "cost_unknown_candidate", nil)
	}
	if revision != q.ConnectionRevision {
		return fail(StateUnknown, "cost_candidate_revision_mismatch", nil)
	}
	exponent, exists := e.currencies[q.PaymentCurrency]
	if !exists {
		return fail(StateUnknown, "cost_missing_currency_metadata", nil)
	}
	if _, exists := e.currencies[q.QuoteCurrency]; !exists {
		return fail(StateUnknown, "cost_missing_currency_metadata", nil)
	}
	schedules := e.schedules[scheduleKey{q.CandidateID, q.ConnectionRevision, q.Operation, q.PaymentCurrency}]
	var selected *FeeSchedule
	expired := false
	for _, schedule := range schedules {
		if !at.Before(schedule.EffectiveFrom) && at.Before(schedule.EffectiveUntil) {
			selected = &schedule
			break
		}
		expired = expired || !at.Before(schedule.EffectiveUntil)
	}
	if selected == nil {
		if expired {
			return fail(StateStale, "cost_stale_schedule", nil)
		}
		return fail(StateUnknown, "cost_missing_schedule", nil)
	}
	schedule := *selected
	needsClassification := schedule.Attempt.CrossBorderBPS != 0 || schedule.Success.CrossBorderBPS != 0 || schedule.Failure.CrossBorderBPS != 0
	if needsClassification && q.CrossBorder.State != FactKnown {
		if q.CrossBorder.State == FactStale {
			return fail(StateStale, "cost_stale_cross_border", nil)
		}
		return fail(StateUnknown, "cost_unknown_cross_border", nil)
	}
	if needsClassification && q.CrossBorder.RuleVersion != schedule.CrossBorderRuleVersion {
		return fail(StateUnknown, "cost_cross_border_rule_mismatch", nil)
	}
	var totals [3]int64
	breakdown := make([]BreakdownItem, 0, 9)
	for i, scenario := range []struct {
		name string
		fee  Fee
	}{{"attempt", schedule.Attempt}, {"success_only", schedule.Success}, {"failure_only", schedule.Failure}} {
		total, items, failure := e.evaluateFee(at, q, exponent, schedule, scenario.name, scenario.fee)
		if failure != nil {
			return fail(failure.state, failure.reason, failure.err)
		}
		totals[i], breakdown = total, append(breakdown, items...)
	}
	succeeded, err := checkedAdd(totals[0], totals[1])
	if err != nil {
		return fail(StateInvalid, "cost_arithmetic_overflow", err)
	}
	failed, err := checkedAdd(totals[0], totals[2])
	if err != nil {
		return fail(StateInvalid, "cost_arithmetic_overflow", err)
	}
	result.State = StateKnown
	result.ReasonCodes = []string{"cost_contract_scenarios", "cost_expected_not_actual"}
	result.Estimate = &Estimate{
		QuoteCurrency: q.QuoteCurrency, AttemptCostMicros: totals[0],
		SucceededCostMicros: succeeded, FailedCostMicros: failed, Breakdown: breakdown,
		ScheduleID: schedule.ID, ScheduleVersion: schedule.Version,
		CurrencyMetadataVersion: e.artifact.CurrencyMetadataVersion,
		CrossBorderRuleVersion:  q.CrossBorder.RuleVersion, EvaluatedAt: at.UTC(),
	}
	return result, nil
}

type evaluationFailure struct {
	state, reason string
	err           error
}

func (e *Evaluator) evaluateFee(at time.Time, q Query, exponent uint8, schedule FeeSchedule, scenario string, fee Fee) (int64, []BreakdownItem, *evaluationFailure) {
	variable, err := percentageMicros(q.AmountMinor, exponent, fee.VariableBPS, schedule.PercentageRound)
	if err != nil {
		return 0, nil, &evaluationFailure{StateInvalid, "cost_arithmetic_overflow", err}
	}
	items := []BreakdownItem{
		{Scenario: scenario, Kind: "fixed", Basis: "contract_fixed", SourceCurrency: schedule.FixedCurrency, SourceMicros: fee.FixedMicros},
		{Scenario: scenario, Kind: "variable", Basis: "payment_amount_bps", SourceCurrency: q.PaymentCurrency, SourceMicros: variable, PercentageRound: schedule.PercentageRound},
	}
	if fee.CrossBorderBPS != 0 {
		surcharge := int64(0)
		if q.CrossBorder.Value {
			surcharge, err = percentageMicros(q.AmountMinor, exponent, fee.CrossBorderBPS, schedule.PercentageRound)
			if err != nil {
				return 0, nil, &evaluationFailure{StateInvalid, "cost_arithmetic_overflow", err}
			}
		}
		items = append(items, BreakdownItem{
			Scenario: scenario, Kind: "cross_border", Basis: "classified_payment_amount_bps",
			SourceCurrency: q.PaymentCurrency, SourceMicros: surcharge, PercentageRound: schedule.PercentageRound,
			CrossBorderRuleVersion: q.CrossBorder.RuleVersion,
		})
	}
	total := int64(0)
	for i := range items {
		item := &items[i]
		item.QuoteCurrency, item.AmountMicros = q.QuoteCurrency, item.SourceMicros
		if item.SourceCurrency != q.QuoteCurrency {
			quote, state, reason := e.resolveFX(at, item.SourceCurrency, q.QuoteCurrency)
			if reason != "" {
				return 0, nil, &evaluationFailure{state, reason, nil}
			}
			item.AmountMicros, err = mulDiv(item.SourceMicros, quote.Numerator, quote.Denominator, quote.Rounding)
			if err != nil {
				return 0, nil, &evaluationFailure{StateInvalid, "cost_arithmetic_overflow", err}
			}
			item.FXVersion, item.FXSource = quote.Version, quote.Source
			item.FXNumerator, item.FXDenominator, item.FXRounding = quote.Numerator, quote.Denominator, quote.Rounding
			asOf, expiresAt := quote.AsOf, quote.ExpiresAt
			item.FXAsOf, item.FXExpiresAt = &asOf, &expiresAt
		}
		total, err = checkedAdd(total, item.AmountMicros)
		if err != nil {
			return 0, nil, &evaluationFailure{StateInvalid, "cost_arithmetic_overflow", err}
		}
	}
	return total, items, nil
}

func (e *Evaluator) resolveFX(at time.Time, base, quote string) (FXQuote, string, string) {
	expired := false
	for _, value := range e.fx[fxKey{base, quote}] {
		if !at.Before(value.AsOf) && at.Before(value.ExpiresAt) {
			return value, StateKnown, ""
		}
		expired = expired || !at.Before(value.ExpiresAt)
	}
	if expired {
		return FXQuote{}, StateStale, "cost_stale_fx"
	}
	return FXQuote{}, StateUnknown, "cost_missing_fx"
}

func validQuery(q Query) bool {
	if !candidateID.MatchString(q.CandidateID) || !candidateID.MatchString(q.ConnectionRevision) ||
		!validOperation(q.Operation) || q.AmountMinor <= 0 ||
		!currencyID.MatchString(q.PaymentCurrency) || !currencyID.MatchString(q.QuoteCurrency) {
		return false
	}
	if q.CrossBorder.RuleVersion != "" && !identifier.MatchString(q.CrossBorder.RuleVersion) {
		return false
	}
	switch q.CrossBorder.State {
	case FactKnown:
		return q.CrossBorder.RuleVersion != ""
	case "", FactUnknown, FactRedacted, FactNotApplicable:
		return !q.CrossBorder.Value
	case FactStale:
		return true // A stale historical value is retained but never used.
	default:
		return false
	}
}
