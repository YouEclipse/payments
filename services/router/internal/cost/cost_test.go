package cost

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func costFixture() (Artifact, Query, time.Time) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a := Artifact{
		SchemaVersion: SchemaVersion, Version: "fees_1", CurrencyMetadataVersion: "currency_test_v1", PublishedAt: at.Add(-time.Hour),
		Candidates: []Candidate{{"cand_a", "revision_a"}, {"cand_b", "revision_b"}},
		Currencies: []Currency{{"USD", 2}, {"EUR", 2}, {"JPY", 0}},
	}
	for i, candidate := range a.Candidates {
		fixed, bps, failed := int64(300_000), int64(260), int64(50_000)
		if i == 1 {
			fixed, bps, failed = 100_000, 290, 20_000
		}
		a.Schedules = append(a.Schedules, FeeSchedule{
			ID: "fee_" + candidate.ID, Version: "fee_v1", CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision,
			Operation: "sale", PaymentCurrency: "USD", FixedCurrency: "USD", EffectiveFrom: at.Add(-2 * time.Hour), EffectiveUntil: at.Add(2 * time.Hour),
			PercentageRound: RoundHalfUp, Attempt: Fee{FixedMicros: fixed}, Success: Fee{VariableBPS: bps}, Failure: Fee{FixedMicros: failed},
		})
	}
	q := Query{CandidateID: "cand_a", ConnectionRevision: "revision_a", Operation: "sale", AmountMinor: 1000, PaymentCurrency: "USD", QuoteCurrency: "USD", CrossBorder: CrossBorderFact{State: FactUnknown}}
	return a, q, at
}

func mustCompileCost(t *testing.T, a Artifact) *Evaluator {
	t.Helper()
	e, err := Compile(a)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFeeScenariosReproducePriceCrossingWithoutInventingProbability(t *testing.T) {
	a, q, at := costFixture()
	e := mustCompileCost(t, a)
	for _, tc := range []struct {
		amount       int64
		aCost, bCost int64
	}{{1000, 560_000, 390_000}, {10000, 2_900_000, 3_000_000}} {
		q.AmountMinor = tc.amount
		gotA, err := e.Estimate(at, q)
		if err != nil || gotA.State != StateKnown || gotA.Estimate.SucceededCostMicros != tc.aCost {
			t.Fatalf("A=%+v err=%v", gotA, err)
		}
		qB := q
		qB.CandidateID, qB.ConnectionRevision = "cand_b", "revision_b"
		gotB, err := e.Estimate(at, qB)
		if err != nil || gotB.State != StateKnown || gotB.Estimate.SucceededCostMicros != tc.bCost {
			t.Fatalf("B=%+v err=%v", gotB, err)
		}
		if gotA.Estimate.AttemptCostMicros != 300_000 || gotA.Estimate.FailedCostMicros != 350_000 || gotB.Estimate.FailedCostMicros != 120_000 {
			t.Fatal("failed-attempt fee was omitted or mixed into every success")
		}
		for _, result := range []Result{gotA, gotB} {
			assertScenarioSums(t, result)
		}
	}
}

func assertScenarioSums(t *testing.T, result Result) {
	t.Helper()
	if result.Estimate == nil || result.State != StateKnown || result.ArtifactDigest == "" || result.ArtifactVersion == "" {
		t.Fatal("missing comparable evidence")
	}
	var base, success, failure int64
	for _, item := range result.Estimate.Breakdown {
		if item.QuoteCurrency != result.Estimate.QuoteCurrency || item.AmountMicros < 0 || item.Basis == "" {
			t.Fatalf("invalid breakdown: %+v", item)
		}
		switch item.Scenario {
		case "attempt":
			base += item.AmountMicros
		case "success_only":
			success += item.AmountMicros
		case "failure_only":
			failure += item.AmountMicros
		default:
			t.Fatal("unknown scenario")
		}
	}
	if result.Estimate.AttemptCostMicros != base || result.Estimate.SucceededCostMicros != base+success || result.Estimate.FailedCostMicros != base+failure {
		t.Fatal("scenario totals are not conserved")
	}
}

func TestScheduleHalfOpenIntervalsDoNotDependOnLoadingOrder(t *testing.T) {
	a, q, at := costFixture()
	a.PublishedAt = at.Add(-3 * time.Hour)
	a.Schedules[0].EffectiveUntil = at
	next := a.Schedules[0]
	next.Version, next.EffectiveFrom, next.EffectiveUntil, next.Attempt.FixedMicros = "fee_v2", at, at.Add(time.Hour), 700_000
	a.Schedules = append(a.Schedules, next)
	slices.Reverse(a.Schedules)
	e := mustCompileCost(t, a)
	for _, tc := range []struct {
		at                     time.Time
		version, state, reason string
	}{
		{at.Add(-time.Nanosecond), "fee_v1", StateKnown, ""}, {at, "fee_v2", StateKnown, ""},
		{at.Add(time.Hour), "", StateStale, "cost_stale_schedule"},
		{at.Add(-2*time.Hour - time.Nanosecond), "", StateUnknown, "cost_missing_schedule"},
	} {
		got, err := e.Estimate(tc.at, q)
		if err != nil || got.State != tc.state {
			t.Fatalf("%s: %+v %v", tc.at, got, err)
		}
		if tc.version != "" {
			if got.Estimate == nil || got.Estimate.ScheduleVersion != tc.version {
				t.Fatal("wrong contract at boundary")
			}
		} else if got.Estimate != nil || !slices.Contains(got.ReasonCodes, tc.reason) {
			t.Fatal("unavailable contract became a price")
		}
	}
}

func TestConditionalCrossBorderNeedsVersionedFactInEveryScenario(t *testing.T) {
	for _, scenario := range []string{"attempt", "success", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			a, q, at := costFixture()
			a.Schedules[0].CrossBorderRuleVersion = "cross_border_v2"
			switch scenario {
			case "attempt":
				a.Schedules[0].Attempt.CrossBorderBPS = 100
			case "success":
				a.Schedules[0].Success.CrossBorderBPS = 100
			case "failure":
				a.Schedules[0].Failure.CrossBorderBPS = 100
			}
			e := mustCompileCost(t, a)
			for _, state := range []string{"", FactUnknown, FactRedacted, FactNotApplicable, FactStale} {
				q.CrossBorder = CrossBorderFact{State: state}
				got, err := e.Estimate(at, q)
				if err != nil || got.Estimate != nil || got.State == StateKnown {
					t.Fatalf("%s treated as domestic: %+v %v", state, got, err)
				}
			}
			q.CrossBorder = CrossBorderFact{State: FactKnown, Value: false, RuleVersion: "cross_border_v2"}
			domestic, err := e.Estimate(at, q)
			if err != nil || domestic.State != StateKnown {
				t.Fatal(domestic, err)
			}
			q.CrossBorder.Value = true
			cross, err := e.Estimate(at, q)
			if err != nil || cross.State != StateKnown || cross.Estimate.CrossBorderRuleVersion != "cross_border_v2" {
				t.Fatal(cross, err)
			}
			wantSuccess, wantFailure := int64(0), int64(0)
			if scenario != "failure" {
				wantSuccess = 100_000
			}
			if scenario != "success" {
				wantFailure = 100_000
			}
			if cross.Estimate.SucceededCostMicros-domestic.Estimate.SucceededCostMicros != wantSuccess || cross.Estimate.FailedCostMicros-domestic.Estimate.FailedCostMicros != wantFailure {
				t.Fatal("surcharge entered wrong outcome scenario")
			}
			assertScenarioSums(t, cross)
			q.CrossBorder.Value, q.CrossBorder.RuleVersion = false, "another_geographic_definition"
			mismatch, err := e.Estimate(at, q)
			if err != nil || mismatch.Estimate != nil || mismatch.State != StateUnknown || !slices.Contains(mismatch.ReasonCodes, "cost_cross_border_rule_mismatch") {
				t.Fatal("another contract's domestic classification was trusted", mismatch, err)
			}
		})
	}
}

func TestFXHasExplicitMajorUnitDirectionAndExpiry(t *testing.T) {
	a, q, at := costFixture()
	q.QuoteCurrency = "EUR"
	a.FXQuotes = []FXQuote{{Version: "fx_v1", Source: "contract_fixture", BaseCurrency: "USD", QuoteCurrency: "EUR", Numerator: 9, Denominator: 10, AsOf: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour), Rounding: RoundHalfUp}}
	e := mustCompileCost(t, a)
	got, err := e.Estimate(at, q)
	if err != nil || got.State != StateKnown || got.Estimate.SucceededCostMicros != 504_000 || got.Estimate.FailedCostMicros != 315_000 {
		t.Fatalf("wrong FX direction or scale: %+v, %v", got, err)
	}
	for _, item := range got.Estimate.Breakdown {
		if item.FXVersion != "fx_v1" || item.FXSource != "contract_fixture" || item.FXNumerator != 9 || item.FXDenominator != 10 || item.FXAsOf == nil || item.FXExpiresAt == nil {
			t.Fatal("FX provenance omitted")
		}
	}
	assertScenarioSums(t, got)
	stale, err := e.Estimate(at.Add(time.Hour), q)
	if err != nil || stale.Estimate != nil || stale.State != StateStale || !slices.Contains(stale.ReasonCodes, "cost_stale_fx") {
		t.Fatal("expired FX still priced", stale, err)
	}
	a.FXQuotes[0].BaseCurrency, a.FXQuotes[0].QuoteCurrency = "EUR", "USD"
	missing, err := mustCompileCost(t, a).Estimate(at, q)
	if err != nil || missing.Estimate != nil || !slices.Contains(missing.ReasonCodes, "cost_missing_fx") {
		t.Fatal("inverse FX guessed", missing, err)
	}
}

func TestCostUnknownAndOverflowNeverExposePartialZeroTotals(t *testing.T) {
	for _, tc := range []struct {
		name          string
		edit          func(*Artifact, *Query)
		state, reason string
		wantErr       error
	}{
		{"missing schedule", func(a *Artifact, q *Query) { a.Schedules = nil }, StateUnknown, "cost_missing_schedule", nil},
		{"revision mismatch", func(a *Artifact, q *Query) { q.ConnectionRevision = "different_revision" }, StateUnknown, "cost_candidate_revision_mismatch", nil},
		{"unknown candidate", func(a *Artifact, q *Query) { q.CandidateID = "other" }, StateUnknown, "cost_unknown_candidate", nil},
		{"missing currency metadata", func(a *Artifact, q *Query) { q.PaymentCurrency = "ABC" }, StateUnknown, "cost_missing_currency_metadata", nil},
		{"missing quote metadata", func(a *Artifact, q *Query) { q.QuoteCurrency = "ABC" }, StateUnknown, "cost_missing_currency_metadata", nil},
		{"schedule not yet published", func(a *Artifact, q *Query) { a.PublishedAt = a.PublishedAt.Add(2 * time.Hour) }, StateUnknown, "cost_artifact_not_yet_published", nil},
		{"fixed plus percentage overflow", func(a *Artifact, q *Query) { a.Schedules[0].Attempt.FixedMicros = math.MaxInt64 }, StateInvalid, "cost_arithmetic_overflow", ErrCostOverflow},
		{"failure-only overflow", func(a *Artifact, q *Query) { a.Schedules[0].Failure.FixedMicros = math.MaxInt64 }, StateInvalid, "cost_arithmetic_overflow", ErrCostOverflow},
		{"follow-on cannot be routed", func(a *Artifact, q *Query) { q.Operation = "capture" }, StateInvalid, "cost_invalid_query", ErrInvalidQuery},
		{"zero amount", func(a *Artifact, q *Query) { q.AmountMinor = 0 }, StateInvalid, "cost_invalid_query", ErrInvalidQuery},
		{"known classification needs version", func(a *Artifact, q *Query) { q.CrossBorder = CrossBorderFact{State: FactKnown} }, StateInvalid, "cost_invalid_query", ErrInvalidQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, q, at := costFixture()
			tc.edit(&a, &q)
			got, err := mustCompileCost(t, a).Estimate(at, q)
			if got.State != tc.state || got.Estimate != nil || !slices.Contains(got.ReasonCodes, tc.reason) || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	a, q, at := costFixture()
	a.Schedules[0].Attempt = Fee{}
	a.Schedules[0].Success = Fee{VariableBPS: 1}
	a.Schedules[0].Failure = Fee{}
	q.AmountMinor = math.MaxInt64
	got, err := mustCompileCost(t, a).Estimate(at, q)
	if err != nil || got.Estimate == nil || got.Estimate.SucceededCostMicros != math.MaxInt64 {
		t.Fatal("representable large fee rejected", got, err)
	}
}

func TestCostCompileRejectsAmbiguousOrUnboundArtifacts(t *testing.T) {
	for name, edit := range map[string]func(*Artifact){
		"schema":                    func(a *Artifact) { a.SchemaVersion = "fee_catalog.v99" },
		"version":                   func(a *Artifact) { a.Version = "" },
		"currency metadata version": func(a *Artifact) { a.CurrencyMetadataVersion = "" },
		"empty candidates":          func(a *Artifact) { a.Candidates = nil },
		"duplicate candidate":       func(a *Artifact) { a.Candidates = append(a.Candidates, a.Candidates[0]) },
		"candidate revision":        func(a *Artifact) { a.Schedules[0].ConnectionRevision = "unbound" },
		"duplicate currency":        func(a *Artifact) { a.Currencies = append(a.Currencies, a.Currencies[0]) },
		"currency precision":        func(a *Artifact) { a.Currencies[0].Exponent = 7 },
		"negative fee":              func(a *Artifact) { a.Schedules[0].Failure.FixedMicros = -1 },
		"negative bps":              func(a *Artifact) { a.Schedules[0].Success.VariableBPS = -1 },
		"negative conditional fee":  func(a *Artifact) { a.Schedules[0].Attempt.CrossBorderBPS = -1 },
		"unbound classification":    func(a *Artifact) { a.Schedules[0].Failure.CrossBorderBPS = 1 },
		"no rounding":               func(a *Artifact) { a.Schedules[0].PercentageRound = "" },
		"no fixed currency":         func(a *Artifact) { a.Schedules[0].FixedCurrency = "" },
		"no payment currency":       func(a *Artifact) { a.Schedules[0].PaymentCurrency = "" },
		"follow-on operation":       func(a *Artifact) { a.Schedules[0].Operation = "refund" },
		"empty interval":            func(a *Artifact) { a.Schedules[0].EffectiveUntil = a.Schedules[0].EffectiveFrom },
		"non UTC":                   func(a *Artifact) { a.PublishedAt = a.PublishedAt.In(time.FixedZone("offset", 3600)) },
		"duplicate schedule":        func(a *Artifact) { a.Schedules = append(a.Schedules, a.Schedules[0]) },
		"overlapping schedule": func(a *Artifact) {
			v := a.Schedules[0]
			v.Version = "v2"
			v.EffectiveFrom = v.EffectiveUntil.Add(-time.Nanosecond)
			a.Schedules = append(a.Schedules, v)
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := costFixture()
			edit(&a)
			if got, err := Compile(a); got != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("invalid artifact accepted: %v, %v", got, err)
			}
		})
	}
	for name, edit := range map[string]func(*FXQuote){
		"future observation": func(q *FXQuote) { q.AsOf = q.AsOf.Add(time.Nanosecond) },
		"zero numerator":     func(q *FXQuote) { q.Numerator = 0 },
		"zero denominator":   func(q *FXQuote) { q.Denominator = 0 },
		"same currency":      func(q *FXQuote) { q.QuoteCurrency = q.BaseCurrency },
		"unknown base":       func(q *FXQuote) { q.BaseCurrency = "ABC" },
		"unknown quote":      func(q *FXQuote) { q.QuoteCurrency = "ABC" },
		"empty source":       func(q *FXQuote) { q.Source = "" },
		"bad rounding":       func(q *FXQuote) { q.Rounding = "nearest" },
		"empty lifetime":     func(q *FXQuote) { q.ExpiresAt = q.AsOf },
	} {
		t.Run("FX "+name, func(t *testing.T) {
			a, _, at := costFixture()
			v := FXQuote{Version: "fx_1", Source: "fixture", BaseCurrency: "USD", QuoteCurrency: "EUR", Numerator: 9, Denominator: 10, AsOf: a.PublishedAt, ExpiresAt: at.Add(time.Hour), Rounding: RoundDown}
			edit(&v)
			a.FXQuotes = []FXQuote{v}
			if got, err := Compile(a); got != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatal("bad FX accepted", got, err)
			}
		})
	}
}

func TestCostSnapshotCanonicalOwnershipAndConcurrentReplay(t *testing.T) {
	a, q, at := costFixture()
	before, _ := json.Marshal(a)
	e := mustCompileCost(t, a)
	baseline, err := e.Estimate(at, q)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(a)
	if string(before) != string(after) {
		t.Fatal("compile mutated caller artifact")
	}
	random := rand.New(rand.NewSource(9))
	for i := 0; i < 30; i++ {
		random.Shuffle(len(a.Candidates), func(i, j int) { a.Candidates[i], a.Candidates[j] = a.Candidates[j], a.Candidates[i] })
		random.Shuffle(len(a.Currencies), func(i, j int) { a.Currencies[i], a.Currencies[j] = a.Currencies[j], a.Currencies[i] })
		random.Shuffle(len(a.Schedules), func(i, j int) { a.Schedules[i], a.Schedules[j] = a.Schedules[j], a.Schedules[i] })
		other := mustCompileCost(t, a)
		got, err := other.Estimate(at, q)
		if err != nil || e.Digest() != other.Digest() || !reflect.DeepEqual(got, baseline) {
			t.Fatal("order changed immutable evidence")
		}
	}
	a.Candidates[0].ID = "mutated"
	a.Currencies[0].Exponent = 6
	a.Schedules[0].Attempt.FixedMicros = 0
	copyCandidates := e.Candidates()
	copyCandidates[0].ID = "mutated_return"
	mutated, _ := e.Estimate(at, q)
	mutated.Estimate.Breakdown[0].AmountMicros = 0
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 20; j++ {
				got, err := e.Estimate(at, q)
				if err != nil || !reflect.DeepEqual(got, baseline) {
					t.Error("snapshot changed under concurrent read")
				}
			}
		}()
	}
	group.Wait()
	if value, err := (*Evaluator)(nil).Estimate(at, q); value.Estimate != nil || !errors.Is(err, ErrInvalidArtifact) {
		t.Fatal("nil evaluator must fail closed")
	}
}

func TestFXRejectsDuplicateOrOverlappingSnapshots(t *testing.T) {
	for _, duplicateVersion := range []bool{false, true} {
		a, _, at := costFixture()
		first := FXQuote{Version: "fx_v1", Source: "fixture", BaseCurrency: "USD", QuoteCurrency: "EUR", Numerator: 9, Denominator: 10, AsOf: a.PublishedAt.Add(-2 * time.Hour), ExpiresAt: a.PublishedAt, Rounding: RoundHalfUp}
		second := first
		second.Version, second.AsOf, second.ExpiresAt = "fx_v2", a.PublishedAt.Add(-time.Nanosecond), at.Add(time.Hour)
		if duplicateVersion {
			second.Version, second.AsOf = first.Version, first.ExpiresAt
		}
		a.FXQuotes = []FXQuote{second, first}
		if got, err := Compile(a); got != nil || !errors.Is(err, ErrInvalidArtifact) {
			t.Fatal("ambiguous FX artifact accepted", got, err)
		}
	}
	// Adjacent half-open FX revisions are deterministic in either input order.
	a, q, at := costFixture()
	q.QuoteCurrency = "EUR"
	a.FXQuotes = []FXQuote{
		{Version: "fx_v1", Source: "fixture", BaseCurrency: "USD", QuoteCurrency: "EUR", Numerator: 8, Denominator: 10, AsOf: a.PublishedAt.Add(-time.Hour), ExpiresAt: a.PublishedAt, Rounding: RoundDown},
		{Version: "fx_v2", Source: "fixture", BaseCurrency: "USD", QuoteCurrency: "EUR", Numerator: 9, Denominator: 10, AsOf: a.PublishedAt, ExpiresAt: at.Add(time.Hour), Rounding: RoundDown},
	}
	one := mustCompileCost(t, a)
	slices.Reverse(a.FXQuotes)
	two := mustCompileCost(t, a)
	left, err := one.Estimate(a.PublishedAt, q)
	if err != nil {
		t.Fatal(err)
	}
	right, err := two.Estimate(a.PublishedAt, q)
	if err != nil || one.Digest() != two.Digest() || !reflect.DeepEqual(left, right) || left.Estimate.Breakdown[0].FXVersion != "fx_v2" {
		t.Fatal("FX boundary or ordering changed result")
	}
}

func TestFXUsesMajorUnitsAcrossDifferentMinorExponents(t *testing.T) {
	a, q, at := costFixture()
	a.Schedules[0].PaymentCurrency, a.Schedules[0].FixedCurrency = "JPY", "JPY"
	a.Schedules[0].Attempt.FixedMicros = 30_000_000 // JPY 30, plus 2.6% of JPY 1000.
	q.PaymentCurrency, q.QuoteCurrency = "JPY", "USD"
	a.FXQuotes = []FXQuote{{Version: "jpy_usd_1", Source: "fixture", BaseCurrency: "JPY", QuoteCurrency: "USD", Numerator: 1, Denominator: 150, AsOf: a.PublishedAt, ExpiresAt: at.Add(time.Hour), Rounding: RoundHalfUp}}
	got, err := mustCompileCost(t, a).Estimate(at, q)
	if err != nil || got.Estimate == nil || got.Estimate.AttemptCostMicros != 200_000 || got.Estimate.SucceededCostMicros != 373_333 {
		t.Fatalf("FX mixed minor and major units: %+v, %v", got, err)
	}
	assertScenarioSums(t, got)
}

func TestFXRoundingIsPerComponentAndOverflowIsAtomic(t *testing.T) {
	a, q, at := costFixture()
	a.Schedules[0].FixedCurrency = "EUR"
	a.Schedules[0].Attempt = Fee{FixedMicros: 1}
	a.Schedules[0].Success = Fee{FixedMicros: 1}
	a.Schedules[0].Failure = Fee{}
	a.FXQuotes = []FXQuote{{Version: "eur_usd_1", Source: "fixture", BaseCurrency: "EUR", QuoteCurrency: "USD", Numerator: 3, Denominator: 2, AsOf: a.PublishedAt, ExpiresAt: at.Add(time.Hour), Rounding: RoundUp}}
	e := mustCompileCost(t, a)
	got, err := e.Estimate(at, q)
	if err != nil || got.Estimate == nil || got.Estimate.AttemptCostMicros != 2 || got.Estimate.SucceededCostMicros != 4 {
		t.Fatal("FX rounded the grand total instead of each contracted component", got, err)
	}
	copyResult, _ := e.Estimate(at, q)
	*copyResult.Estimate.Breakdown[0].FXAsOf = time.Time{}
	again, _ := e.Estimate(at, q)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("returned FX timestamp mutated snapshot")
	}
	a.Schedules[0].Attempt.FixedMicros = math.MaxInt64
	a.FXQuotes[0].Numerator, a.FXQuotes[0].Denominator = 2, 1
	overflow, err := mustCompileCost(t, a).Estimate(at, q)
	if !errors.Is(err, ErrCostOverflow) || overflow.Estimate != nil || overflow.State != StateInvalid {
		t.Fatal("FX overflow exposed a partial estimate", overflow, err)
	}
}
