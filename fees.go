package clobclient

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

// feeDigits is the precision Polymarket charges fees at. Flooring to it
// reproduced every one of 39 taker fills in the wallet's activity on
// 2026-09-15..16; rounding half-up matched 27.
const feeDigits = 5

// FeeSchedule is a market's trading fee, as the CLOB publishes it under "fd"
// on /clob-markets/{condition_id}. A fill of shares at price p pays
// Rate * shares * (p * (1 - p))^Exponent in collateral; with TakerOnly set,
// maker fills pay nothing. The zero value charges no fee, which is what a
// market without "fd" means.
type FeeSchedule struct {
	Rate      json.Number `json:"r"`
	Exponent  json.Number `json:"e"`
	TakerOnly bool        `json:"to"`
}

// Fee returns the fee for one fill, as a decimal string floored to the
// exchange's precision. taker reports whether the fill took liquidity.
func (s FeeSchedule) Fee(shares, price string, taker bool) (string, error) {
	if s.Rate == "" || (s.TakerOnly && !taker) {
		return "0", nil
	}
	rate, ok := decimal.Rat(s.Rate.String())
	if !ok || rate.Sign() < 0 {
		return "", fmt.Errorf("invalid fee rate %q", s.Rate)
	}
	size, ok := decimal.Rat(shares)
	if !ok || size.Sign() < 0 {
		return "", fmt.Errorf("invalid fill shares %q", shares)
	}
	p, ok := decimal.Rat(price)
	if !ok || p.Sign() <= 0 || p.Cmp(big.NewRat(1, 1)) >= 0 {
		return "", fmt.Errorf("invalid fill price %q", price)
	}
	variance := new(big.Rat).Mul(p, new(big.Rat).Sub(big.NewRat(1, 1), p))
	exponent := 1.0
	if s.Exponent != "" {
		value, err := s.Exponent.Float64()
		if err != nil || value < 0 || math.IsInf(value, 0) {
			return "", fmt.Errorf("invalid fee exponent %q", s.Exponent)
		}
		exponent = value
	}
	scaled := new(big.Rat)
	if whole := math.Trunc(exponent); whole == exponent && whole <= 16 {
		scaled.SetInt64(1)
		for range int(whole) {
			scaled.Mul(scaled, variance)
		}
	} else {
		v, _ := variance.Float64()
		scaled.SetFloat64(math.Pow(v, exponent))
	}
	fee := new(big.Rat).Mul(rate, size)
	fee.Mul(fee, scaled)
	return floorFee(fee), nil
}

// floorFee renders a non-negative fee floored to feeDigits. A fee below the
// exchange's precision is zero, not an error.
func floorFee(fee *big.Rat) string {
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(feeDigits), nil)
	scaled := new(big.Rat).Mul(fee, new(big.Rat).SetInt(unit))
	units := new(big.Int).Quo(scaled.Num(), scaled.Denom())
	return new(big.Rat).SetFrac(units, unit).FloatString(feeDigits)
}

// FeeSchedule returns conditionID's fee schedule, cached for the life of the
// client: a market's schedule is fixed when it is created.
func (c *Client) FeeSchedule(ctx context.Context, conditionID string) (FeeSchedule, error) {
	conditionID = strings.TrimSpace(conditionID)
	if conditionID == "" {
		return FeeSchedule{}, fmt.Errorf("condition ID is required")
	}
	c.metadata.mu.RLock()
	value, ok := c.metadata.feeSchedule[conditionID]
	c.metadata.mu.RUnlock()
	if ok {
		return value, nil
	}
	var out struct {
		FeeDetails *FeeSchedule `json:"fd"`
	}
	path := "/clob-markets/" + url.PathEscape(conditionID)
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: path}, &out); err != nil {
		return FeeSchedule{}, err
	}
	var schedule FeeSchedule
	if out.FeeDetails != nil {
		schedule = *out.FeeDetails
	}
	c.metadata.mu.Lock()
	c.metadata.feeSchedule[conditionID] = schedule
	c.metadata.mu.Unlock()
	return schedule, nil
}
