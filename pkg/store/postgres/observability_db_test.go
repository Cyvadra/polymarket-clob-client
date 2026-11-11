//go:build postgres

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func TestObservabilityRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	runID, err := s.StartDaemonRun(ctx, store.DaemonRun{Signer: "0xsigner", Maker: "0xmaker", Strategies: []string{"pmm"}, Config: map[string]any{"max_ladder_levels": 3}})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.RecordSweep(ctx, store.SweepRecord{RunID: runID, At: at, Checked: 4, Lost: 1, Redeemed: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRiskEvent(ctx, store.RiskEvent{RunID: runID, At: at, Kind: store.RiskSuspend, Subject: "pmm", Reason: "over limit", Details: map[string]any{"drawdown": 0.3}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRiskEvent(ctx, store.RiskEvent{At: at}); err == nil {
		t.Fatal("expected an error for a risk event with no kind")
	}
	if err := s.StopDaemonRun(ctx, runID, at); err != nil {
		t.Fatal(err)
	}
	var redeemed int
	var kind, maker string
	var drawdown float64
	var stopped bool
	if err := s.pool.QueryRow(ctx, `
		SELECT sw.redeemed, re.kind, (re.details->>'drawdown')::float8, r.maker, r.stopped_at IS NOT NULL
		FROM daemon_runs r JOIN settlement_sweeps sw USING (run_id) JOIN risk_events re USING (run_id)
		WHERE r.run_id = $1`, runID).Scan(&redeemed, &kind, &drawdown, &maker, &stopped); err != nil {
		t.Fatal(err)
	}
	if redeemed != 2 || kind != store.RiskSuspend || drawdown != 0.3 || maker != "0xmaker" || !stopped {
		t.Fatalf("got redeemed=%d kind=%s drawdown=%v maker=%s stopped=%v", redeemed, kind, drawdown, maker, stopped)
	}
	var intents int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM v_intents_daily`).Scan(&intents); err != nil {
		t.Fatalf("v_intents_daily: %v", err)
	}
}

func TestLastGuardEvent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	subject := "guard-" + time.Now().Format("150405.000000000")
	if _, ok, err := s.LastGuardEvent(ctx, subject); err != nil || ok {
		t.Fatalf("want no event yet, got ok=%v err=%v", ok, err)
	}
	at := time.Now().UTC()
	for i, kind := range []string{store.RiskSuspend, store.RiskSizingBase, store.RiskResume, store.RiskSnapshotDeferred} {
		if err := s.RecordRiskEvent(ctx, store.RiskEvent{At: at.Add(time.Duration(i) * time.Second), Kind: kind, Subject: subject}); err != nil {
			t.Fatal(err)
		}
	}
	kind, ok, err := s.LastGuardEvent(ctx, subject)
	if err != nil || !ok || kind != store.RiskResume {
		t.Fatalf("want resume, got %q ok=%v err=%v", kind, ok, err)
	}
}
