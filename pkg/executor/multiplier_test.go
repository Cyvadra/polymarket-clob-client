package executor

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

func TestScaleSize(t *testing.T) {
	for _, tc := range []struct{ value, multiplier, want string }{
		{"4", "2.5", "10"},
		{"3", "0.5", "1.5"},
		{"1", "", "1"},
		{"0.000001", "0.5", "0"},
		{"0.333333", "0.5", "0.166666"},
		{"abc", "2", "abc"},
		{"", "2", ""},
	} {
		if got := scaleSize(tc.value, tc.multiplier, usdDigits); got != tc.want {
			t.Fatalf("scaleSize(%q, %q)=%q, want %q", tc.value, tc.multiplier, got, tc.want)
		}
	}
}

func TestExecuteOpenScalesDollarTargetByStrategyPrefix(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, _ := New(storer, client, time.Now)
	exec.SetSizeMultipliers(map[string]string{"strat": "2.5", "other": "0.5"})
	if err := exec.ExecuteOpen(context.Background(), openRequest()); err != nil {
		t.Fatalf("execute open: %v", err)
	}
	if got := storer.insertedIntents[0].TargetUSD; got != "2.5" {
		t.Fatalf("target_usd=%q, want 2.5", got)
	}
	// $2.5 at 0.5 plans 5 shares.
	if client.created.Shares != 5 {
		t.Fatalf("created=%+v", client.created)
	}
}

func TestExecuteOpenScalesEquityFraction(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, _ := New(storer, client, time.Now)
	sizer := &fakeSizer{entry: equity.Entry{TargetUSD: "1.5", EquityUSD: 100}}
	exec.SetEntrySizer(sizer)
	exec.SetSizeMultipliers(map[string]string{"strategy": "0.5"})
	if err := exec.ExecuteOpen(context.Background(), equityOpenRequest()); err != nil {
		t.Fatalf("execute open: %v", err)
	}
	if sizer.fraction != "0.015" {
		t.Fatalf("sized fraction %q, want 0.015", sizer.fraction)
	}
	if got := storer.insertedIntents[0].TargetEquityFraction; got != "0.015" {
		t.Fatalf("intent fraction %q", got)
	}
}
