package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func TestSubmitSettlesOrderFromMatchedResponse(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		side        protocol.Side
		tif         protocol.TimeInForce
		shares      string
		response    clobclient.OrderResponse
		wantState   statemachine.State
		wantMatched string
		wantResult  bool
	}{
		// A force close plans the unfloored position; the signed order is 5.28.
		{"sell fully matched against floored size", protocol.SideSell, protocol.TimeInForceFAK, "5.280366",
			clobclient.OrderResponse{Status: "matched", MakingAmount: "5.28", TakingAmount: "2.8512"}, statemachine.StateFilled, "5.28", true},
		{"buy partially matched", protocol.SideBuy, protocol.TimeInForceFAK, "5",
			clobclient.OrderResponse{Status: "matched", MakingAmount: "1.5", TakingAmount: "3"}, statemachine.StateCanceled, "3", true},
		{"immediate order not yet matched", protocol.SideBuy, protocol.TimeInForceFAK, "5",
			clobclient.OrderResponse{Status: "delayed"}, statemachine.StateLive, "0", false},
		{"resting buy fully matched on arrival", protocol.SideBuy, protocol.TimeInForceGTC, "7.14",
			clobclient.OrderResponse{Status: "matched", MakingAmount: "3.4272", TakingAmount: "7.14"}, statemachine.StateFilled, "7.14", true},
		// The unmatched remainder rests, so the order is still working.
		{"resting buy partially matched on arrival", protocol.SideBuy, protocol.TimeInForceGTC, "5",
			clobclient.OrderResponse{Status: "matched", MakingAmount: "1", TakingAmount: "2"}, statemachine.StatePartiallyFilled, "2", false},
		{"resting order placed on the book", protocol.SideBuy, protocol.TimeInForceGTC, "5",
			clobclient.OrderResponse{Status: "live"}, statemachine.StateLive, "0", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			intent := protocol.ExecutionIntent{IntentID: "intent-1", UniqueTag: "lane-a", Kind: protocol.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: testCase.side}
			storer := &fakeStore{intent: store.OrderIntentRecord{IntentID: "intent-1", UniqueTag: "lane-a", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.Side(testCase.side)}}
			response := testCase.response
			response.Success, response.OrderID = true, "order-1"
			exec, err := New(storer, &fakeCLOB{response: &response}, time.Now)
			if err != nil {
				t.Fatalf("new executor: %v", err)
			}
			pub := &recordingPublisher{}
			exec.SetEventPublisher(pub)
			child := plannedChild{Sequence: store.StrategyChildSequence, Shares: testCase.shares, Price: "0.5", TimeInForce: testCase.tif}
			if err := exec.submitOrder(context.Background(), intent, clobclient.SignedOrderV2{OrderID: "order-1"}, child, 1); err != nil {
				t.Fatalf("submit: %v", err)
			}
			if storer.order.State != testCase.wantState || storer.order.MatchedShares != testCase.wantMatched {
				t.Fatalf("expected %s matched=%s, got %s matched=%s", testCase.wantState, testCase.wantMatched, storer.order.State, storer.order.MatchedShares)
			}
			published := false
			for _, record := range pub.publishes {
				if record.subject == protocol.SubjectExecutionOpenResult {
					published = true
				}
			}
			if published != testCase.wantResult {
				t.Fatalf("open result published=%t, want %t: %+v", published, testCase.wantResult, pub.publishes)
			}
		})
	}
}

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
