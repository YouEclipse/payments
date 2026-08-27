package scoring

import (
	"math"
	"math/big"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/performance"
)

func validateBatch(at time.Time, profile Profile, batch Batch) (bool, bool, error) {
	invalid := func() (bool, bool, error) { return false, false, ErrInvalidBatch }
	if len(batch.Allowed) > 128 || len(batch.Costs) != len(batch.Allowed) || len(batch.Performance) != len(batch.Allowed) {
		return invalid()
	}
	allowed := make(map[string]string, len(batch.Allowed))
	for _, candidate := range batch.Allowed {
		if !candidateID.MatchString(candidate.ID) || !candidateID.MatchString(candidate.ConnectionRevision) || candidate.RuleRank < 0 || allowed[candidate.ID] != "" {
			return invalid()
		}
		allowed[candidate.ID] = candidate.ConnectionRevision
	}
	seen := make(map[string]bool)
	var costVersion, costDigest, currencyVersion string
	costMissing := false
	for _, value := range batch.Costs {
		if value.ConnectionRevision == "" || allowed[value.CandidateID] != value.ConnectionRevision || seen[value.CandidateID] || !validCost(at, profile.QuoteCurrency, value) {
			return invalid()
		}
		seen[value.CandidateID] = true
		if value.ArtifactVersion != "" {
			if costVersion == "" {
				costVersion, costDigest = value.ArtifactVersion, value.ArtifactDigest
			}
			if costVersion != value.ArtifactVersion || costDigest != value.ArtifactDigest {
				return invalid()
			}
		}
		if value.State != cost.StateKnown {
			costMissing = true
			continue
		}
		if currencyVersion == "" {
			currencyVersion = value.Estimate.CurrencyMetadataVersion
		}
		if currencyVersion != value.Estimate.CurrencyMetadataVersion {
			return invalid()
		}
	}
	seen = make(map[string]bool)
	var contract *performanceContract
	var contextValues [3]string
	performanceMissing := false
	for _, value := range batch.Performance {
		if value.ConnectionRevision == "" || allowed[value.CandidateID] != value.ConnectionRevision || seen[value.CandidateID] || !validPerformance(at, value) {
			return invalid()
		}
		seen[value.CandidateID] = true
		if value.SnapshotVersion != "" {
			current := contractOf(value)
			if contract == nil {
				contract = &current
			} else if *contract != current {
				return invalid()
			}
		}
		if value.State != performance.StateKnown {
			performanceMissing = true
			continue
		}
		// Different fallback levels may suppress a dimension with '*', but
		// concrete source dimensions must describe one frozen payment context.
		key := value.SourceSegment
		for i, dimension := range []string{key.CountryValue, key.NetworkValue, key.Currency} {
			if dimension == performance.Wildcard {
				continue
			}
			if contextValues[i] != "" && contextValues[i] != dimension {
				return invalid()
			}
			contextValues[i] = dimension
		}
	}
	return costMissing, performanceMissing, nil
}

func validCost(at time.Time, quoteCurrency string, result cost.Result) bool {
	if !validReasons(result.ReasonCodes) || !validVersionDigest(result.ArtifactVersion, result.ArtifactDigest) {
		return false
	}
	switch result.State {
	case cost.StateUnknown, cost.StateStale:
		return result.Estimate == nil && (result.State != cost.StateStale || result.ArtifactVersion != "")
	case cost.StateKnown:
		if result.Estimate == nil || result.ArtifactVersion == "" {
			return false
		}
	default:
		return false // invalid is not optional unavailability.
	}
	e := result.Estimate
	if e.QuoteCurrency != quoteCurrency || !e.EvaluatedAt.Equal(at) || !utcInstant(e.EvaluatedAt) ||
		!identifier.MatchString(e.ScheduleID) || !identifier.MatchString(e.ScheduleVersion) || !identifier.MatchString(e.CurrencyMetadataVersion) ||
		(e.CrossBorderRuleVersion != "" && !identifier.MatchString(e.CrossBorderRuleVersion)) ||
		e.AttemptCostMicros < 0 || e.SucceededCostMicros < e.AttemptCostMicros || e.FailedCostMicros < e.AttemptCostMicros ||
		len(e.Breakdown) == 0 || len(e.Breakdown) > 64 {
		return false
	}
	var sums [3]int64
	seen := make(map[[2]string]bool)
	for _, item := range e.Breakdown {
		key := [2]string{item.Scenario, item.Kind}
		if seen[key] || !currencyCode.MatchString(item.SourceCurrency) || item.QuoteCurrency != quoteCurrency || item.AmountMicros < 0 || item.SourceMicros < 0 {
			return false
		}
		seen[key] = true
		index := -1
		switch item.Scenario {
		case "attempt":
			index = 0
		case "success_only":
			index = 1
		case "failure_only":
			index = 2
		default:
			return false
		}
		switch item.Kind {
		case "fixed":
			if item.Basis != "contract_fixed" || item.PercentageRound != "" || item.CrossBorderRuleVersion != "" {
				return false
			}
		case "variable":
			if item.Basis != "payment_amount_bps" || !validRounding(item.PercentageRound) || item.CrossBorderRuleVersion != "" {
				return false
			}
		case "cross_border":
			if item.Basis != "classified_payment_amount_bps" || !validRounding(item.PercentageRound) || !identifier.MatchString(item.CrossBorderRuleVersion) || item.CrossBorderRuleVersion != e.CrossBorderRuleVersion {
				return false
			}
		default:
			return false
		}
		if item.SourceCurrency == quoteCurrency {
			if item.AmountMicros != item.SourceMicros || item.FXVersion != "" || item.FXSource != "" || item.FXNumerator != 0 || item.FXDenominator != 0 || item.FXRounding != "" || item.FXAsOf != nil || item.FXExpiresAt != nil {
				return false
			}
		} else if !identifier.MatchString(item.FXVersion) || !identifier.MatchString(item.FXSource) || item.FXNumerator <= 0 || item.FXDenominator <= 0 || !validRounding(item.FXRounding) ||
			item.FXAsOf == nil || item.FXExpiresAt == nil || !utcInstant(*item.FXAsOf) || !utcInstant(*item.FXExpiresAt) || item.FXAsOf.After(at) || !at.Before(*item.FXExpiresAt) || !validFXAmount(item) {
			return false
		}
		if item.AmountMicros > math.MaxInt64-sums[index] {
			return false
		}
		sums[index] += item.AmountMicros
	}
	if sums[1] > math.MaxInt64-sums[0] || sums[2] > math.MaxInt64-sums[0] {
		return false
	}
	for _, scenario := range []string{"attempt", "success_only", "failure_only"} {
		if !seen[[2]string{scenario, "fixed"}] || !seen[[2]string{scenario, "variable"}] {
			return false
		}
	}
	return sums[0] == e.AttemptCostMicros && sums[0]+sums[1] == e.SucceededCostMicros && sums[0]+sums[2] == e.FailedCostMicros
}

// This checks the self-contained direction/rounding claim in evidence, not the
// market validity of an FX quote. The feature compiler owns the rate artifact.
func validFXAmount(item cost.BreakdownItem) bool {
	numerator := new(big.Int).Mul(big.NewInt(item.SourceMicros), big.NewInt(item.FXNumerator))
	denominator := big.NewInt(item.FXDenominator)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if (item.FXRounding == cost.RoundUp && remainder.Sign() != 0) ||
		(item.FXRounding == cost.RoundHalfUp && new(big.Int).Lsh(remainder, 1).Cmp(denominator) >= 0) {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient.IsInt64() && quotient.Int64() == item.AmountMicros
}

type performanceContract struct {
	version, digest, country       string
	cohort                         performance.Cohort
	start, end, observed, produced string
}

func contractOf(result performance.Result) performanceContract {
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	return performanceContract{result.SnapshotVersion, result.SnapshotDigest, result.CountryField, result.Cohort, stamp(result.WindowStart), stamp(result.WindowEnd), stamp(result.ObservedAt), stamp(result.ProducedAt)}
}

func validPerformance(at time.Time, result performance.Result) bool {
	if !validReasons(result.ReasonCodes) || !hasReason(result.ReasonCodes, "performance_observational_only") ||
		result.Interpretation != performance.ObservationalOnly || !validVersionDigest(result.SnapshotVersion, result.SnapshotDigest) {
		return false
	}
	if result.State != performance.StateKnown && result.State != performance.StatePrior && result.State != performance.StateUnknown && result.State != performance.StateStale {
		return false
	}
	if result.SnapshotVersion == "" {
		return result.State == performance.StateUnknown && result.Estimate == nil && result.Source == "none" && result.SourceSegment == nil && result.ParentSegment == nil && result.FallbackLevel == -1 &&
			result.CountryField == "" && result.Cohort == (performance.Cohort{}) && result.WindowStart.IsZero() && result.WindowEnd.IsZero() && result.ObservedAt.IsZero() && result.ProducedAt.IsZero() &&
			(result.EvaluatedAt.IsZero() || (utcInstant(result.EvaluatedAt) && result.EvaluatedAt.Equal(at)))
	}
	if !utcInstant(result.WindowStart) || !utcInstant(result.WindowEnd) || !utcInstant(result.ObservedAt) || !utcInstant(result.ProducedAt) || !utcInstant(result.EvaluatedAt) ||
		!result.WindowStart.Before(result.WindowEnd) || result.ObservedAt.Before(result.WindowEnd) || result.ProducedAt.Before(result.ObservedAt) || !result.EvaluatedAt.Equal(at) ||
		(result.CountryField != performance.CountryMerchant && result.CountryField != performance.CountryIssuer) || !validCohort(result.Cohort) {
		return false
	}
	if result.State == performance.StateUnknown || result.State == performance.StateStale {
		return result.Estimate == nil && result.Source == "none" && result.SourceSegment == nil && result.ParentSegment == nil && result.FallbackLevel == -1
	}
	if result.ProducedAt.After(at) || result.Estimate == nil || !validPerformanceEstimate(*result.Estimate) {
		return false
	}
	e := result.Estimate
	if result.State == performance.StatePrior {
		return result.Source == "global_prior" && result.SourceSegment == nil && result.ParentSegment == nil && result.FallbackLevel == 4 &&
			e.SampleSize == 0 && e.Successes == 0 && e.PendingCount == 0 && e.CoveragePPM == 0 && e.Confidence == "prior_only" &&
			e.ObservedLower95 == 0 && e.ObservedUpper95 == 1 && e.ConservativePPM == 0 && hasReason(result.ReasonCodes, "performance_cold_start")
	}
	if result.Source != "bucket" || result.SourceSegment == nil || result.FallbackLevel < 0 || result.FallbackLevel > 3 || e.SampleSize == 0 || (e.Confidence != "low" && e.Confidence != "medium" && e.Confidence != "high") {
		return false
	}
	if e.Confidence != performanceConfidence(*e, result.FallbackLevel) {
		return false
	}
	key := *result.SourceSegment
	if key.CandidateID != result.CandidateID || key.ConnectionRevision != result.ConnectionRevision || segmentLevel(key) != result.FallbackLevel {
		return false
	}
	if result.ParentSegment != nil {
		parent := *result.ParentSegment
		if parent.CandidateID != result.CandidateID || parent.ConnectionRevision != result.ConnectionRevision || segmentLevel(parent) <= result.FallbackLevel ||
			(parent.CountryValue != performance.Wildcard && parent.CountryValue != key.CountryValue) ||
			(parent.NetworkValue != performance.Wildcard && parent.NetworkValue != key.NetworkValue) ||
			(parent.Currency != performance.Wildcard && parent.Currency != key.Currency) {
			return false
		}
	}
	return true
}

func validPerformanceEstimate(e performance.Estimate) bool {
	const maxCount = uint64(1 << 53)
	for _, value := range []float64{e.Probability, e.ConservativeProbability, e.ObservedLower95, e.ObservedUpper95, e.ParentProbability} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return false
		}
	}
	if math.IsNaN(e.PriorStrength) || math.IsInf(e.PriorStrength, 0) || e.PriorStrength <= 0 || e.PriorStrength > float64(maxCount) ||
		e.Successes > e.SampleSize || e.SampleSize > maxCount || e.PendingCount > maxCount || e.CoveragePPM > uint32(performance.ProbabilityScale) ||
		e.ObservedLower95 > e.ObservedUpper95 || e.ConservativeProbability != math.Min(e.Probability, e.ObservedLower95) ||
		e.ProbabilityPPM != int64(math.Floor(e.Probability*float64(performance.ProbabilityScale))) || e.ConservativePPM != int64(math.Floor(e.ConservativeProbability*float64(performance.ProbabilityScale))) ||
		e.EstimatorVersion != performance.EstimatorVersion || e.IntervalMethodVersion != performance.IntervalMethodVersion {
		return false
	}
	wantProbability := (float64(e.Successes) + e.PriorStrength*e.ParentProbability) / (float64(e.SampleSize) + e.PriorStrength)
	wantLower, wantUpper := observedWilson95(e.Successes, e.SampleSize)
	if e.Probability != wantProbability || e.ObservedLower95 != wantLower || e.ObservedUpper95 != wantUpper {
		return false
	}
	coverage := uint32(0)
	if e.SampleSize != 0 {
		numerator := new(big.Int).Mul(new(big.Int).SetUint64(e.SampleSize), big.NewInt(performance.ProbabilityScale))
		numerator.Quo(numerator, new(big.Int).SetUint64(e.SampleSize+e.PendingCount))
		coverage = uint32(numerator.Uint64())
	}
	return e.CoveragePPM == coverage
}

// These formulae are pinned by the estimator/interval versions accepted
// above. Recomputing the self-contained claim keeps internally fabricated or
// corrupted rows from becoming a score; it does not establish causality.
func observedWilson95(successes, samples uint64) (float64, float64) {
	if samples == 0 {
		return 0, 1
	}
	const z = 1.959963984540054
	n := float64(samples)
	p := float64(successes) / n
	z2 := z * z
	denominator := 1 + z2/n
	center := (p + z2/(2*n)) / denominator
	radius := z * math.Sqrt((p*(1-p)+z2/(4*n))/n) / denominator
	return math.Max(0, center-radius), math.Min(1, center+radius)
}

func performanceConfidence(value performance.Estimate, level int) string {
	width := value.ObservedUpper95 - value.ObservedLower95
	if level == 0 && value.SampleSize >= 1000 && width <= .05 && value.CoveragePPM >= 990_000 {
		return "high"
	}
	if level < 3 && value.SampleSize >= 100 && width <= .20 && value.CoveragePPM >= 950_000 {
		return "medium"
	}
	return "low"
}

func validCohort(value performance.Cohort) bool {
	if value.PaymentMethod != "card" || value.AttemptKind != "first" || value.DenominatorVersion != performance.DenominatorVersion {
		return false
	}
	return (value.Operation == "sale" && value.CaptureMethod == "automatic" && value.OutcomeDefinition == performance.SaleOutcome) ||
		(value.Operation == "authorize" && value.CaptureMethod == "manual" && value.OutcomeDefinition == performance.AuthorizationOutcome)
}

func segmentLevel(key performance.SegmentKey) int {
	if key.CountryValue == performance.Wildcard && key.NetworkValue == performance.Wildcard && key.Currency == performance.Wildcard {
		return 3
	}
	if !currencyCode.MatchString(key.Currency) {
		return -1
	}
	if key.CountryValue == performance.Wildcard && key.NetworkValue == performance.Wildcard {
		return 2
	}
	if !countryCode.MatchString(key.CountryValue) {
		return -1
	}
	if key.NetworkValue == performance.Wildcard {
		return 1
	}
	switch key.NetworkValue {
	case "visa", "mastercard", "amex", "discover", "jcb", "unionpay", "diners":
		return 0
	default:
		return -1
	}
}

func validRounding(value cost.RoundingMode) bool {
	return value == cost.RoundDown || value == cost.RoundHalfUp || value == cost.RoundUp
}
func validVersionDigest(version, digest string) bool {
	return (version == "" && digest == "") || (identifier.MatchString(version) && digestValue.MatchString(digest))
}
func hasReason(reasons []string, wanted string) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}
func validReasons(reasons []string) bool {
	if len(reasons) == 0 || len(reasons) > 64 {
		return false
	}
	seen := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		if !reasonCode.MatchString(reason) || seen[reason] {
			return false
		}
		seen[reason] = true
	}
	return true
}
