package performance

import (
	"math"
	"math/big"
)

// Exact integer counts are bounded before conversion to binary64. This is a
// schema limit, not a claim that extremely large payment cohorts are IID.
const maxExactSamples = uint64(1 << 53)

func estimate(successes, samples uint64, parentProbability, strength float64) (Estimate, error) {
	if successes > samples || samples > maxExactSamples || !finite(parentProbability) ||
		parentProbability < 0 || parentProbability > 1 || !finite(strength) || strength < 0 || strength > float64(maxExactSamples) {
		return Estimate{}, ErrInvalidEvidence
	}
	n := float64(samples)
	if n+strength == 0 {
		return Estimate{}, ErrInvalidEvidence
	}
	probability := (float64(successes) + strength*parentProbability) / (n + strength)
	lower, upper := wilson95(successes, samples)
	conservative := math.Min(probability, lower)
	return Estimate{
		Probability: probability, ProbabilityPPM: probabilityPPM(probability),
		ConservativeProbability: conservative, ConservativePPM: probabilityPPM(conservative),
		ObservedLower95: lower, ObservedUpper95: upper,
		Successes: successes, SampleSize: samples,
		ParentProbability: parentProbability, PriorStrength: strength,
		EstimatorVersion: EstimatorVersion, IntervalMethodVersion: IntervalMethodVersion,
	}, nil
}

// Wilson's score interval for the OBSERVED binomial proportion (z=1.95996...).
// It intentionally does not borrow the prior's pseudo-counts or claim that
// historical routing removed confounding or correlated outcomes.
func wilson95(successes, samples uint64) (float64, float64) {
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

func probabilityPPM(value float64) int64 {
	return int64(math.Floor(value * float64(ProbabilityScale)))
}

func coveragePPM(samples, pending uint64) uint32 {
	if samples == 0 {
		return 0
	}
	// Both counts are bounded by 2^53, so their sum cannot overflow uint64.
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(samples), big.NewInt(ProbabilityScale))
	numerator.Quo(numerator, new(big.Int).SetUint64(samples+pending))
	return uint32(numerator.Uint64())
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
