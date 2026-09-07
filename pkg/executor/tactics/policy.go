package tactics

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// policyPrices is the parsed, validated form of the price-related policy
// fields. Parsing happens once per intent and is shared by intent validation
// and planning so the two can never disagree about what a policy means.
type policyPrices struct {
	style       protocol.ExecutionStyle
	limit       float64
	signal      float64
	maxPrice    float64
	hasMaxPrice bool
	minPrice    float64
	hasMinPrice bool
	priceStep   float64
	quoteOffset float64
}

// ValidatePolicy reports whether an intent's execution policy can be planned.
func ValidatePolicy(intent protocol.ExecutionIntent) error {
	_, err := parsePolicyPrices(intent)
	return err
}

func parsePolicyPrices(intent protocol.ExecutionIntent) (policyPrices, error) {
	policy := intent.Policy
	parsed := policyPrices{style: policy.Style}
	limit, err := decimal.Price(intent.LimitPrice)
	if err != nil {
		return policyPrices{}, fmt.Errorf("invalid limit price %q", intent.LimitPrice)
	}
	parsed.limit = limit

	switch policy.Style {
	case protocol.ExecutionStyleLimit:
		parsed.signal = limit
		return parsed, nil
	case protocol.ExecutionStyleMakerPostOnly, protocol.ExecutionStyleTakerAggressive:
	case "":
		return policyPrices{}, unsupportedStyleError{reason: "execution policy style is required"}
	default:
		return policyPrices{}, unsupportedStyleError{reason: fmt.Sprintf("unsupported execution style %q", policy.Style)}
	}

	parsed.signal, err = optionalPrice("mid_price", policy.MidPrice, limit)
	if err != nil {
		return policyPrices{}, err
	}
	if strings.TrimSpace(policy.MidPrice) == "" {
		if parsed.signal, err = optionalPrice("initial_price", policy.InitialPrice, limit); err != nil {
			return policyPrices{}, err
		}
	}
	if parsed.maxPrice, parsed.hasMaxPrice, err = optionalBound("max_price", policy.MaxPrice); err != nil {
		return policyPrices{}, err
	}
	if parsed.minPrice, parsed.hasMinPrice, err = optionalBound("min_price", policy.MinPrice); err != nil {
		return policyPrices{}, err
	}
	if parsed.priceStep, err = optionalFraction("price_step", policy.PriceStep, true); err != nil {
		return policyPrices{}, err
	}
	if parsed.quoteOffset, err = optionalFraction("quote_offset", policy.QuoteOffset, false); err != nil {
		return policyPrices{}, err
	}

	if intent.Side == protocol.SideBuy {
		if !parsed.hasMaxPrice {
			return policyPrices{}, fmt.Errorf("buy execution policy requires max_price")
		}
		if parsed.signal > parsed.maxPrice {
			return policyPrices{}, fmt.Errorf("buy execution policy signal price exceeds max_price")
		}
	}
	if intent.Side == protocol.SideSell {
		if !parsed.hasMinPrice {
			return policyPrices{}, fmt.Errorf("sell execution policy requires min_price")
		}
		if parsed.signal < parsed.minPrice {
			return policyPrices{}, fmt.Errorf("sell execution policy signal price is below min_price")
		}
	}
	return parsed, nil
}

// unsupportedStyleError distinguishes an unknown or missing style from other
// policy problems so the executor can map it onto its own reason code.
type unsupportedStyleError struct{ reason string }

func (e unsupportedStyleError) Error() string { return e.reason }

// UnsupportedStyle reports whether err was caused by a missing or unknown
// execution style.
func UnsupportedStyle(err error) bool {
	var target unsupportedStyleError
	return errors.As(err, &target)
}

func optionalPrice(name, value string, fallback float64) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	price, err := decimal.Price(value)
	if err != nil {
		return 0, fmt.Errorf("invalid execution policy %s %q", name, value)
	}
	return price, nil
}

func optionalBound(name, value string) (float64, bool, error) {
	if strings.TrimSpace(value) == "" {
		return 0, false, nil
	}
	price, err := optionalPrice(name, value, 0)
	if err != nil {
		return 0, false, err
	}
	return price, true, nil
}

// optionalFraction parses a policy value that must sit inside (0,1) when
// strictlyPositive, or [0,1) otherwise.
func optionalFraction(name, value string, strictlyPositive bool) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := decimal.NonNegativeFloat(value)
	if err != nil || parsed >= 1 || (strictlyPositive && parsed <= 0) {
		return 0, fmt.Errorf("invalid execution policy %s %q", name, value)
	}
	return parsed, nil
}
