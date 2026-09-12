package executor

import (
	"math/big"
	"regexp"
	"strings"
)

// sizeMismatchPhrases are the exchange's wordings for a refusal whose cause is
// the order's size rather than anything permanent about the order. The CLOB
// gives no structured code for any of them, only this text.
var sizeMismatchPhrases = []string{
	"not enough balance",
	"not enough allowance",
	"invalid amount",
	"minimum order size",
	"order size",
}

// balancePattern captures the balance the CLOB reports alongside a size
// refusal, as in:
//
//	not enough balance / allowance: the balance is not enough -> balance: 0, order amount: 2000000
//
// Both figures are in 1e6 fixed-point units.
var balancePattern = regexp.MustCompile(`balance:\s*(\d+)`)

// fixedPointScale is the 1e6 denominator the CLOB reports balances in.
var fixedPointScale = big.NewInt(1_000_000)

// isSizeMismatch reports whether the exchange refused the order over size or
// balance. Such a refusal is worth retrying against a freshly read position:
// the order was well formed and the size was simply wrong.
func isSizeMismatch(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, phrase := range sizeMismatchPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// exchangeBalance parses the position the CLOB reports in a size refusal and
// returns it in shares. It is the only place the exchange's own view of a
// position reaches this daemon, so a close that disagreed with the local fill
// ledger can be resized to what the wallet actually holds.
func exchangeBalance(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	match := balancePattern.FindStringSubmatch(err.Error())
	if match == nil {
		return "", false
	}
	raw, ok := new(big.Int).SetString(match[1], 10)
	if !ok {
		return "", false
	}
	shares := new(big.Rat).SetFrac(raw, fixedPointScale)
	// Six decimal places is the full precision of the reported figure, so this
	// is exact rather than rounded.
	return trimTrailingZeros(shares.FloatString(6)), true
}

// trimTrailingZeros drops a decimal string's insignificant fraction, leaving a
// whole number bare. It never returns an empty string.
func trimTrailingZeros(value string) string {
	if !strings.Contains(value, ".") {
		return value
	}
	value = strings.TrimRight(value, "0")
	value = strings.TrimSuffix(value, ".")
	if value == "" || value == "-" {
		return "0"
	}
	return value
}
