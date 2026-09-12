package executor

import (
	"testing"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func TestExchangeBalanceParsesTheReportedHolding(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		body  string
		want  string
		found bool
	}{
		{name: "unsettled", body: `{"error":"not enough balance / allowance: the balance is not enough -> balance: 0, order amount: 2000000"}`, want: "0", found: true},
		{name: "partial holding", body: `{"error":"not enough balance / allowance: the balance is not enough -> balance: 2000000, order amount: 10000000"}`, want: "2", found: true},
		{name: "fractional holding", body: `{"error":"not enough balance -> balance: 2500000, order amount: 10000000"}`, want: "2.5", found: true},
		{name: "no figure", body: `{"error":"order size below the minimum"}`, found: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order", Body: []byte(testCase.body)}
			got, ok := exchangeBalance(err)
			if ok != testCase.found {
				t.Fatalf("expected found=%v, got %v", testCase.found, ok)
			}
			if ok && got != testCase.want {
				t.Fatalf("expected balance %q, got %q", testCase.want, got)
			}
		})
	}
}

func TestIsSizeMismatchRecognisesTheExchangeWordings(t *testing.T) {
	mismatches := []string{
		"not enough balance / allowance: the balance is not enough",
		"not enough allowance",
		"invalid amount",
		"order size below the minimum order size",
	}
	for _, message := range mismatches {
		err := &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order", Body: []byte(message)}
		if !isSizeMismatch(err) {
			t.Fatalf("expected %q to read as a size mismatch", message)
		}
	}
	permanent := &clobclient.APIError{StatusCode: 403, Method: "POST", Path: "/order", Body: []byte("geoblocked region")}
	if isSizeMismatch(permanent) {
		t.Fatal("expected a geoblock not to read as a size mismatch")
	}
}
