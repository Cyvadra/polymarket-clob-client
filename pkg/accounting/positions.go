// Package accounting contains pure execution accounting rules.
package accounting

import (
	"fmt"
	"math/big"
)

// CreditedShares returns the position shares credited for one fill. Taker buy
// fees are charged in outcome shares using Polymarket's 7% fee coefficient.
func CreditedShares(side, shares, price, traderSide string) (string, error) {
	value, ok := new(big.Rat).SetString(shares)
	if !ok || value.Sign() <= 0 {
		return "", fmt.Errorf("invalid fill shares %q", shares)
	}
	if side != "BUY" || traderSide != "TAKER" {
		return value.FloatString(18), nil
	}
	fillPrice, ok := new(big.Rat).SetString(price)
	if !ok || fillPrice.Sign() <= 0 || fillPrice.Cmp(big.NewRat(1, 1)) >= 0 {
		return "", fmt.Errorf("invalid fill price %q", price)
	}
	feeFraction := new(big.Rat).Mul(big.NewRat(7, 100), new(big.Rat).Sub(big.NewRat(1, 1), fillPrice))
	return new(big.Rat).Mul(value, new(big.Rat).Sub(big.NewRat(1, 1), feeFraction)).FloatString(18), nil
}
