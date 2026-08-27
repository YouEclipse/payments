package performance

import (
	"errors"
	"math"
	"math/rand"
	"testing"
	"testing/quick"
)

func TestShrinkageDoesNotTurnOneSuccessIntoCertainSuperiority(t *testing.T) {
	small, err := estimate(1, 1, 0.9, 20)
	if err != nil {
		t.Fatal(err)
	}
	stable, err := estimate(900, 1000, 0.9, 20)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(small.Probability-19.0/21.0) > 1e-14 || math.Abs(stable.Probability-0.9) > 1e-14 {
		t.Fatal("incorrect transparent shrinkage")
	}
	if small.Probability <= stable.Probability || small.ConservativePPM >= stable.ConservativePPM {
		t.Fatal("conservative evidence did not protect against the tempting 1/1 point estimate")
	}
	if small.ObservedUpper95-small.ObservedLower95 <= stable.ObservedUpper95-stable.ObservedLower95 {
		t.Fatal("small sample has unjustified precision")
	}
	if small.IntervalMethodVersion != IntervalMethodVersion || small.EstimatorVersion != EstimatorVersion {
		t.Fatal("statistical method provenance missing")
	}
}

func TestObservedWilsonIntervalIsNotAShrinkagePosterior(t *testing.T) {
	for _, tc := range []struct {
		s, n         uint64
		lower, upper float64
	}{
		{0, 0, 0, 1}, {0, 1, 0, 0.7934506856227626}, {1, 1, 0.20654931437723745, 1},
		{50, 100, 0.4038315303659957, 0.5961684696340044},
	} {
		lower, upper := wilson95(tc.s, tc.n)
		if math.Abs(lower-tc.lower) > 1e-13 || math.Abs(upper-tc.upper) > 1e-13 {
			t.Fatalf("%d/%d interval=(%.16f,%.16f)", tc.s, tc.n, lower, upper)
		}
	}
	value, err := estimate(100, 100, 0.1, 1000)
	if err != nil || value.Probability >= value.ObservedLower95 || value.ConservativeProbability != value.Probability {
		t.Fatal("raw interval was incorrectly moved to surround the shrunk point", value, err)
	}
	prior, err := estimate(0, 0, 0.85, 20)
	if err != nil || prior.SampleSize != 0 || prior.Probability != 0.85 || prior.ConservativePPM != 0 || prior.ObservedLower95 != 0 || prior.ObservedUpper95 != 1 {
		t.Fatal("prior claimed observed successes", prior, err)
	}
}

func TestStatisticsRejectNonFiniteAndInconsistentEvidence(t *testing.T) {
	for _, tc := range []struct {
		s, n uint64
		p, k float64
	}{
		{2, 1, .5, 1}, {0, maxExactSamples + 1, .5, 1}, {0, 0, .5, 0},
		{1, 2, -.1, 1}, {1, 2, 1.1, 1}, {1, 2, math.NaN(), 1}, {1, 2, math.Inf(1), 1},
		{1, 2, .5, -1}, {1, 2, .5, math.NaN()}, {1, 2, .5, math.Inf(1)}, {1, 2, .5, float64(maxExactSamples) * 2},
	} {
		if got, err := estimate(tc.s, tc.n, tc.p, tc.k); got != (Estimate{}) || !errors.Is(err, ErrInvalidEvidence) {
			t.Fatalf("invalid evidence produced %+v, %v", got, err)
		}
	}
	for _, tc := range []struct {
		n, pending uint64
		want       uint32
	}{
		{0, 0, 0}, {0, 10, 0}, {95, 5, 950_000}, {94, 6, 940_000}, {1, 2, 333_333},
		{maxExactSamples, maxExactSamples, 500_000}, {maxExactSamples, 0, 1_000_000},
	} {
		if got := coveragePPM(tc.n, tc.pending); got != tc.want {
			t.Fatalf("coverage for %+v = %d", tc, got)
		}
	}
}

func statisticsProperty(rawSuccesses, rawSamples, rawPrior, rawStrength uint64) bool {
	n := rawSamples % (maxExactSamples + 1)
	s := rawSuccesses % (n + 1)
	p := float64(rawPrior%1_000_001) / 1_000_000
	k := float64(rawStrength%1_000_000) + 1
	v, err := estimate(s, n, p, k)
	if err != nil {
		return false
	}
	for _, x := range []float64{v.Probability, v.ConservativeProbability, v.ObservedLower95, v.ObservedUpper95} {
		if !finite(x) || x < 0 || x > 1 {
			return false
		}
	}
	if v.ConservativeProbability > v.Probability || v.ConservativeProbability > v.ObservedLower95 || v.ObservedLower95 > v.ObservedUpper95 {
		return false
	}
	if v.ProbabilityPPM < 0 || v.ProbabilityPPM > ProbabilityScale || v.ConservativePPM < 0 || v.ConservativePPM > v.ProbabilityPPM {
		return false
	}
	if s < n {
		higher, err := estimate(s+1, n, p, k)
		if err != nil || higher.Probability < v.Probability || higher.ObservedLower95+1e-14 < v.ObservedLower95 {
			return false
		}
	}
	return true
}

func TestStatisticalProbabilityAndConfidenceProperties(t *testing.T) {
	if err := quick.Check(statisticsProperty, &quick.Config{MaxCount: 10000, Rand: rand.New(rand.NewSource(8))}); err != nil {
		t.Fatal(err)
	}
}

func FuzzStatisticalEvidenceRemainsBounded(f *testing.F) {
	f.Add(uint64(1), uint64(1), uint64(900000), uint64(20))
	f.Add(uint64(900), uint64(1000), uint64(850000), uint64(20))
	f.Add(maxExactSamples, maxExactSamples, uint64(1000000), maxExactSamples)
	f.Fuzz(func(t *testing.T, s, n, p, k uint64) {
		if !statisticsProperty(s, n, p, k) {
			t.Fatal("statistics violated probability/monotonicity bounds")
		}
	})
}
