package executor

import (
	"errors"
	"strings"
	"testing"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func TestIsOrderRejectedSeparatesDefinitiveRefusalFromUnknownOutcome(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		err      error
		rejected bool
	}{
		{"success false", &clobclient.OrderRejectedError{Message: "not enough balance"}, true},
		{"geoblocked", &clobclient.APIError{StatusCode: 403, Method: "POST", Path: "/order", Body: []byte(`{"error":"Trading restricted in your region"}`)}, true},
		{"bad request", &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order"}, true},
		{"unauthorized", &clobclient.APIError{StatusCode: 401, Method: "POST", Path: "/order"}, true},
		{"request timeout", &clobclient.APIError{StatusCode: 408, Method: "POST", Path: "/order"}, false},
		{"rate limited", &clobclient.APIError{StatusCode: 429, Method: "POST", Path: "/order"}, false},
		{"server error", &clobclient.APIError{StatusCode: 502, Method: "POST", Path: "/order"}, false},
		{"transport failure", errors.New("dial tcp: connection reset"), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isOrderRejected(testCase.err); got != testCase.rejected {
				t.Fatalf("isOrderRejected = %v, want %v", got, testCase.rejected)
			}
		})
	}
}

func TestRejectionReasonKeepsExchangeWording(t *testing.T) {
	if reason := rejectionReason(&clobclient.OrderRejectedError{Message: "not enough balance"}); reason != "not enough balance" {
		t.Fatalf("rejection reason = %q", reason)
	}
	reason := rejectionReason(&clobclient.APIError{StatusCode: 403, Method: "POST", Path: "/order", Body: []byte(`{"error":"Trading restricted in your region"}`)})
	if !strings.Contains(reason, "403") || !strings.Contains(reason, "Trading restricted in your region") {
		t.Fatalf("rejection reason lost the exchange response: %q", reason)
	}
}
