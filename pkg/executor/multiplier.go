package executor

import (
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
)

// SetSizeMultipliers scales each open's size by the multiplier of the
// strategy prefix it starts with: target_usd for dollar opens, the fraction
// for equity-sized ones. Prefixes do not overlap, so at most one applies;
// a strategy with none opens at the size it asked for.
func (e *Executor) SetSizeMultipliers(multipliers map[string]string) {
	e.multipliers = multipliers
}

// sizeMultiplier returns the multiplier for strategy, or "" if none applies.
func (e *Executor) sizeMultiplier(strategy string) string {
	for prefix, multiplier := range e.multipliers {
		if strategy != "" && strings.HasPrefix(strategy, prefix) {
			return multiplier
		}
	}
	return ""
}

// scaleSize multiplies value, floored to digits places so a scaled entry
// never exceeds its share. A value that is not a decimal is returned as it
// is, for validation to reject; one that floors to nothing becomes "0".
func scaleSize(value, multiplier string, digits int) string {
	if multiplier == "" || value == "" {
		return value
	}
	product, ok := decimal.MulString(value, multiplier)
	if !ok {
		return value
	}
	floored, ok := decimal.FloorTo(product, digits)
	if !ok {
		return "0"
	}
	if strings.Contains(floored, ".") {
		floored = strings.TrimRight(strings.TrimRight(floored, "0"), ".")
	}
	return floored
}

// USD targets are floored to USDC precision; fractions keep more places, as
// the sizer multiplies them by the whole equity.
const (
	usdDigits      = 6
	fractionDigits = 12
)
