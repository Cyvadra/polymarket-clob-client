package clobclient

import (
	"math"
	"testing"
	"time"
)

func TestExecutionFromOrderRejectsInvalidNumericValues(t *testing.T) {
	tests := []Order{
		{Status: "LIVE", SizeMatched: "-1"},
		{Status: "LIVE", SizeMatched: "NaN"},
		{Status: "LIVE", SizeMatched: "+Inf"},
		{Status: "LIVE", SizeMatched: "3"},
		{Status: "LIVE", SizeMatched: "1", AvgPrice: "0"},
		{Status: "LIVE", SizeMatched: "1", AvgPrice: "NaN"},
		{Status: "LIVE", SizeMatched: "1", AvgPrice: "1"},
	}
	for _, order := range tests {
		if _, err := ExecutionFromOrder("order", &order, 2, time.Now()); err == nil {
			t.Fatalf("expected invalid order %+v", order)
		}
	}
	if _, err := ExecutionFromOrder("order", &Order{Status: "LIVE"}, math.NaN(), time.Now()); err == nil {
		t.Fatal("expected invalid requested shares")
	}
}

func TestExecutionFromOrderAcceptsValidExecution(t *testing.T) {
	exec, err := ExecutionFromOrder("order", &Order{Status: "MATCHED", SizeMatched: "2", AvgPrice: "0.42"}, 2, time.Now())
	if err != nil || !exec.Terminal || exec.MatchedShares != 2 || exec.AveragePrice != .42 {
		t.Fatalf("execution=%+v err=%v", exec, err)
	}
}
