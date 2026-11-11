package main

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeHistory struct {
	sweeps []store.SweepRecord
	events []store.RiskEvent
}

func (h *fakeHistory) StartDaemonRun(context.Context, store.DaemonRun) (int64, error) { return 7, nil }
func (h *fakeHistory) StopDaemonRun(context.Context, int64, time.Time) error          { return nil }
func (h *fakeHistory) RecordSweep(_ context.Context, s store.SweepRecord) error {
	h.sweeps = append(h.sweeps, s)
	return nil
}
func (h *fakeHistory) LastGuardEvent(_ context.Context, subject string) (string, bool, error) {
	for i := len(h.events) - 1; i >= 0; i-- {
		e := h.events[i]
		if e.Subject == subject && (e.Kind == store.RiskSuspend || e.Kind == store.RiskResume) {
			return e.Kind, true, nil
		}
	}
	return "", false, nil
}
func (h *fakeHistory) RecordRiskEvent(_ context.Context, e store.RiskEvent) error {
	h.events = append(h.events, e)
	return nil
}

// indexStore reads a trade index against a peak of 1.
type indexStore struct {
	store.EquityStore
	index string
}

func (s *indexStore) LatestEquitySnapshot(context.Context) (store.EquitySnapshotRecord, bool, error) {
	return store.EquitySnapshotRecord{ID: 1, TakenAt: time.Unix(100, 0), TradeIndex: s.index}, true, nil
}
func (s *indexStore) PeakTradeIndexSince(context.Context, time.Time) (string, bool, error) {
	return "1", true, nil
}

func TestObserverRecordsSuspendAndResumeOnce(t *testing.T) {
	ctx := context.Background()
	history := &fakeHistory{}
	obs := newObserver(newMetrics(), history, func() time.Time { return time.Unix(200, 0) })
	obs.startRun(ctx, "0xsigner", "0xmaker", config{})
	equities := &indexStore{index: "0.9"}
	guard, err := equity.NewDrawdownGuard(equities, time.Unix(0, 0), equity.FractionLimit(0.2))
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"0.9", "0.7", "0.6", "0.95", "0.95"} {
		equities.index = index
		if _, err := guard.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		obs.guard(ctx, guard)
	}
	if len(history.events) != 2 || history.events[0].Kind != store.RiskSuspend || history.events[1].Kind != store.RiskResume {
		t.Fatalf("want one suspend then one resume, got %+v", history.events)
	}
	if history.events[0].RunID != 7 || history.events[0].Subject != "wallet" {
		t.Fatalf("event not tied to the run and guard: %+v", history.events[0])
	}

	obs.sweep(ctx, settlement.Sweep{Checked: 3})
	obs.sweep(ctx, settlement.Sweep{Checked: 3, Redeemed: 1})
	if len(history.sweeps) != 1 || history.sweeps[0].Redeemed != 1 {
		t.Fatalf("want only the sweep that did something, got %+v", history.sweeps)
	}
}

func TestObserverSkipsSuspendAlreadyStoredBeforeRestart(t *testing.T) {
	ctx := context.Background()
	history := &fakeHistory{events: []store.RiskEvent{{Kind: store.RiskSuspend, Subject: "wallet"}}}
	obs := newObserver(newMetrics(), history, func() time.Time { return time.Unix(200, 0) })
	equities := &indexStore{index: "0.7"}
	guard, err := equity.NewDrawdownGuard(equities, time.Unix(0, 0), equity.FractionLimit(0.2))
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"0.7", "0.6", "0.95"} {
		equities.index = index
		if _, err := guard.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		obs.guard(ctx, guard)
	}
	if len(history.events) != 2 || history.events[1].Kind != store.RiskResume {
		t.Fatalf("want only the resume after the stored suspend, got %+v", history.events)
	}
}

func TestObserverRecordsDeferralOnlyWhenHeldChanges(t *testing.T) {
	ctx := context.Background()
	history := &fakeHistory{}
	obs := newObserver(newMetrics(), history, func() time.Time { return time.Unix(200, 0) })
	for _, held := range []int{2, 2, 2, 3, 3, 0, 0, 3} {
		obs.snapshotDeferred(ctx, held)
	}
	var got []any
	for _, e := range history.events {
		got = append(got, e.Details["winners_held"])
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 3 {
		t.Fatalf("want deferrals for 2, 3 and 3 again after a snapshot, got %v", got)
	}
}
