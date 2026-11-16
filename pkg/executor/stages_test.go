package executor

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

func TestSubmitHookReportsStagesWithinTotal(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, _ := New(storer, client, time.Now)
	exec.SetEntrySizer(&fakeSizer{entry: equity.Entry{TargetUSD: "3", EquityUSD: 100}})
	var (
		calls  int
		total  time.Duration
		stages PrePostStages
	)
	exec.SetSubmitLatencyHook(func(_ protocol.ExecutionIntent, prePost time.Duration, s PrePostStages) {
		calls++
		total, stages = prePost, s
	})
	if err := exec.ExecuteOpen(context.Background(), equityOpenRequest()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
	sum := stages.Size + stages.Insert + stages.Plan + stages.Reserve + stages.Sign + stages.Persist + stages.Publish
	if sum > total {
		t.Fatalf("stage sum %v exceeds total %v", sum, total)
	}
	for name, d := range map[string]time.Duration{"size": stages.Size, "insert": stages.Insert, "plan": stages.Plan, "reserve": stages.Reserve, "sign": stages.Sign, "persist": stages.Persist} {
		if d <= 0 {
			t.Errorf("stage %s not recorded", name)
		}
	}
}
