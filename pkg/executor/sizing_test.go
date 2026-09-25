package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

type fakeSizer struct {
	entry    equity.Entry
	err      error
	fraction string
}

func (f *fakeSizer) Size(_ context.Context, fraction string) (equity.Entry, error) {
	f.fraction = fraction
	return f.entry, f.err
}

func equityOpenRequest() protocol.ExecutionOpenRequest {
	req := openRequest()
	req.TargetUSD = ""
	req.TargetEquityFraction = "0.03"
	return req
}

func TestExecuteOpenResolvesEquityFraction(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, _ := New(storer, client, time.Now)
	sizer := &fakeSizer{entry: equity.Entry{TargetUSD: "3", EquityUSD: 100}}
	exec.SetEntrySizer(sizer)
	if err := exec.ExecuteOpen(context.Background(), equityOpenRequest()); err != nil {
		t.Fatalf("execute open: %v", err)
	}
	if sizer.fraction != "0.03" {
		t.Fatalf("sized fraction %q", sizer.fraction)
	}
	if len(storer.insertedIntents) != 1 {
		t.Fatalf("intents=%d", len(storer.insertedIntents))
	}
	got := storer.insertedIntents[0]
	if got.TargetUSD != "3" || got.TargetEquityFraction != "0.03" || got.SizedEquityUSD != "100.000000" {
		t.Fatalf("intent=%+v", got)
	}
	// $3 at 0.5 plans 6 shares.
	if client.created.Shares != 6 {
		t.Fatalf("created=%+v", client.created)
	}
}

func TestExecuteOpenRejectsEquitySizingProblems(t *testing.T) {
	both := equityOpenRequest()
	both.TargetUSD = "1"
	sell := equityOpenRequest()
	sell.Side = protocol.SideSell
	cases := []struct {
		name  string
		req   protocol.ExecutionOpenRequest
		sizer EntrySizer
		code  string
	}{
		{"both targets", both, &fakeSizer{}, protocol.ReasonInvalidIntent},
		{"sell", sell, &fakeSizer{}, protocol.ReasonInvalidIntent},
		{"no sizer", equityOpenRequest(), nil, protocol.ReasonInvalidIntent},
		{"bad fraction", equityOpenRequest(), &fakeSizer{err: fmt.Errorf("%w: 3", equity.ErrInvalidFraction)}, protocol.ReasonInvalidIntent},
		{"no cash", equityOpenRequest(), &fakeSizer{err: fmt.Errorf("%w: short", equity.ErrInsufficientCash)}, protocol.ReasonExposureLimit},
		{"balance down", equityOpenRequest(), &fakeSizer{err: errors.New("timeout")}, protocol.ReasonExecutionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storer := &fakeStore{inserted: true}
			client := &fakeCLOB{}
			exec, _ := New(storer, client, time.Now)
			if tc.sizer != nil {
				exec.SetEntrySizer(tc.sizer)
			}
			pub := &resultPublisher{}
			exec.SetEventPublisher(pub)
			if err := exec.ExecuteOpen(context.Background(), tc.req); err == nil {
				t.Fatal("expected rejection")
			}
			result, ok := pub.value.(protocol.ExecutionOpenResult)
			if !ok || result.ReasonCode != tc.code {
				t.Fatalf("result=%+v, want %s", pub.value, tc.code)
			}
			if len(storer.insertedIntents) != 0 || client.submissions != 0 {
				t.Fatalf("rejected open reached the store or exchange")
			}
		})
	}
}
