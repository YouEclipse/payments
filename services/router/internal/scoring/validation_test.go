package scoring

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/performance"
)

func TestMalformedEvidenceNeverGetsPermissiveFallback(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	profile.OnUnavailable = StaticOrder
	scorer := mustCompile(t, profile)
	valid := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	for name, edit := range map[string]func(*Batch){
		"missing cost row":                 func(b *Batch) { b.Costs = b.Costs[:1] },
		"missing performance row":          func(b *Batch) { b.Performance = b.Performance[:1] },
		"excluded identity":                func(b *Batch) { b.Allowed = b.Allowed[:1] },
		"duplicate allowed":                func(b *Batch) { b.Allowed[1] = b.Allowed[0] },
		"duplicate cost":                   func(b *Batch) { b.Costs[1] = b.Costs[0] },
		"duplicate performance":            func(b *Batch) { b.Performance[1] = b.Performance[0] },
		"negative rule rank":               func(b *Batch) { b.Allowed[0].RuleRank = -1 },
		"uncanonical candidate":            func(b *Batch) { b.Allowed[0].ID = "Candidate A" },
		"uncanonical revision":             func(b *Batch) { b.Allowed[0].ConnectionRevision = "Revision/A" },
		"long candidate":                   func(b *Batch) { b.Allowed[0].ID = strings.Repeat("a", 129) },
		"cost revision mismatch":           func(b *Batch) { b.Costs[0].ConnectionRevision = "another_revision" },
		"cost empty revision":              func(b *Batch) { b.Costs[0].ConnectionRevision = "" },
		"cost inserted identity":           func(b *Batch) { b.Costs[0].CandidateID = "excluded" },
		"perf revision mismatch":           func(b *Batch) { b.Performance[0].ConnectionRevision = "another_revision" },
		"perf inserted identity":           func(b *Batch) { b.Performance[0].CandidateID = "excluded" },
		"cost invalid state":               func(b *Batch) { b.Costs[0].State = cost.StateInvalid; b.Costs[0].Estimate = nil },
		"cost implicit state":              func(b *Batch) { b.Costs[0].State = "" },
		"cost unknown has value":           func(b *Batch) { b.Costs[0].State = cost.StateUnknown },
		"cost known has no value":          func(b *Batch) { b.Costs[0].Estimate = nil },
		"cost known no contract":           func(b *Batch) { b.Costs[0].ArtifactVersion = ""; b.Costs[0].ArtifactDigest = "" },
		"cost invalid digest":              func(b *Batch) { b.Costs[0].ArtifactDigest = "digest" },
		"cost absent digest":               func(b *Batch) { b.Costs[0].ArtifactDigest = "" },
		"cost invalid version":             func(b *Batch) { b.Costs[0].ArtifactVersion = "other/version" },
		"cost mixed versions":              func(b *Batch) { b.Costs[0].ArtifactVersion = "fees_v2" },
		"cost mixed digests":               func(b *Batch) { b.Costs[0].ArtifactDigest = "sha256:" + strings.Repeat("0", 64) },
		"cost mixed currency metadata":     func(b *Batch) { b.Costs[0].Estimate.CurrencyMetadataVersion = "other_metadata" },
		"cost profile currency mismatch":   func(b *Batch) { b.Costs[0].Estimate.QuoteCurrency = "EUR" },
		"cost prior evaluation":            func(b *Batch) { b.Costs[0].Estimate.EvaluatedAt = at.Add(-time.Nanosecond) },
		"cost future evaluation":           func(b *Batch) { b.Costs[0].Estimate.EvaluatedAt = at.Add(time.Nanosecond) },
		"cost missing schedule id":         func(b *Batch) { b.Costs[0].Estimate.ScheduleID = "" },
		"cost bad schedule version":        func(b *Batch) { b.Costs[0].Estimate.ScheduleVersion = "version/new" },
		"cost missing currency metadata":   func(b *Batch) { b.Costs[0].Estimate.CurrencyMetadataVersion = "" },
		"cost negative attempt":            func(b *Batch) { b.Costs[0].Estimate.AttemptCostMicros = -1 },
		"cost success below attempt":       func(b *Batch) { b.Costs[0].Estimate.SucceededCostMicros = 0 },
		"cost failure below attempt":       func(b *Batch) { b.Costs[0].Estimate.FailedCostMicros = 0 },
		"cost no breakdown":                func(b *Batch) { b.Costs[0].Estimate.Breakdown = nil },
		"cost too many items":              func(b *Batch) { b.Costs[0].Estimate.Breakdown = make([]cost.BreakdownItem, 65) },
		"cost unknown scenario":            func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].Scenario = "every_outcome" },
		"cost unknown kind":                func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].Kind = "expected" },
		"cost negative component":          func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].AmountMicros = -1 },
		"cost negative source":             func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].SourceMicros = -1 },
		"cost invalid source currency":     func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].SourceCurrency = "usd" },
		"cost quote currency mix":          func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].QuoteCurrency = "EUR" },
		"cost fixed wrong basis":           func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].Basis = "payment_amount_bps" },
		"cost fixed unexpected rounding":   func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].PercentageRound = cost.RoundUp },
		"cost percentage missing rounding": func(b *Batch) { b.Costs[0].Estimate.Breakdown[1].PercentageRound = "" },
		"cost percentage wrong basis":      func(b *Batch) { b.Costs[0].Estimate.Breakdown[1].Basis = "contract_fixed" },
		"cost component duplicated": func(b *Batch) {
			b.Costs[0].Estimate.Breakdown = append(b.Costs[0].Estimate.Breakdown, b.Costs[0].Estimate.Breakdown[0])
		},
		"cost zero component omitted": func(b *Batch) {
			b.Costs[0].Estimate.Breakdown = append(b.Costs[0].Estimate.Breakdown[:1], b.Costs[0].Estimate.Breakdown[2:]...)
		},
		"cost total inconsistent":          func(b *Batch) { b.Costs[0].Estimate.SucceededCostMicros++ },
		"cost same currency changed value": func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].SourceMicros++ },
		"cost unnecessary FX":              func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].FXVersion = "fx_v1" },
		"cost missing FX":                  func(b *Batch) { b.Costs[0].Estimate.Breakdown[0].SourceCurrency = "EUR" },
		"cost sum overflow": func(b *Batch) {
			b.Costs[0].Estimate.Breakdown[0].SourceMicros, b.Costs[0].Estimate.Breakdown[0].AmountMicros = math.MaxInt64, math.MaxInt64
			b.Costs[0].Estimate.Breakdown[1].SourceMicros, b.Costs[0].Estimate.Breakdown[1].AmountMicros = 1, 1
		},
		"cost scenario addition overflow": func(b *Batch) {
			b.Costs[0].Estimate.Breakdown[0].SourceMicros, b.Costs[0].Estimate.Breakdown[0].AmountMicros = math.MaxInt64, math.MaxInt64
		},
		"empty reasons":                        func(b *Batch) { b.Costs[0].ReasonCodes = nil },
		"duplicate reasons":                    func(b *Batch) { b.Costs[0].ReasonCodes = append(b.Costs[0].ReasonCodes, b.Costs[0].ReasonCodes[0]) },
		"unsafe reason":                        func(b *Batch) { b.Costs[0].ReasonCodes[0] = "raw user input" },
		"too many reasons":                     func(b *Batch) { b.Costs[0].ReasonCodes = make([]string, 65) },
		"performance invalid state":            func(b *Batch) { b.Performance[0].State = performance.StateInvalid; b.Performance[0].Estimate = nil },
		"performance unknown has value":        func(b *Batch) { b.Performance[0].State = performance.StateUnknown },
		"performance known no value":           func(b *Batch) { b.Performance[0].Estimate = nil },
		"performance known absent contract":    func(b *Batch) { b.Performance[0].SnapshotVersion = ""; b.Performance[0].SnapshotDigest = "" },
		"performance absent digest":            func(b *Batch) { b.Performance[0].SnapshotDigest = "" },
		"performance mixed version":            func(b *Batch) { b.Performance[0].SnapshotVersion = "perf_v2" },
		"performance mixed digest":             func(b *Batch) { b.Performance[0].SnapshotDigest = "sha256:" + strings.Repeat("0", 64) },
		"performance geography definition mix": func(b *Batch) { b.Performance[0].CountryField = performance.CountryMerchant },
		"performance ambiguous country":        func(b *Batch) { b.Performance[0].CountryField = "country" },
		"performance mix valid cohorts": func(b *Batch) {
			b.Performance[0].Cohort.Operation, b.Performance[0].Cohort.CaptureMethod, b.Performance[0].Cohort.OutcomeDefinition = "authorize", "manual", performance.AuthorizationOutcome
		},
		"performance incompatible outcome":          func(b *Batch) { b.Performance[0].Cohort.OutcomeDefinition = performance.AuthorizationOutcome },
		"performance payment method":                func(b *Batch) { b.Performance[0].Cohort.PaymentMethod = "bank_transfer" },
		"performance attempt definition":            func(b *Batch) { b.Performance[0].Cohort.AttemptKind = "all_attempts" },
		"performance denominator":                   func(b *Batch) { b.Performance[0].Cohort.DenominatorVersion = "http_200" },
		"performance not observational":             func(b *Batch) { b.Performance[0].Interpretation = "counterfactual" },
		"performance interpretation reason missing": func(b *Batch) { b.Performance[0].ReasonCodes = []string{"performance_known"} },
		"performance missing window":                func(b *Batch) { b.Performance[0].WindowStart = time.Time{} },
		"performance empty window":                  func(b *Batch) { b.Performance[0].WindowEnd = b.Performance[0].WindowStart },
		"performance mixed windows":                 func(b *Batch) { b.Performance[0].WindowStart = b.Performance[0].WindowStart.Add(time.Nanosecond) },
		"performance future publication":            func(b *Batch) { b.Performance[0].ProducedAt = at.Add(time.Second) },
		"performance observed before window closes": func(b *Batch) { b.Performance[0].ObservedAt = b.Performance[0].WindowEnd.Add(-time.Nanosecond) },
		"performance produced before observed":      func(b *Batch) { b.Performance[0].ProducedAt = b.Performance[0].ObservedAt.Add(-time.Nanosecond) },
		"performance previous evaluation":           func(b *Batch) { b.Performance[0].EvaluatedAt = at.Add(-time.Nanosecond) },
		"performance non UTC":                       func(b *Batch) { b.Performance[0].EvaluatedAt = at.In(time.FixedZone("local", 3600)) },
		"performance missing source":                func(b *Batch) { b.Performance[0].Source = "none" },
		"performance missing source segment":        func(b *Batch) { b.Performance[0].SourceSegment = nil },
		"performance zero known sample":             func(b *Batch) { b.Performance[0].Estimate.SampleSize = 0 },
		"performance invalid confidence":            func(b *Batch) { b.Performance[0].Estimate.Confidence = "certain" },
		"performance understated confidence":        func(b *Batch) { b.Performance[0].Estimate.Confidence = "low" },
		"performance source changed identity":       func(b *Batch) { b.Performance[0].SourceSegment.CandidateID = "cand_b" },
		"performance source changed revision":       func(b *Batch) { b.Performance[0].SourceSegment.ConnectionRevision = "revision_b" },
		"performance source unexpected network":     func(b *Batch) { b.Performance[0].SourceSegment.NetworkValue = "Visa" },
		"performance source ambiguous currency":     func(b *Batch) { b.Performance[0].SourceSegment.Currency = "" },
		"performance mixed context countries":       func(b *Batch) { b.Performance[0].SourceSegment.CountryValue = "FR" },
		"performance mixed context networks":        func(b *Batch) { b.Performance[0].SourceSegment.NetworkValue = "amex" },
		"performance mixed context currencies":      func(b *Batch) { b.Performance[0].SourceSegment.Currency = "EUR" },
		"performance invalid country value":         func(b *Batch) { b.Performance[0].SourceSegment.CountryValue = "germany" },
		"performance incorrect fallback level":      func(b *Batch) { b.Performance[0].FallbackLevel = 1 },
		"performance implicit fallback level":       func(b *Batch) { b.Performance[0].FallbackLevel = -1; b.Performance[0].SourceSegment.CountryValue = "" },
		"performance invalid parent":                func(b *Batch) { key := *b.Performance[0].SourceSegment; b.Performance[0].ParentSegment = &key },
		"performance parent other identity": func(b *Batch) {
			b.Performance[0].ParentSegment = &performance.SegmentKey{CandidateID: "cand_b", ConnectionRevision: "revision_b", CountryValue: "*", NetworkValue: "*", Currency: "*"}
		},
		"performance parent other country": func(b *Batch) {
			b.Performance[0].ParentSegment = &performance.SegmentKey{CandidateID: "cand_a", ConnectionRevision: "revision_a", CountryValue: "FR", NetworkValue: "*", Currency: "USD"}
		},
		"performance NaN probability":       func(b *Batch) { b.Performance[0].Estimate.Probability = math.NaN() },
		"performance infinite interval":     func(b *Batch) { b.Performance[0].Estimate.ObservedUpper95 = math.Inf(1) },
		"performance negative probability":  func(b *Batch) { b.Performance[0].Estimate.Probability = -.1 },
		"performance probability above one": func(b *Batch) { b.Performance[0].Estimate.Probability = 1.1 },
		"performance mismatched point PPM":  func(b *Batch) { b.Performance[0].Estimate.ProbabilityPPM++ },
		"performance fabricated coherent point": func(b *Batch) {
			b.Performance[0].Estimate.Probability, b.Performance[0].Estimate.ProbabilityPPM = .5, 500_000
			b.Performance[0].Estimate.ConservativeProbability, b.Performance[0].Estimate.ConservativePPM = .5, 500_000
		},
		"performance mismatched conservative PPM": func(b *Batch) { b.Performance[0].Estimate.ConservativePPM++ },
		"performance negative conservative PPM":   func(b *Batch) { b.Performance[0].Estimate.ConservativePPM = -1 },
		"performance reversed interval": func(b *Batch) {
			b.Performance[0].Estimate.ObservedLower95, b.Performance[0].Estimate.ObservedUpper95 = 1, 0
		},
		"performance fabricated coherent interval": func(b *Batch) {
			b.Performance[0].Estimate.ObservedLower95, b.Performance[0].Estimate.ObservedUpper95 = .1, .99
			b.Performance[0].Estimate.ConservativeProbability, b.Performance[0].Estimate.ConservativePPM = .1, 100_000
		},
		"performance optimistic conservative value": func(b *Batch) {
			b.Performance[0].Estimate.ConservativeProbability = 1
			b.Performance[0].Estimate.ConservativePPM = 1_000_000
		},
		"performance zero strength":               func(b *Batch) { b.Performance[0].Estimate.PriorStrength = 0 },
		"performance infinite strength":           func(b *Batch) { b.Performance[0].Estimate.PriorStrength = math.Inf(1) },
		"performance large strength":              func(b *Batch) { b.Performance[0].Estimate.PriorStrength = float64(1 << 54) },
		"performance successes exceed sample":     func(b *Batch) { b.Performance[0].Estimate.Successes = 1001 },
		"performance unsafe count":                func(b *Batch) { b.Performance[0].Estimate.SampleSize = 1 << 54 },
		"performance unsafe pending count":        func(b *Batch) { b.Performance[0].Estimate.PendingCount = 1 << 54 },
		"performance impossible coverage":         func(b *Batch) { b.Performance[0].Estimate.CoveragePPM = 1_000_001 },
		"performance inconsistent coverage":       func(b *Batch) { b.Performance[0].Estimate.PendingCount++ },
		"performance unknown estimator":           func(b *Batch) { b.Performance[0].Estimate.EstimatorVersion = "point_estimate" },
		"performance missing interval definition": func(b *Batch) { b.Performance[0].Estimate.IntervalMethodVersion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			batch := copyBatch(t, valid)
			edit(&batch)
			assertInvalidBatch(t, scorer, at, batch)
		})
	}
	batch := copyBatch(t, valid)
	batch.Allowed = make([]Candidate, 129)
	batch.Costs, batch.Performance = make([]cost.Result, 129), make([]performance.Result, 129)
	assertInvalidBatch(t, scorer, at, batch)
}

func TestValidUnavailableRowsDoNotHideOtherMalformedEvidence(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	profile.OnUnavailable = StaticOrder
	fees.Schedules = fees.Schedules[1:]
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	if batch.Costs[0].State != cost.StateUnknown {
		t.Fatal("fixture did not create explicit unavailability")
	}
	batch.Performance[1].Estimate.Probability = math.NaN()
	assertInvalidBatch(t, mustCompile(t, profile), at, batch)
	// A declared prior is valid unavailability, but a corrupted prior is not.
	profile, fees, perf, at = scoringFixture()
	profile.OnUnavailable = StaticOrder
	perf.Buckets = perf.Buckets[1:]
	batch = evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	if batch.Performance[0].State != performance.StatePrior {
		t.Fatal("fixture did not create a prior")
	}
	batch.Performance[0].Estimate.Confidence = "high"
	assertInvalidBatch(t, mustCompile(t, profile), at, batch)
}

func TestExplicitAbsentArtifactsAreUnknownNotNumericDefaults(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	profile.OnUnavailable = StaticOrder
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	batch.Costs[0] = cost.Result{CandidateID: "cand_a", ConnectionRevision: "revision_a", State: cost.StateUnknown, ReasonCodes: []string{"cost_snapshot_unavailable"}}
	batch.Performance[0] = performance.Result{
		CandidateID: "cand_a", ConnectionRevision: "revision_a", State: performance.StateUnknown, ReasonCodes: []string{"performance_observational_only", "performance_snapshot_unavailable"},
		Interpretation: performance.ObservationalOnly, Source: "none", FallbackLevel: -1, EvaluatedAt: at,
	}
	result, err := mustCompile(t, profile).Score(at, batch)
	if err != nil || result.Status != StatusStaticFallback || !reflect.DeepEqual(result.CandidateOrder, []string{"cand_a", "cand_b"}) || result.Breakdown[0].Numbers != nil || result.Breakdown[1].Numbers != nil {
		t.Fatal("explicit unavailable producer became fabricated numeric evidence", result, err)
	}
	for _, mutate := range []func(*Batch){
		func(b *Batch) { b.Costs[0].State = cost.StateStale },
		func(b *Batch) { b.Performance[0].State = performance.StateStale },
		func(b *Batch) { b.Performance[0].CountryField = performance.CountryIssuer },
		func(b *Batch) { b.Performance[0].ProducedAt = at },
		func(b *Batch) { b.Performance[0].EvaluatedAt = at.Add(-time.Second) },
	} {
		bad := copyBatch(t, batch)
		mutate(&bad)
		assertInvalidBatch(t, mustCompile(t, profile), at, bad)
	}
}

func TestDirectedFXEvidenceAndHierarchyArePreserved(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	profile.OnUnavailable = StaticOrder
	for i := range fees.Schedules {
		fees.Schedules[i].FixedCurrency = "EUR"
	}
	fees.FXQuotes = []cost.FXQuote{{Version: "fx_v1", Source: "fixture_contract", BaseCurrency: "EUR", QuoteCurrency: "USD", Numerator: 11, Denominator: 10, AsOf: fees.PublishedAt, ExpiresAt: at.Add(time.Hour), Rounding: cost.RoundHalfUp}}
	parent := perf.Buckets[0]
	parent.Segment.NetworkValue, parent.Successes = performance.Wildcard, 850
	perf.Buckets = append(perf.Buckets, parent)
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	result, err := mustCompile(t, profile).Score(at, batch)
	if err != nil || result.Status != StatusScored || result.Breakdown[0].Cost.Estimate.SucceededCostMicros != 420_000 || result.Breakdown[0].Performance.ParentSegment == nil {
		t.Fatal("valid directed FX or hierarchical observation rejected", result, err)
	}
	for name, edit := range map[string]func(*cost.BreakdownItem){
		"FX missing version":      func(i *cost.BreakdownItem) { i.FXVersion = "" },
		"FX missing source":       func(i *cost.BreakdownItem) { i.FXSource = "" },
		"FX negative rate":        func(i *cost.BreakdownItem) { i.FXNumerator = -1 },
		"FX inconsistent rate":    func(i *cost.BreakdownItem) { i.FXNumerator = 9 },
		"FX zero denominator":     func(i *cost.BreakdownItem) { i.FXDenominator = 0 },
		"FX unsupported rounding": func(i *cost.BreakdownItem) { i.FXRounding = "bankers" },
		"FX missing timestamp":    func(i *cost.BreakdownItem) { i.FXAsOf = nil },
		"FX future timestamp":     func(i *cost.BreakdownItem) { future := at.Add(time.Second); i.FXAsOf = &future },
		"FX expired timestamp":    func(i *cost.BreakdownItem) { expiry := at; i.FXExpiresAt = &expiry },
	} {
		t.Run(name, func(t *testing.T) {
			bad := copyBatch(t, batch)
			edit(&bad.Costs[0].Estimate.Breakdown[0])
			assertInvalidBatch(t, mustCompile(t, profile), at, bad)
		})
	}
	// Nested timestamp pointers in returned evidence are independently owned.
	*batch.Costs[0].Estimate.Breakdown[0].FXAsOf = at
	if result.Breakdown[0].Cost.Estimate.Breakdown[0].FXAsOf.Equal(at) {
		t.Fatal("returned FX metadata aliases the source batch")
	}
}

func TestFXRoundingClaimUsesExactIntegerArithmetic(t *testing.T) {
	for _, tc := range []struct {
		mode                                   cost.RoundingMode
		source, numerator, denominator, amount int64
		valid                                  bool
	}{
		{cost.RoundDown, 1, 1, 2, 0, true},
		{cost.RoundHalfUp, 1, 1, 2, 1, true},
		{cost.RoundUp, 1, 1, 2, 1, true},
		{cost.RoundDown, 2, 1, 2, 1, true},
		{cost.RoundHalfUp, 1, 1, 3, 0, true},
		{cost.RoundHalfUp, 2, 1, 3, 1, true},
		{cost.RoundUp, 0, 1, 3, 0, true},
		{cost.RoundDown, 1, 1, 2, 1, false},
		{cost.RoundDown, math.MaxInt64, 2, 1, math.MaxInt64, false},
	} {
		item := cost.BreakdownItem{SourceMicros: tc.source, AmountMicros: tc.amount, FXNumerator: tc.numerator, FXDenominator: tc.denominator, FXRounding: tc.mode}
		if got := validFXAmount(item); got != tc.valid {
			t.Fatalf("%+v valid=%v want %v", item, got, tc.valid)
		}
	}
}

func TestVersionedCrossBorderBasisCannotBeReclassifiedInScoring(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	fees.Schedules[0].CrossBorderRuleVersion = "cross_border_v1"
	fees.Schedules[0].Success.CrossBorderBPS = 10
	evaluator, err := cost.Compile(fees)
	if err != nil {
		t.Fatal(err)
	}
	batch := evaluatedBatch(t, fees, perf, at, profile.QuoteCurrency)
	batch.Costs[0], err = evaluator.Estimate(at, cost.Query{CandidateID: "cand_a", ConnectionRevision: "revision_a", Operation: "sale", AmountMinor: 1000, PaymentCurrency: "USD", QuoteCurrency: "USD", CrossBorder: cost.CrossBorderFact{State: cost.FactKnown, Value: true, RuleVersion: "cross_border_v1"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := mustCompile(t, profile).Score(at, batch)
	if err != nil || result.Status != StatusScored || result.Breakdown[0].Cost.Estimate.CrossBorderRuleVersion != "cross_border_v1" {
		t.Fatal("versioned conditional price lost its basis", result, err)
	}
	for name, edit := range map[string]func(*cost.Estimate){
		"different classifier": func(e *cost.Estimate) { e.CrossBorderRuleVersion = "another_classifier" },
		"missing classifier":   func(e *cost.Estimate) { e.CrossBorderRuleVersion = "" },
		"invalid classifier":   func(e *cost.Estimate) { e.CrossBorderRuleVersion = "invalid/classifier" },
		"unclassified conditional item": func(e *cost.Estimate) {
			for i := range e.Breakdown {
				if e.Breakdown[i].Kind == "cross_border" {
					e.Breakdown[i].Basis = "payment_amount_bps"
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := copyBatch(t, batch)
			edit(bad.Costs[0].Estimate)
			assertInvalidBatch(t, mustCompile(t, profile), at, bad)
		})
	}
}
