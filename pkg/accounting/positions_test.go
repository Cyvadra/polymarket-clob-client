package accounting

import "testing"

func TestCreditedShares(t *testing.T) {
	shares, err := CreditedShares("BUY", "10", "0.5", "TAKER")
	if err != nil {
		t.Fatalf("credited shares: %v", err)
	}
	if shares != "9.650000000000000000" {
		t.Fatalf("shares=%q", shares)
	}
	shares, err = CreditedShares("SELL", "10", "0.5", "TAKER")
	if err != nil {
		t.Fatalf("credited shares: %v", err)
	}
	if shares != "10.000000000000000000" {
		t.Fatalf("shares=%q", shares)
	}
}
