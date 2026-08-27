package cost

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"testing"
	"testing/quick"
)

func TestFixedPointRoundingAndRepresentableLargeQuotients(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		value, numerator, denominator int64
		mode                          RoundingMode
		want                          int64
		wantErr                       error
	}{
		{"floor", 1, 1, 2, RoundDown, 0, nil},
		{"half up", 1, 1, 2, RoundHalfUp, 1, nil},
		{"ceiling", 1, 1, 3, RoundUp, 1, nil},
		{"below half", 1, 1, 3, RoundHalfUp, 0, nil},
		{"above half", 2, 1, 3, RoundHalfUp, 1, nil},
		{"zero fee", math.MaxInt64, 0, 1, RoundUp, 0, nil},
		{"large product small quotient", math.MaxInt64, math.MaxInt64, math.MaxInt64, RoundHalfUp, math.MaxInt64, nil},
		{"denominator maximum no threshold overflow", math.MaxInt64 / 2, 1, math.MaxInt64, RoundHalfUp, 0, nil},
		{"denominator maximum above half", math.MaxInt64/2 + 1, 1, math.MaxInt64, RoundHalfUp, 1, nil},
		{"final result overflow", math.MaxInt64, 2, 1, RoundDown, 0, ErrCostOverflow},
		{"negative value", -1, 1, 1, RoundDown, 0, ErrInvalidQuery},
		{"negative numerator", 1, -1, 1, RoundDown, 0, ErrInvalidQuery},
		{"zero denominator", 1, 1, 0, RoundDown, 0, ErrInvalidQuery},
		{"undeclared rounding", 0, 0, 1, "bankers", 0, ErrInvalidQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mulDiv(tc.value, tc.numerator, tc.denominator, tc.mode)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %d, %v; want %d, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestMinorUnitExponentAndFeeBounds(t *testing.T) {
	for _, tc := range []struct {
		minor     int64
		exponent  uint8
		bps, want int64
	}{
		{1000, 2, 260, 260_000},    // 10 USD at 2.6% = 0.26 USD.
		{1000, 0, 260, 26_000_000}, // 1000 JPY, not 10 JPY.
		{1000, 3, 260, 26_000},
		{math.MaxInt64, 2, 1, math.MaxInt64}, // Principal micros need not fit int64.
		{1, 6, 0, 0},
	} {
		got, err := percentageMicros(tc.minor, tc.exponent, tc.bps, RoundDown)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %d %v", tc, got, err)
		}
	}
	for _, tc := range []struct {
		minor    int64
		exponent uint8
		bps      int64
		mode     RoundingMode
		want     error
	}{
		{0, 2, 1, RoundDown, ErrInvalidQuery}, {-1, 2, 1, RoundDown, ErrInvalidQuery},
		{1, 7, 1, RoundDown, ErrInvalidQuery}, {1, 2, -1, RoundDown, ErrInvalidQuery},
		{1, 2, 1, "", ErrInvalidQuery}, {math.MaxInt64, 0, 1, RoundDown, ErrCostOverflow},
	} {
		if got, err := percentageMicros(tc.minor, tc.exponent, tc.bps, tc.mode); got != 0 || !errors.Is(err, tc.want) {
			t.Fatalf("invalid fee returned %d, %v", got, err)
		}
	}
	if got, err := checkedAdd(math.MaxInt64, 0); err != nil || got != math.MaxInt64 {
		t.Fatal(got, err)
	}
	for _, pair := range [][2]int64{{math.MaxInt64, 1}, {-1, 1}, {1, -1}} {
		if got, err := checkedAdd(pair[0], pair[1]); got != 0 || !errors.Is(err, ErrCostOverflow) {
			t.Fatal(got, err)
		}
	}
}

func TestMulDivAgainstExactRationalProperties(t *testing.T) {
	property := func(a, b, c uint64) bool {
		value, numerator, denominator := int64(a&math.MaxInt64), int64(b&math.MaxInt64), int64(c&math.MaxInt64)
		if denominator == 0 {
			denominator = 1
		}
		return checkMulDivProperty(value, numerator, denominator)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 10000, Rand: rand.New(rand.NewSource(7))}); err != nil {
		t.Fatal(err)
	}
}

func checkMulDivProperty(value, numerator, denominator int64) bool {
	exact := new(big.Rat).Mul(new(big.Rat).SetInt64(value), new(big.Rat).SetFrac64(numerator, denominator))
	for _, mode := range []RoundingMode{RoundDown, RoundHalfUp, RoundUp} {
		// Round the rational using a shifted floor, independently from the
		// production quotient/remainder implementation.
		shifted := new(big.Rat).Set(exact)
		if mode == RoundHalfUp {
			shifted.Add(shifted, big.NewRat(1, 2))
		}
		want := new(big.Int).Quo(shifted.Num(), shifted.Denom())
		if mode == RoundUp && !exact.IsInt() {
			want.Add(want, big.NewInt(1))
		}
		got, err := mulDiv(value, numerator, denominator, mode)
		if !want.IsInt64() {
			if !errors.Is(err, ErrCostOverflow) || got != 0 {
				return false
			}
		} else if err != nil || got != want.Int64() {
			return false
		}
	}
	return true
}

func FuzzFixedPointDoesNotWrap(f *testing.F) {
	f.Add(uint64(math.MaxInt64), uint64(math.MaxInt64), uint64(math.MaxInt64))
	f.Add(uint64(1), uint64(1), uint64(2))
	f.Add(uint64(1000), uint64(260), uint64(10000))
	f.Fuzz(func(t *testing.T, a, b, c uint64) {
		denominator := int64(c & math.MaxInt64)
		if denominator == 0 {
			denominator = 1
		}
		if !checkMulDivProperty(int64(a&math.MaxInt64), int64(b&math.MaxInt64), denominator) {
			t.Fatal("fixed-point arithmetic disagrees with rational bounds")
		}
	})
}
