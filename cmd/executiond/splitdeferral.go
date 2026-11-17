package main

import (
	"context"
	"time"
)

// splitDeferral holds equity snapshots off while a split or merge settles. A
// split or merge moves cash on chain before the exchange's balance shows it,
// and a snapshot taken in between reads the gap as a deposit or withdrawal.
// It defers for operations settling within settle, but not for longer than
// max in a row, so steady refills cannot starve equity.
type splitDeferral struct {
	settling func(ctx context.Context, since time.Time) (bool, error)
	now      func() time.Time
	settle   time.Duration
	max      time.Duration
	since    time.Time
}

// deferral is the outcome of one check.
type deferral int

const (
	deferNone    deferral = iota // nothing settling: record
	deferWait                    // settling: hold the snapshot off
	deferExpired                 // settling past max: record anyway
)

func newSplitDeferral(settling func(context.Context, time.Time) (bool, error)) *splitDeferral {
	return &splitDeferral{settling: settling, now: time.Now, settle: 30 * time.Second, max: 2 * time.Minute}
}

// check reports whether a snapshot due now should wait. An error reading the
// operations records the snapshot rather than holding it off.
func (d *splitDeferral) check(ctx context.Context) (deferral, error) {
	now := d.now()
	settling, err := d.settling(ctx, now.Add(-d.settle))
	if err != nil {
		return deferNone, err
	}
	if !settling {
		d.since = time.Time{}
		return deferNone, nil
	}
	if d.since.IsZero() {
		d.since = now
	}
	if now.Sub(d.since) > d.max {
		// This snapshot records; the next deferral starts its own window.
		d.since = time.Time{}
		return deferExpired, nil
	}
	return deferWait, nil
}
