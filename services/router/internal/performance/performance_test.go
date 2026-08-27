package performance

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

func performanceFixture() (Artifact, Query, time.Time) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a := Artifact{
		SchemaVersion: SchemaVersion, Version: "performance_v1", SegmentSchemaVersion: SegmentSchemaVersion, EstimatorVersion: EstimatorVersion, CountryField: CountryIssuer,
		Cohort:      Cohort{PaymentMethod: "card", Operation: "sale", CaptureMethod: "automatic", AttemptKind: "first", OutcomeDefinition: SaleOutcome, DenominatorVersion: DenominatorVersion},
		WindowStart: at.Add(-24 * time.Hour), WindowEnd: at.Add(-30 * time.Minute), ObservedAt: at.Add(-20 * time.Minute), ProducedAt: at.Add(-10 * time.Minute),
		MaxAge: time.Hour, MaxWindowAge: 2 * time.Hour, MinSamples: []uint64{25, 50, 100, 100}, MinCoveragePPM: 950_000, Prior: Prior{.85, 20},
		Candidates: []Candidate{{"cand_a", "revision_a"}, {"cand_b", "revision_b"}},
		Buckets: []Bucket{
			{Segment: SegmentKey{"cand_a", "revision_a", "DE", "visa", "EUR"}, Successes: 1, SampleSize: 1},
			{Segment: SegmentKey{"cand_a", "revision_a", "DE", Wildcard, "EUR"}, Successes: 900, SampleSize: 1000},
			{Segment: SegmentKey{"cand_a", "revision_a", Wildcard, Wildcard, "EUR"}, Successes: 995, SampleSize: 1000},
			{Segment: SegmentKey{"cand_a", "revision_a", Wildcard, Wildcard, Wildcard}, Successes: 800, SampleSize: 1000},
			{Segment: SegmentKey{"cand_b", "revision_b", "DE", "visa", "EUR"}, Successes: 900, SampleSize: 1000},
		},
	}
	q := Query{CandidateID: "cand_a", ConnectionRevision: "revision_a", CountryField: CountryIssuer, CountryState: StateKnown, CountryValue: "DE", NetworkState: StateKnown, NetworkValue: "visa", Currency: "EUR", Operation: "sale", CaptureMethod: "automatic"}
	return a, q, at
}

func mustCompilePerformance(t *testing.T, a Artifact) *Snapshot {
	t.Helper()
	s, err := Compile(a)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFixedHierarchyRejectsOneOfOneAndDoesNotChooseBestParent(t *testing.T) {
	a, q, at := performanceFixture()
	got, err := mustCompilePerformance(t, a).Lookup(at, q)
	if err != nil || got.State != StateKnown || got.FallbackLevel != 1 || got.SourceSegment == nil || *got.SourceSegment != a.Buckets[1].Segment || got.Estimate.SampleSize != 1000 || got.Estimate.Successes != 900 {
		t.Fatalf("wrong hierarchy: %+v, %v", got, err)
	}
	if got.Estimate.Probability >= .99 || got.ParentSegment == nil || *got.ParentSegment != a.Buckets[2].Segment {
		t.Fatal("higher-rate sibling/ancestor was selected as winner")
	}
	for _, reason := range []string{"performance_observational_only", "performance_insufficient_sample", "performance_hierarchy_fallback"} {
		if !slices.Contains(got.ReasonCodes, reason) {
			t.Fatal("reason missing", reason)
		}
	}
	if got.Interpretation != ObservationalOnly || got.SnapshotDigest == "" || got.Cohort.OutcomeDefinition != SaleOutcome || got.Estimate.IntervalMethodVersion != IntervalMethodVersion {
		t.Fatal("evidence lost its interpretation or method")
	}
	qB := q
	qB.CandidateID, qB.ConnectionRevision = "cand_b", "revision_b"
	b, err := mustCompilePerformance(t, a).Lookup(at, qB)
	if err != nil || b.FallbackLevel != 0 || b.SourceSegment.CandidateID != "cand_b" {
		t.Fatal("candidate evidence crossed identities", b, err)
	}
}

func TestUnknownGeographyAndNetworkNeverBecomeKnownEmptyKeys(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*Query)
		level  int
		reason string
	}{
		{"unknown network", func(q *Query) { q.NetworkState, q.NetworkValue = StateUnknown, "" }, 1, "performance_network_unknown"},
		{"redacted network", func(q *Query) { q.NetworkState, q.NetworkValue = StateRedacted, "" }, 1, "performance_network_redacted"},
		{"stale network value", func(q *Query) { q.NetworkState = StateStale }, 1, "performance_network_stale"},
		{"unknown issuer", func(q *Query) { q.CountryState, q.CountryValue = StateUnknown, "" }, 2, "performance_country_unknown"},
		{"redacted issuer", func(q *Query) { q.CountryState, q.CountryValue = StateRedacted, "" }, 2, "performance_country_redacted"},
		{"stale issuer value", func(q *Query) { q.CountryState = StateStale }, 2, "performance_country_stale"},
		{"inapplicable issuer", func(q *Query) { q.CountryState, q.CountryValue = StateNotApplicable, "" }, 2, "performance_country_not_applicable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, q, at := performanceFixture()
			tc.edit(&q)
			got, err := mustCompilePerformance(t, a).Lookup(at, q)
			if err != nil || got.State != StateKnown || got.FallbackLevel != tc.level || !slices.Contains(got.ReasonCodes, tc.reason) {
				t.Fatalf("bad context fallback %+v, %v", got, err)
			}
		})
	}
	for name, edit := range map[string]func(*Query){
		"unknown carries value":     func(q *Query) { q.CountryState = StateUnknown },
		"known empty country":       func(q *Query) { q.CountryValue = "" },
		"known wildcard":            func(q *Query) { q.CountryValue = Wildcard },
		"implicit network state":    func(q *Query) { q.NetworkState = "" },
		"redacted carries network":  func(q *Query) { q.NetworkState = StateRedacted },
		"network case variant":      func(q *Query) { q.NetworkValue = "Visa" },
		"unnamed country semantics": func(q *Query) { q.CountryField = "country" },
		"follow-on operation":       func(q *Query) { q.Operation = "refund" },
	} {
		t.Run(name, func(t *testing.T) {
			a, q, at := performanceFixture()
			edit(&q)
			got, err := mustCompilePerformance(t, a).Lookup(at, q)
			if got.Estimate != nil || !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("invalid query accepted %+v,%v", got, err)
			}
		})
	}
}

func TestPerformanceCohortAndRevisionCannotSilentlyMix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*Query)
		reason string
	}{
		{"other geography semantics", func(q *Query) { q.CountryField = CountryMerchant }, "performance_cohort_mismatch"},
		{"different funds phase", func(q *Query) { q.Operation, q.CaptureMethod = "authorize", "manual" }, "performance_cohort_mismatch"},
		{"different revision", func(q *Query) { q.ConnectionRevision = "revision_new" }, "performance_candidate_revision_mismatch"},
		{"unknown candidate", func(q *Query) { q.CandidateID = "cand_other" }, "performance_unknown_candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, q, at := performanceFixture()
			tc.edit(&q)
			got, err := mustCompilePerformance(t, a).Lookup(at, q)
			if err != nil || got.State != StateUnknown || got.Estimate != nil || !slices.Contains(got.ReasonCodes, tc.reason) {
				t.Fatalf("mismatched identity/cohort produced evidence: %+v,%v", got, err)
			}
		})
	}
	a, q, at := performanceFixture()
	a.Cohort.Operation, a.Cohort.CaptureMethod, a.Cohort.OutcomeDefinition = "authorize", "manual", AuthorizationOutcome
	q.Operation, q.CaptureMethod = "authorize", "manual"
	if got, err := mustCompilePerformance(t, a).Lookup(at, q); err != nil || got.State != StateKnown || got.Cohort.OutcomeDefinition != AuthorizationOutcome {
		t.Fatal("explicit authorization cohort rejected", got, err)
	}
}

func TestPendingOutcomesDegradeCoverageWithoutBecomingDeclines(t *testing.T) {
	a, q, at := performanceFixture()
	a.Buckets[0].Successes, a.Buckets[0].SampleSize, a.Buckets[0].PendingCount = 1000, 1000, 1000
	old := mustCompilePerformance(t, a)
	got, err := old.Lookup(at, q)
	if err != nil || got.FallbackLevel != 1 || !slices.Contains(got.ReasonCodes, "performance_incomplete_outcomes") {
		t.Fatal("pending observations became a 100% trustworthy bucket", got, err)
	}
	a.Version = "performance_v2"
	a.Buckets[0].PendingCount = 0
	a.Buckets[0].SampleSize = 2000
	a.Buckets[0].Successes = 1900
	newer, err := mustCompilePerformance(t, a).Lookup(at, q)
	if err != nil || newer.FallbackLevel != 0 || newer.Estimate.SampleSize != 2000 || newer.Estimate.Successes != 1900 || newer.SnapshotDigest == got.SnapshotDigest {
		t.Fatal("new finalized evidence not versioned", newer, err)
	}
	replayed, err := old.Lookup(at, q)
	if err != nil || !reflect.DeepEqual(got, replayed) {
		t.Fatal("late outcome rewrote published history")
	}
	// Coverage equality is accepted, one additional unresolved outcome is not.
	a.Buckets[0].SampleSize, a.Buckets[0].Successes, a.Buckets[0].PendingCount = 950, 900, 50
	exact, err := mustCompilePerformance(t, a).Lookup(at, q)
	if err != nil || exact.FallbackLevel != 0 || exact.Estimate.CoveragePPM != 950_000 {
		t.Fatal("coverage boundary", exact, err)
	}
	a.Buckets[0].PendingCount = 51
	below, err := mustCompilePerformance(t, a).Lookup(at, q)
	if err != nil || below.FallbackLevel != 1 {
		t.Fatal("incomplete coverage accepted", below, err)
	}
}

func TestFreshPublicationCannotHideAnOldWindow(t *testing.T) {
	a, q, at := performanceFixture()
	for _, tc := range []struct {
		name          string
		at            time.Time
		state, reason string
	}{
		{"exact age", a.ProducedAt.Add(a.MaxAge), StateKnown, ""},
		{"one nanosecond stale", a.ProducedAt.Add(a.MaxAge + time.Nanosecond), StateStale, "performance_snapshot_stale"},
		{"future artifact", a.ProducedAt.Add(-time.Nanosecond), StateStale, "performance_snapshot_not_yet_published"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mustCompilePerformance(t, a).Lookup(tc.at, q)
			if err != nil || got.State != tc.state {
				t.Fatal(got, err)
			}
			if tc.reason != "" && (got.Estimate != nil || !slices.Contains(got.ReasonCodes, tc.reason)) {
				t.Fatal("stale evidence exposed")
			}
		})
	}
	a.MaxAge = 4 * time.Hour
	exact, err := mustCompilePerformance(t, a).Lookup(a.WindowEnd.Add(a.MaxWindowAge), q)
	if err != nil || exact.State != StateKnown {
		t.Fatal(exact, err)
	}
	stale, err := mustCompilePerformance(t, a).Lookup(a.WindowEnd.Add(a.MaxWindowAge+time.Nanosecond), q)
	if err != nil || stale.State != StateStale || stale.Estimate != nil {
		t.Fatal("republishing old data refreshed its authority", stale, err)
	}
	if zero, err := mustCompilePerformance(t, a).Lookup(time.Time{}, q); zero.Estimate != nil || !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("zero evaluation time accepted")
	}
	if zero, err := (*Snapshot)(nil).Lookup(at, q); zero.Estimate != nil || !errors.Is(err, ErrInvalidArtifact) {
		t.Fatal("nil snapshot accepted")
	}
}

func TestColdStartPriorIsNotFabricatedCandidateHistory(t *testing.T) {
	a, q, at := performanceFixture()
	a.Buckets = nil
	got, err := mustCompilePerformance(t, a).Lookup(at, q)
	if err != nil || got.State != StatePrior || got.Source != "global_prior" || got.FallbackLevel != 4 || got.SourceSegment != nil || got.Estimate.SampleSize != 0 || got.Estimate.Successes != 0 || got.Estimate.Probability != a.Prior.Probability || got.Estimate.ConservativePPM != 0 || got.Estimate.Confidence != "prior_only" || !slices.Contains(got.ReasonCodes, "performance_cold_start") {
		t.Fatalf("cold-start semantics missing: %+v,%v", got, err)
	}
}

func TestPerformanceCompileRejectsAmbiguousStatisticsAndContracts(t *testing.T) {
	for name, edit := range map[string]func(*Artifact){
		"schema":                          func(a *Artifact) { a.SchemaVersion = "performance.v2" },
		"hierarchy schema":                func(a *Artifact) { a.SegmentSchemaVersion = "choose_best_parent" },
		"estimator":                       func(a *Artifact) { a.EstimatorVersion = "arbitrary" },
		"version":                         func(a *Artifact) { a.Version = "" },
		"country semantics":               func(a *Artifact) { a.CountryField = "country" },
		"mixed phase":                     func(a *Artifact) { a.Cohort.OutcomeDefinition = AuthorizationOutcome },
		"denominator":                     func(a *Artifact) { a.Cohort.DenominatorVersion = "http_requests" },
		"fallback cohort":                 func(a *Artifact) { a.Cohort.AttemptKind = "any" },
		"follow-on":                       func(a *Artifact) { a.Cohort.Operation = "capture" },
		"duplicate candidate":             func(a *Artifact) { a.Candidates = append(a.Candidates, a.Candidates[0]) },
		"unknown revision":                func(a *Artifact) { a.Buckets[0].Segment.ConnectionRevision = "other" },
		"duplicate bucket":                func(a *Artifact) { a.Buckets = append(a.Buckets, a.Buckets[0]) },
		"sibling hierarchy":               func(a *Artifact) { a.Buckets[0].Segment.CountryValue = Wildcard },
		"currency wildcard at fine level": func(a *Artifact) { a.Buckets[0].Segment.Currency = Wildcard },
		"bad network":                     func(a *Artifact) { a.Buckets[0].Segment.NetworkValue = "all_cards" },
		"success exceeds sample":          func(a *Artifact) { a.Buckets[0].Successes = 2 },
		"inexact sample":                  func(a *Artifact) { a.Buckets[0].SampleSize = maxExactSamples + 1 },
		"inexact pending":                 func(a *Artifact) { a.Buckets[0].PendingCount = maxExactSamples + 1 },
		"NaN prior":                       func(a *Artifact) { a.Prior.Probability = math.NaN() },
		"infinite strength":               func(a *Artifact) { a.Prior.Strength = math.Inf(1) },
		"no strength":                     func(a *Artifact) { a.Prior.Strength = 0 },
		"missing sample threshold":        func(a *Artifact) { a.MinSamples = a.MinSamples[:3] },
		"extra sample threshold":          func(a *Artifact) { a.MinSamples = append(a.MinSamples, 1) },
		"zero threshold":                  func(a *Artifact) { a.MinSamples[0] = 0 },
		"no coverage policy":              func(a *Artifact) { a.MinCoveragePPM = 0 },
		"coverage beyond one":             func(a *Artifact) { a.MinCoveragePPM = 1_000_001 },
		"no age":                          func(a *Artifact) { a.MaxAge = 0 },
		"no data-age bound":               func(a *Artifact) { a.MaxWindowAge = 0 },
		"future watermark":                func(a *Artifact) { a.ObservedAt = a.ProducedAt.Add(time.Nanosecond) },
		"incomplete window":               func(a *Artifact) { a.ObservedAt = a.WindowEnd.Add(-time.Nanosecond) },
		"empty window":                    func(a *Artifact) { a.WindowStart = a.WindowEnd },
		"non UTC":                         func(a *Artifact) { a.ProducedAt = a.ProducedAt.In(time.FixedZone("offset", 3600)) },
	} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := performanceFixture()
			edit(&a)
			if got, err := Compile(a); got != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("bad snapshot accepted: %v,%v", got, err)
			}
		})
	}
}

func TestPerformanceSnapshotCanonicalOwnershipAndConcurrentReplay(t *testing.T) {
	a, q, at := performanceFixture()
	before, _ := json.Marshal(a)
	s := mustCompilePerformance(t, a)
	baseline, err := s.Lookup(at, q)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(a)
	if string(before) != string(after) {
		t.Fatal("compile mutated source")
	}
	random := rand.New(rand.NewSource(12))
	for i := 0; i < 30; i++ {
		random.Shuffle(len(a.Candidates), func(i, j int) { a.Candidates[i], a.Candidates[j] = a.Candidates[j], a.Candidates[i] })
		random.Shuffle(len(a.Buckets), func(i, j int) { a.Buckets[i], a.Buckets[j] = a.Buckets[j], a.Buckets[i] })
		other := mustCompilePerformance(t, a)
		got, err := other.Lookup(at, q)
		if err != nil || s.Digest() != other.Digest() || !reflect.DeepEqual(got, baseline) {
			t.Fatal("artifact order changed evidence")
		}
	}
	a.MinSamples[0] = 1
	a.Buckets[0].Successes = 0
	a.Candidates[0].ConnectionRevision = "mutated"
	returned := s.Candidates()
	returned[0].ID = "mutated_return"
	changed, _ := s.Lookup(at, q)
	changed.Estimate.Probability = 1
	changed.SourceSegment.CandidateID = "mutated"
	changed.ReasonCodes[0] = "mutated"
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 20; j++ {
				got, err := s.Lookup(at, q)
				if err != nil || !reflect.DeepEqual(got, baseline) {
					t.Error("snapshot changed under concurrent read")
				}
			}
		}()
	}
	group.Wait()
}
