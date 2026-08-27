package scoring

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
	"testing/quick"

	"github.com/YouEclipse/payments/services/router/internal/performance"
)

func TestNormalizationGoldenRoundingClampingAndExtremeRange(t *testing.T) {
	profile, _, _, _ := scoringFixture()
	for _, tc := range []struct {
		name                                                 string
		cost, ppm, clamped, successPoints, costPoints, total int64
	}{
		{"zero", 0, 0, 100_000, 0, 10000, 3000},
		{"before half", 100_000, 49, 100_000, 0, 10000, 3000},
		{"exact half", 100_000, 50, 100_000, 1, 10000, 3000},
		{"half retained", 100_000, 150, 100_000, 2, 10000, 3001},
		{"point floor", 100_001, 1_000_000, 100_001, 10000, 9999, 9999},
		{"exact ceiling", 1_100_000, 1_000_000, 1_100_000, 10000, 0, 7000},
		{"beyond ceiling", math.MaxInt64, 1_000_000, 1_100_000, 10000, 0, 7000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize(profile, tc.cost, tc.ppm)
			if got.SucceededCostMicros != tc.cost || got.ConservativePPM != tc.ppm || got.ClampedCostMicros != tc.clamped || got.SuccessPoints != tc.successPoints || got.CostPoints != tc.costPoints || got.TotalPoints != tc.total {
				t.Fatalf("rounding or raw value changed: %+v", got)
			}
		})
	}
	profile.CostFloorMicros, profile.CostCeilingMicros = 0, math.MaxInt64
	for _, tc := range []struct{ cost, points int64 }{{0, 10000}, {math.MaxInt64 / 2, 5000}, {math.MaxInt64 - 1, 0}, {math.MaxInt64, 0}} {
		if got := normalize(profile, tc.cost, 500_000); got.CostPoints != tc.points {
			t.Fatal("wide normalization product overflowed", got)
		}
	}
	profile.CostFloorMicros, profile.CostCeilingMicros = math.MaxInt64-1, math.MaxInt64
	if got := normalize(profile, math.MaxInt64-1, 999_999); got.CostPoints != PointScale || got.SuccessPoints != PointScale || got.TotalPoints != PointScale {
		t.Fatal("one-micro interval near int64 maximum lost precision", got)
	}
}

func normalizationProperty(a, b, amountA, amountB, ppmBitsA, ppmBitsB uint64, weight uint16) bool {
	floor, ceiling := int64(a&math.MaxInt64), int64(b&math.MaxInt64)
	if floor > ceiling {
		floor, ceiling = ceiling, floor
	}
	if floor == ceiling {
		if ceiling == math.MaxInt64 {
			floor--
		} else {
			ceiling++
		}
	}
	costA, costB := int64(amountA&math.MaxInt64), int64(amountB&math.MaxInt64)
	if costA > costB {
		costA, costB = costB, costA
	}
	ppmA, ppmB := int64(ppmBitsA%1_000_001), int64(ppmBitsB%1_000_001)
	if ppmA > ppmB {
		ppmA, ppmB = ppmB, ppmA
	}
	profile := Profile{QuoteCurrency: "USD", CostFloorMicros: floor, CostCeilingMicros: ceiling, SuccessWeightBPS: int64(weight) % (PointScale + 1)}
	profile.CostWeightBPS = PointScale - profile.SuccessWeightBPS
	got := normalize(profile, costA, ppmA)
	moreExpensive, moreSuccessful := normalize(profile, costB, ppmA), normalize(profile, costA, ppmB)
	// Independent exact rational arithmetic is the normalization oracle.
	clamped := min(max(costA, floor), ceiling)
	ratio := new(big.Rat).SetFrac(big.NewInt(ceiling-clamped), big.NewInt(ceiling-floor))
	ratio.Mul(ratio, new(big.Rat).SetInt64(PointScale))
	wantCost := new(big.Int).Quo(ratio.Num(), ratio.Denom()).Int64()
	return got.SucceededCostMicros == costA && got.ConservativePPM == ppmA && got.ClampedCostMicros == clamped && got.CostPoints == wantCost &&
		got.CostPoints >= 0 && got.CostPoints <= PointScale && got.SuccessPoints >= 0 && got.SuccessPoints <= PointScale &&
		got.TotalPoints >= 0 && got.TotalPoints <= PointScale && got.TotalPoints == got.SuccessContribution+got.CostContribution &&
		got.TotalPoints >= moreExpensive.TotalPoints && got.CostPoints >= moreExpensive.CostPoints &&
		got.TotalPoints <= moreSuccessful.TotalPoints && got.SuccessPoints <= moreSuccessful.SuccessPoints &&
		got.SuccessPoints == (ppmA*PointScale+performance.ProbabilityScale/2)/performance.ProbabilityScale
}

func TestNormalizationProperties(t *testing.T) {
	if err := quick.Check(normalizationProperty, &quick.Config{MaxCount: 20_000, Rand: rand.New(rand.NewSource(9323))}); err != nil {
		t.Fatal(err)
	}
}

func FuzzNormalization(f *testing.F) {
	f.Add(uint64(0), uint64(math.MaxInt64), uint64(0), uint64(math.MaxInt64), uint64(0), uint64(1_000_000), uint16(7000))
	f.Add(uint64(math.MaxInt64-1), uint64(math.MaxInt64), uint64(math.MaxInt64-1), uint64(math.MaxInt64), uint64(49), uint64(50), uint16(10000))
	f.Add(uint64(5), uint64(5), uint64(5), uint64(0), uint64(150), uint64(1_000_000), uint16(0))
	f.Fuzz(func(t *testing.T, a, b, amountA, amountB, ppmA, ppmB uint64, weight uint16) {
		if !normalizationProperty(a, b, amountA, amountB, ppmA, ppmB, weight) {
			t.Fatal("normalization violated an exact bound or monotonicity")
		}
	})
}
