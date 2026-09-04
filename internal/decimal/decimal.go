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

func OptionalFloat(value string, fallback float64) float64 {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fallback
	}
	return parsed
}

func MulString(left, right string) (string, bool) {
	leftRat, leftOK := Rat(left)
	rightRat, rightOK := Rat(right)
	if !leftOK || !rightOK {
		return "", false
	}
	return new(big.Rat).Mul(leftRat, rightRat).FloatString(scale), true
}

func FormatPrice(value float64) string {
	return strconv.FormatFloat(math.Round(value*1e6)/1e6, 'f', -1, 64)
}
