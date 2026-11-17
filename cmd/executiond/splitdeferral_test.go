package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSplitDeferral(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	settling := true
	var readErr error
	d := newSplitDeferral(func(context.Context, time.Time) (bool, error) { return settling, readErr })
	d.now = func() time.Time { return now }
	check := func(want deferral) {
		t.Helper()
		got, _ := d.check(context.Background())
		if got != want {
			t.Fatalf("at %s: got %d, want %d", now.Format(time.TimeOnly), got, want)
		}
	}
	check(deferWait)
	now = now.Add(time.Minute)
	check(deferWait)
	now = now.Add(time.Minute + time.Second)
	check(deferExpired)
	// The expired window ends; a new one starts.
	check(deferWait)
	settling = false
	check(deferNone)
	settling = true
	now = now.Add(time.Hour)
	check(deferWait)
	readErr = errors.New("database down")
	if got, err := d.check(context.Background()); got != deferNone || err == nil {
		t.Fatalf("read error: got %d, %v", got, err)
	}
}
