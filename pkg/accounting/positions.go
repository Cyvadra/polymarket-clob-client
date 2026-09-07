// Package accounting contains pure execution accounting rules.
package accounting

import (
	"fmt"
	"math/big"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
)

// takerFeeCoefficient is the taker buy fee charged in outcome shares, as a
// fraction of the pre-fee share count. Polymarket prices taker buy fees as
// feeRate * (1 - price) of the shares; the coefficient here is 7% and is a
// deliberate strategy-level override of the exchange-reported fee_rate_bps,
// which the position ledger intentionally does not consult.
var takerFeeCoefficient = big.NewRat(7, 100)

// CreditedShares returns the position shares credited for one fill. Taker buy
// fees are charged in outcome shares using the configured taker fee
// coefficient; all other sides credit the full share amount.
func CreditedShares(side, shares, price, traderSide string) (string, error) {
	value, ok := decimal.Rat(shares)
	if !ok || value.Sign() <= 0 {
		return "", fmt.Errorf("invalid fill shares %q", shares)
	}
	if side != "BUY" || traderSide != "TAKER" {
		return value.FloatString(18), nil
	}
	fillPrice, ok := decimal.Rat(price)
	if !ok || fillPrice.Sign() <= 0 || fillPrice.Cmp(big.NewRat(1, 1)) >= 0 {
		return "", fmt.Errorf("invalid fill price %q", price)
	}
	feeFraction := new(big.Rat).Mul(takerFeeCoefficient, new(big.Rat).Sub(big.NewRat(1, 1), fillPrice))
	return new(big.Rat).Mul(value, new(big.Rat).Sub(big.NewRat(1, 1), feeFraction)).FloatString(18), nil
}
