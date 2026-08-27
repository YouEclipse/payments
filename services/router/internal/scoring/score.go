package scoring

import (
	"encoding/json"
	"math/big"
	"sort"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/performance"
)

// Score requires exactly one Cost and Performance row for every allowed
// identity. Explicit unknown/stale/prior rows may trigger the declared
// whole-batch fallback. Missing rows, malformed data, and mixed contracts never
// become a permissive fallback. Callers must not mutate Batch during this call.
func (s *Scorer) Score(at time.Time, input Batch) (Result, error) {
	result := Result{Status: StatusRejected, Mode: Reject, CandidateOrder: []string{}, ReasonCodes: []string{}, ProfileVersion: s.Version(), ProfileDigest: s.Digest(), ObjectiveVersion: ObjectiveVersion, Breakdown: []ScoreBreakdown{}}
	if s == nil || s.digest == "" {
		result.ReasonCodes = []string{"scoring_invalid_profile"}
		return result, ErrInvalidProfile
	}
	if !s.ValidAt(at) {
		result.ReasonCodes = []string{"scoring_profile_not_effective"}
		return result, ErrProfileNotEffective
	}
	result.EvaluatedAt = at.UTC()
	costMissing, performanceMissing, err := validateBatch(at, s.profile, input)
	if err != nil {
		result.ReasonCodes = []string{"scoring_invalid_batch"}
		return result, ErrInvalidBatch
	}
	if len(input.Allowed) == 0 {
		result.ReasonCodes = []string{"scoring_no_allowed_candidate"}
		return result, nil
	}
	// Validation bounded all collections, strings, numbers and instants before
	// cloning. Returned nested evidence never aliases the caller's snapshots.
	encoded, err := json.Marshal(input)
	if err != nil {
		result.ReasonCodes = []string{"scoring_invalid_batch"}
		return result, ErrInvalidBatch
	}
	var batch Batch
	if err := json.Unmarshal(encoded, &batch); err != nil {
		result.ReasonCodes = []string{"scoring_invalid_batch"}
		return result, ErrInvalidBatch
	}
	sort.Slice(batch.Allowed, func(i, j int) bool { return batch.Allowed[i].ID < batch.Allowed[j].ID })
	costs := make(map[string]cost.Result, len(batch.Costs))
	performances := make(map[string]performance.Result, len(batch.Performance))
	for _, value := range batch.Costs {
		costs[value.CandidateID] = value
	}
	for _, value := range batch.Performance {
		performances[value.CandidateID] = value
	}
	for _, candidate := range batch.Allowed {
		result.Breakdown = append(result.Breakdown, ScoreBreakdown{CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, RuleRank: candidate.RuleRank, ProfileVersion: s.Version(), ProfileDigest: s.Digest(), Cost: costs[candidate.ID], Performance: performances[candidate.ID]})
	}
	if costMissing || performanceMissing {
		if costMissing {
			result.ReasonCodes = append(result.ReasonCodes, "cost_evidence_unavailable")
		}
		if performanceMissing {
			result.ReasonCodes = append(result.ReasonCodes, "performance_evidence_unavailable")
		}
		if s.profile.OnUnavailable == Reject {
			result.ReasonCodes = append(result.ReasonCodes, "scoring_feature_required")
			return result, nil
		}
		result.Status, result.Mode = StatusStaticFallback, StaticOrder
		result.ReasonCodes = append(result.ReasonCodes, "cost_stage_abstained", "performance_stage_abstained", "scoring_static_order")
		sort.Slice(batch.Allowed, func(i, j int) bool {
			a, b := batch.Allowed[i], batch.Allowed[j]
			if a.RuleRank != b.RuleRank {
				return a.RuleRank < b.RuleRank
			}
			return a.ID < b.ID
		})
		for _, candidate := range batch.Allowed {
			result.CandidateOrder = append(result.CandidateOrder, candidate.ID)
		}
		sort.Strings(result.ReasonCodes)
		return result, nil
	}
	result.Status, result.Mode = StatusScored, PrimaryFull
	result.ReasonCodes = []string{"scoring_full_v1", "scoring_observational_objectives"}
	for i := range result.Breakdown {
		row := &result.Breakdown[i]
		value := normalize(s.profile, row.Cost.Estimate.SucceededCostMicros, row.Performance.Estimate.ConservativePPM)
		row.Numbers = &value
	}
	indexes := make([]int, len(result.Breakdown))
	for i := range indexes {
		indexes[i] = i
	}
	sort.Slice(indexes, func(i, j int) bool {
		a, b := result.Breakdown[indexes[i]], result.Breakdown[indexes[j]]
		if a.Numbers.TotalPoints != b.Numbers.TotalPoints {
			return a.Numbers.TotalPoints > b.Numbers.TotalPoints
		}
		if a.RuleRank != b.RuleRank {
			return a.RuleRank < b.RuleRank
		}
		return a.CandidateID < b.CandidateID
	})
	for _, index := range indexes {
		result.CandidateOrder = append(result.CandidateOrder, result.Breakdown[index].CandidateID)
	}
	if len(indexes) > 1 && result.Breakdown[indexes[0]].Numbers.TotalPoints == result.Breakdown[indexes[1]].Numbers.TotalPoints {
		result.ReasonCodes = append(result.ReasonCodes, "scoring_stable_tie_break")
	}
	return result, nil
}

// Inputs have already passed range checks. The only potentially wide product
// is the cost range multiplied by 10,000; use an exact bounded integer before
// division so even an int64-wide normalization interval is valid.
func normalize(profile Profile, costMicros, successPPM int64) Numbers {
	clamped := min(max(costMicros, profile.CostFloorMicros), profile.CostCeilingMicros)
	rangeMicros := profile.CostCeilingMicros - profile.CostFloorMicros
	numerator := new(big.Int).Mul(big.NewInt(profile.CostCeilingMicros-clamped), big.NewInt(PointScale))
	costPoints := numerator.Quo(numerator, big.NewInt(rangeMicros)).Int64()
	successPoints := (successPPM*PointScale + performance.ProbabilityScale/2) / performance.ProbabilityScale
	costContribution := costPoints * profile.CostWeightBPS / PointScale
	successContribution := successPoints * profile.SuccessWeightBPS / PointScale
	return Numbers{SucceededCostMicros: costMicros, QuoteCurrency: profile.QuoteCurrency, ConservativePPM: successPPM, CostFloorMicros: profile.CostFloorMicros, CostCeilingMicros: profile.CostCeilingMicros, ClampedCostMicros: clamped, SuccessWeightBPS: profile.SuccessWeightBPS, CostWeightBPS: profile.CostWeightBPS, SuccessPoints: successPoints, CostPoints: costPoints, SuccessContribution: successContribution, CostContribution: costContribution, TotalPoints: successContribution + costContribution, RoundingVersion: RoundingVersion}
}
