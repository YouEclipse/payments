package scoring

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/performance"
)

// Fixtures are evaluated by the real feature packages. These tests do not
// manufacture a bare score without the source contract and provenance.
func scoringFixture() (Profile, cost.Artifact, performance.Artifact, time.Time) {
	at := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	profile := Profile{
		SchemaVersion: SchemaVersion, Version: "score_v1", PublishedAt: at.Add(-2 * time.Hour), EffectiveFrom: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour),
		QuoteCurrency: "USD", SuccessWeightBPS: 7000, CostWeightBPS: 3000, CostFloorMicros: 100_000, CostCeilingMicros: 1_100_000, Primary: PrimaryFull, OnUnavailable: Reject,
	}
	fees := cost.Artifact{
		SchemaVersion: cost.SchemaVersion, Version: "fees_v1", CurrencyMetadataVersion: "currencies_v1", PublishedAt: at.Add(-2 * time.Hour),
		Candidates: []cost.Candidate{{ID: "cand_a", ConnectionRevision: "revision_a"}, {ID: "cand_b", ConnectionRevision: "revision_b"}},
		Currencies: []cost.Currency{{Code: "USD", Exponent: 2}, {Code: "EUR", Exponent: 2}},
	}
	for i, candidate := range fees.Candidates {
		fees.Schedules = append(fees.Schedules, cost.FeeSchedule{
			ID: "fee_" + candidate.ID, Version: "fee_v1", CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision,
			Operation: "sale", PaymentCurrency: "USD", FixedCurrency: "USD", EffectiveFrom: at.Add(-time.Hour), EffectiveUntil: at.Add(time.Hour),
			PercentageRound: cost.RoundHalfUp, Attempt: cost.Fee{FixedMicros: 200_000 + int64(i)*100_000}, Success: cost.Fee{VariableBPS: 200}, Failure: cost.Fee{FixedMicros: 50_000},
		})
	}
	perf := performance.Artifact{
		SchemaVersion: performance.SchemaVersion, Version: "perf_v1", SegmentSchemaVersion: performance.SegmentSchemaVersion, EstimatorVersion: performance.EstimatorVersion, CountryField: performance.CountryIssuer,
		Cohort:      performance.Cohort{PaymentMethod: "card", Operation: "sale", CaptureMethod: "automatic", AttemptKind: "first", OutcomeDefinition: performance.SaleOutcome, DenominatorVersion: performance.DenominatorVersion},
		WindowStart: at.Add(-24 * time.Hour), WindowEnd: at.Add(-30 * time.Minute), ObservedAt: at.Add(-20 * time.Minute), ProducedAt: at.Add(-10 * time.Minute),
		MaxAge: time.Hour, MaxWindowAge: 2 * time.Hour, MinSamples: []uint64{25, 50, 100, 100}, MinCoveragePPM: 950_000, Prior: performance.Prior{Probability: .85, Strength: 20},
		Candidates: []performance.Candidate{{ID: "cand_a", ConnectionRevision: "revision_a"}, {ID: "cand_b", ConnectionRevision: "revision_b"}},
		Buckets: []performance.Bucket{
			{Segment: performance.SegmentKey{CandidateID: "cand_a", ConnectionRevision: "revision_a", CountryValue: "DE", NetworkValue: "visa", Currency: "USD"}, Successes: 900, SampleSize: 1000},
			{Segment: performance.SegmentKey{CandidateID: "cand_b", ConnectionRevision: "revision_b", CountryValue: "DE", NetworkValue: "visa", Currency: "USD"}, Successes: 950, SampleSize: 1000},
		},
	}
	return profile, fees, perf, at
}

func evaluatedBatch(t *testing.T, fees cost.Artifact, perf performance.Artifact, at time.Time, quote string) Batch {
	t.Helper()
	evaluator, err := cost.Compile(fees)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := performance.Compile(perf)
	if err != nil {
		t.Fatal(err)
	}
	var batch Batch
	for i, candidate := range fees.Candidates {
		batch.Allowed = append(batch.Allowed, Candidate{ID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, RuleRank: i})
		price, err := evaluator.Estimate(at, cost.Query{
			CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, Operation: "sale", AmountMinor: 1000,
			PaymentCurrency: "USD", QuoteCurrency: quote, CrossBorder: cost.CrossBorderFact{State: cost.FactUnknown},
		})
		if err != nil {
			t.Fatal(err)
		}
		observed, err := snapshot.Lookup(at, performance.Query{
			CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, CountryField: performance.CountryIssuer,
			CountryState: performance.StateKnown, CountryValue: "DE", NetworkState: performance.StateKnown, NetworkValue: "visa",
			Currency: "USD", Operation: "sale", CaptureMethod: "automatic",
		})
		if err != nil {
			t.Fatal(err)
		}
		batch.Costs = append(batch.Costs, price)
		batch.Performance = append(batch.Performance, observed)
	}
	return batch
}

func mustCompile(t *testing.T, profile Profile) *Scorer {
	t.Helper()
	scorer, err := Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	return scorer
}

func copyBatch(t *testing.T, input Batch) Batch {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var output Batch
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestFullScoreHasExactIntegerGoldenAndAllProvenance(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	scorer := mustCompile(t, profile)
	result, err := scorer.Score(at, batch)
	if err != nil || result.Status != StatusScored || result.Mode != PrimaryFull || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_b", "cand_a"}) {
		t.Fatalf("wrong complete ranking: %+v, %v", result, err)
	}
	if result.ObjectiveVersion != ObjectiveVersion || result.ProfileVersion != scorer.Version() || result.ProfileDigest != scorer.Digest() || !result.EvaluatedAt.Equal(at) {
		t.Fatal("scoring contract missing")
	}
	for i, expected := range []Numbers{
		{SucceededCostMicros: 400_000, QuoteCurrency: "USD", ConservativePPM: 879848, CostFloorMicros: 100_000, CostCeilingMicros: 1_100_000, ClampedCostMicros: 400_000, SuccessWeightBPS: 7000, CostWeightBPS: 3000, SuccessPoints: 8798, CostPoints: 7000, SuccessContribution: 6158, CostContribution: 2100, TotalPoints: 8258, RoundingVersion: RoundingVersion},
		{SucceededCostMicros: 500_000, QuoteCurrency: "USD", ConservativePPM: 934686, CostFloorMicros: 100_000, CostCeilingMicros: 1_100_000, ClampedCostMicros: 500_000, SuccessWeightBPS: 7000, CostWeightBPS: 3000, SuccessPoints: 9347, CostPoints: 6000, SuccessContribution: 6542, CostContribution: 1800, TotalPoints: 8342, RoundingVersion: RoundingVersion},
	} {
		row := result.Breakdown[i]
		if row.Numbers == nil || *row.Numbers != expected {
			t.Fatalf("integer golden changed for %s: %+v", row.CandidateID, row.Numbers)
		}
		if row.CandidateID != batch.Allowed[i].ID || row.ConnectionRevision != batch.Allowed[i].ConnectionRevision || row.RuleRank != batch.Allowed[i].RuleRank || row.ProfileVersion != result.ProfileVersion || row.ProfileDigest != result.ProfileDigest ||
			!reflect.DeepEqual(row.Cost, batch.Costs[i]) || !reflect.DeepEqual(row.Performance, batch.Performance[i]) {
			t.Fatal("identity or raw feature provenance was rewritten")
		}
	}
	if result.Breakdown[0].Numbers.ConservativePPM == batch.Performance[0].Estimate.ProbabilityPPM {
		t.Fatal("point estimate replaced the conservative objective")
	}
}

func TestUnavailableIsWholeBatchAndPriorNeverBecomesAZero(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		edit                            func(*cost.Artifact, *performance.Artifact)
		costMissing, performanceMissing bool
	}{
		{"missing fee", func(f *cost.Artifact, p *performance.Artifact) { f.Schedules = f.Schedules[1:] }, true, false},
		{"expired fee", func(f *cost.Artifact, p *performance.Artifact) {
			f.Schedules[0].EffectiveUntil = f.Schedules[0].EffectiveFrom.Add(time.Minute)
		}, true, false},
		{"missing fx", func(f *cost.Artifact, p *performance.Artifact) { f.Schedules[0].FixedCurrency = "EUR" }, true, false},
		{"explicit prior", func(f *cost.Artifact, p *performance.Artifact) { p.Buckets = p.Buckets[1:] }, false, true},
		{"stale snapshot", func(f *cost.Artifact, p *performance.Artifact) { p.MaxAge = time.Nanosecond }, false, true},
		{"both missing", func(f *cost.Artifact, p *performance.Artifact) { f.Schedules = nil; p.Buckets = nil }, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, fees, perf, at := scoringFixture()
			tc.edit(&fees, &perf)
			batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
			for _, action := range []string{StaticOrder, Reject} {
				profile.OnUnavailable = action
				result, err := mustCompile(t, profile).Score(at, batch)
				if err != nil || len(result.Breakdown) != 2 {
					t.Fatal("valid unavailability became invalid metadata", result, err)
				}
				for _, row := range result.Breakdown {
					if row.Numbers != nil {
						t.Fatal("one candidate got a score in a partial feature space")
					}
				}
				if hasReason(result.ReasonCodes, "cost_evidence_unavailable") != tc.costMissing || hasReason(result.ReasonCodes, "performance_evidence_unavailable") != tc.performanceMissing {
					t.Fatal("wrong unavailability explanation", result.ReasonCodes)
				}
				if action == StaticOrder {
					if result.Status != StatusStaticFallback || result.Mode != StaticOrder || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_a", "cand_b"}) ||
						!hasReason(result.ReasonCodes, "cost_stage_abstained") || !hasReason(result.ReasonCodes, "performance_stage_abstained") {
						t.Fatal("static fallback did not abstain both stages", result)
					}
				} else if result.Status != StatusRejected || result.Mode != Reject || len(result.CandidateOrder) != 0 || !hasReason(result.ReasonCodes, "scoring_feature_required") {
					t.Fatal("required features silently defaulted", result)
				}
			}
		})
	}
}

func TestZeroWeightStillRequiresBothFeatureStages(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	fees.Schedules = nil
	profile.SuccessWeightBPS, profile.CostWeightBPS = PointScale, 0
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	result, err := mustCompile(t, profile).Score(at, batch)
	if err != nil || result.Status != StatusRejected || len(result.CandidateOrder) != 0 || !hasReason(result.ReasonCodes, "cost_evidence_unavailable") {
		t.Fatal("full profile bypassed the zero-weight stage", result, err)
	}
}

func TestEligibilityIsAuthoritativeEvenWhenExcludedCandidateHasBestScore(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	// cand_b wins the full fixture, but the preceding hard gate only admits A.
	batch.Allowed, batch.Costs, batch.Performance = batch.Allowed[:1], batch.Costs[:1], batch.Performance[:1]
	result, err := mustCompile(t, profile).Score(at, batch)
	if err != nil || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_a"}) || len(result.Breakdown) != 1 {
		t.Fatal("scoring resurrected an excluded candidate", result, err)
	}
	result, err = mustCompile(t, profile).Score(at, Batch{})
	if err != nil || result.Status != StatusRejected || len(result.CandidateOrder) != 0 || !hasReason(result.ReasonCodes, "scoring_no_allowed_candidate") {
		t.Fatal("empty eligibility acquired a winner", result, err)
	}
}

func TestStableTieUsesRuleRankThenCandidateIDAndNeverInputOrder(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	fees.Schedules[1].Attempt.FixedMicros = fees.Schedules[0].Attempt.FixedMicros
	perf.Buckets[1].Successes = perf.Buckets[0].Successes
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	batch.Allowed[0].RuleRank, batch.Allowed[1].RuleRank = 1, 0
	scorer := mustCompile(t, profile)
	result, err := scorer.Score(at, batch)
	if err != nil || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_b", "cand_a"}) || !hasReason(result.ReasonCodes, "scoring_stable_tie_break") {
		t.Fatal("rule-rank tie break changed", result, err)
	}
	batch.Allowed[0].RuleRank = 0
	result, err = scorer.Score(at, batch)
	if err != nil || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_a", "cand_b"}) {
		t.Fatal("candidate-id tie break changed", result, err)
	}
	random := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		random.Shuffle(len(batch.Allowed), func(i, j int) { batch.Allowed[i], batch.Allowed[j] = batch.Allowed[j], batch.Allowed[i] })
		random.Shuffle(len(batch.Costs), func(i, j int) { batch.Costs[i], batch.Costs[j] = batch.Costs[j], batch.Costs[i] })
		random.Shuffle(len(batch.Performance), func(i, j int) {
			batch.Performance[i], batch.Performance[j] = batch.Performance[j], batch.Performance[i]
		})
		got, err := scorer.Score(at, batch)
		if err != nil || !reflect.DeepEqual(got, result) {
			t.Fatal("equivalent input permutation changed full evidence", got, err)
		}
	}
	profile.OnUnavailable = StaticOrder
	fees.Schedules = nil
	batch = evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	batch.Allowed[1].RuleRank = batch.Allowed[0].RuleRank
	slices.Reverse(batch.Allowed)
	result, err = mustCompile(t, profile).Score(at, batch)
	if err != nil || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_a", "cand_b"}) {
		t.Fatal("static tie used iteration order", result, err)
	}
}

func TestAddingAnIrrelevantCandidateCannotRenormalizeExistingScores(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	before, err := mustCompile(t, profile).Score(at, evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency))
	if err != nil {
		t.Fatal(err)
	}
	fees.Candidates = append(fees.Candidates, cost.Candidate{ID: "cand_c", ConnectionRevision: "revision_c"})
	fee := fees.Schedules[0]
	fee.ID, fee.CandidateID, fee.ConnectionRevision, fee.Attempt.FixedMicros = "fee_cand_c", "cand_c", "revision_c", 9_000_000
	fees.Schedules = append(fees.Schedules, fee)
	perf.Candidates = append(perf.Candidates, performance.Candidate{ID: "cand_c", ConnectionRevision: "revision_c"})
	bucket := perf.Buckets[0]
	bucket.Segment.CandidateID, bucket.Segment.ConnectionRevision, bucket.Successes = "cand_c", "revision_c", 100
	perf.Buckets = append(perf.Buckets, bucket)
	after, err := mustCompile(t, profile).Score(at, evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency))
	if err != nil || !reflect.DeepEqual(after.CandidateOrder, []string{"cand_b", "cand_a", "cand_c"}) {
		t.Fatal(after, err)
	}
	for i := range before.Breakdown {
		if *before.Breakdown[i].Numbers != *after.Breakdown[i].Numbers {
			t.Fatal("candidate-set-dependent min/max changed an existing score")
		}
	}
}

func TestFeatureEvidenceAndCompiledProfileAreOwnedSnapshots(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	scorer := mustCompile(t, profile)
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	pristine := copyBatch(t, batch)
	baseline, err := scorer.Score(at, batch)
	if err != nil {
		t.Fatal(err)
	}
	profile.Version, profile.CostWeightBPS, profile.SuccessWeightBPS = "mutated", 10000, 0
	batch.Allowed[0].ID = "mutated"
	batch.Costs[0].ReasonCodes[0] = "mutated"
	batch.Costs[0].Estimate.Breakdown[0].AmountMicros = 99
	batch.Performance[0].Estimate.Probability = .1
	batch.Performance[0].SourceSegment.CountryValue = "FR"
	got, err := scorer.Score(at, pristine)
	if err != nil || !reflect.DeepEqual(got, baseline) {
		t.Fatal("caller mutation changed compiled profile or returned raw snapshot", got, err)
	}
	baseline.Breakdown[0].Cost.Estimate.Breakdown[0].AmountMicros = 1
	baseline.Breakdown[0].Performance.SourceSegment.NetworkValue = "amex"
	baseline.Breakdown[0].Numbers.TotalPoints = 0
	baseline.CandidateOrder[0] = "mutated"
	again, err := scorer.Score(at, pristine)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatal("mutating returned evidence changed subsequent scoring", again, err)
	}
}

func TestConcurrentScoringIsDeterministicAndRaceFree(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	scorer := mustCompile(t, profile)
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	want, err := scorer.Score(at, batch)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				got, err := scorer.Score(at, batch)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Error("concurrent evaluation changed result", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func assertInvalidBatch(t *testing.T, scorer *Scorer, at time.Time, batch Batch) {
	t.Helper()
	got, err := scorer.Score(at, batch)
	if !errors.Is(err, ErrInvalidBatch) || got.Status != StatusRejected || got.Mode != Reject || len(got.CandidateOrder) != 0 || len(got.Breakdown) != 0 || !hasReason(got.ReasonCodes, "scoring_invalid_batch") {
		t.Fatalf("malformed evidence was scored or fell back: %+v, %v", got, err)
	}
}
