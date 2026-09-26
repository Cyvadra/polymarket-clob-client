package decimal

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const scale = 18

func Rat(value string) (*big.Rat, bool) {
	return new(big.Rat).SetString(strings.TrimSpace(value))
}

func Positive(value string) bool {
	parsed, ok := Rat(value)
	return ok && parsed.Sign() > 0
}

func NonNegative(value string) bool {
	parsed, ok := Rat(value)
	return ok && parsed.Sign() >= 0
}

// Compare orders two decimal strings numerically. Unparsable values sort
// before parsable ones so callers get a deterministic result.
func Compare(left, right string) int {
	leftRat, leftOK := Rat(left)
	rightRat, rightOK := Rat(right)
	switch {
	case leftOK && rightOK:
		return leftRat.Cmp(rightRat)
	case leftOK:
		return 1
	case rightOK:
		return -1
	default:
		return 0
	}
}

func Price(value string) (float64, error) {
	price, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || price <= 0 || price >= 1 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("invalid price %q", value)
	}
	return price, nil
}

func PositiveFloat(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || parsed <= 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("invalid positive decimal %q", value)
	}
	return parsed, nil
}

func NonNegativeFloat(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("invalid non-negative decimal %q", value)
	}
	return parsed, nil
}

// FloorTo floors a positive decimal string to the given number of decimal
// places. Exchange amount encoding truncates beyond a fixed precision, so
// callers floor first and submit exactly what will be encoded.
func FloorTo(value string, digits int) (string, bool) {
	parsed, ok := Rat(value)
	if !ok || digits < 0 {
		return "", false
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	scaled := new(big.Rat).Mul(parsed, new(big.Rat).SetInt(scale))
	units := new(big.Int).Quo(scaled.Num(), scaled.Denom())
	if units.Sign() <= 0 {
		return "", false
	}
	return new(big.Rat).SetFrac(units, scale).FloatString(digits), true
}

// FloorSharesFloat floors a decimal share string to the exchange share
// precision and returns it as a float64. Flooring first keeps the float64
// short enough to round-trip through the order amount encoding exactly.
func FloorSharesFloat(value string, digits int) (float64, error) {
	floored, ok := FloorTo(value, digits)
	if !ok {
		return 0, fmt.Errorf("invalid share amount %q", value)
	}
	return PositiveFloat(floored)
}

// FloorToTick and CeilToTick snap a price onto the market tick grid. The
// signer rejects any price that is not an exact tick multiple.
func FloorToTick(price, tick float64) float64 {
	return snapToTick(price, tick, math.Floor)
}

func CeilToTick(price, tick float64) float64 {
	return snapToTick(price, tick, math.Ceil)
}

func RoundToTick(price, tick float64) float64 {
	return snapToTick(price, tick, math.Round)
}

func snapToTick(price, tick float64, round func(float64) float64) float64 {
	if tick <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return price
	}
	ticks := price / tick
	// A price already on the grid must not be pushed a whole tick away by
	// binary representation error.
	if math.Abs(ticks-math.Round(ticks)) < tickEpsilon {
		ticks = math.Round(ticks)
	} else {
		ticks = round(ticks)
	}
	return math.Round(ticks*tick*1e9) / 1e9
}

const tickEpsilon = 1e-9

// AddString sums two decimal strings exactly, without going through float64.
func AddString(left, right string) (string, bool) {
	a, ok := Rat(left)
	if !ok {
		return "", false
	}
	b, ok := Rat(right)
	if !ok {
		return "", false
	}
	return new(big.Rat).Add(a, b).FloatString(sumPrecision), true
}

// SubString subtracts right from left exactly. A negative result is returned as
// such; callers decide whether that is meaningful.
func SubString(left, right string) (string, bool) {
	a, ok := Rat(left)
	if !ok {
		return "", false
	}
	b, ok := Rat(right)
	if !ok {
		return "", false
	}
	return new(big.Rat).Sub(a, b).FloatString(sumPrecision), true
}

// sumPrecision is the decimal precision sums and differences are rendered at.
// Share counts and prices both carry at most six places on this exchange, so
// this is exact for every figure these helpers see.
const sumPrecision = 6

func MulString(left, right string) (string, bool) {
	leftRat, leftOK := Rat(left)
	rightRat, rightOK := Rat(right)
	if !leftOK || !rightOK {
		return "", false
	}
	return new(big.Rat).Mul(leftRat, rightRat).FloatString(scale), true
}

func DivideAndRoundDown(numerator, denominator string, digits int) (string, bool) {
	left, leftOK := Rat(numerator)
	right, rightOK := Rat(denominator)
	if !leftOK || !rightOK || right.Sign() <= 0 {
		return "", false
	}
	return FloorTo(new(big.Rat).Quo(left, right).FloatString(scale), digits)
}

func FormatPrice(value float64) string {
	return strconv.FormatFloat(RoundUSDC(value), 'f', -1, 64)
}

// RoundUSDC rounds value to USDC's 6-decimal precision.
func RoundUSDC(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}

// baseUnitScale is the scale of a balance the CLOB reports: USDC's six
// decimals, which outcome tokens share.
const baseUnitScale = 1_000_000

// FromBaseUnits converts a balance the CLOB reports in 6-decimal base units
// to dollars or shares. A negative or unparsable balance is an error.
func FromBaseUnits(balance string) (float64, error) {
	units, ok := Rat(balance)
	if !ok || units.Sign() < 0 {
		return 0, fmt.Errorf("invalid balance %q", balance)
	}
	value, _ := new(big.Rat).Quo(units, big.NewRat(baseUnitScale, 1)).Float64()
	return value, nil
}
