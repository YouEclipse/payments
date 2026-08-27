package cost

import (
	"math"
	"math/big"
)

func validRounding(mode RoundingMode) bool {
	return mode == RoundDown || mode == RoundHalfUp || mode == RoundUp
}

func checkedAdd(left, right int64) (int64, error) {
	if left < 0 || right < 0 || right > math.MaxInt64-left {
		return 0, ErrCostOverflow
	}
	return left + right, nil
}

// The intermediate product has at most 126 bits. Using an exact bounded
// integer avoids rejecting a representable quotient merely because value*num
// would overflow int64, and prevents wrapping before the division.
func mulDiv(value, numerator, denominator int64, mode RoundingMode) (int64, error) {
	if value < 0 || numerator < 0 || denominator <= 0 || !validRounding(mode) {
		return 0, ErrInvalidQuery
	}
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(numerator))
	return roundedQuotient(product, denominator, mode)
}

func percentageMicros(amountMinor int64, exponent uint8, bps int64, mode RoundingMode) (int64, error) {
	if amountMinor <= 0 || exponent > 6 || bps < 0 || !validRounding(mode) {
		return 0, ErrInvalidQuery
	}
	factor := int64(1)
	for i := exponent; i < 6; i++ {
		factor *= 10
	}
	product := new(big.Int).Mul(big.NewInt(amountMinor), big.NewInt(factor))
	product.Mul(product, big.NewInt(bps))
	return roundedQuotient(product, 10_000, mode)
}

func roundedQuotient(product *big.Int, denominator int64, mode RoundingMode) (int64, error) {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(product, big.NewInt(denominator), remainder)
	round := mode == RoundUp && remainder.Sign() != 0
	if mode == RoundHalfUp {
		// ceil(denominator/2), without denominator+1 overflowing.
		round = remainder.Cmp(big.NewInt(denominator/2+denominator%2)) >= 0
	}
	if round {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Sign() < 0 {
		return 0, ErrCostOverflow
	}
	return quotient.Int64(), nil
}
