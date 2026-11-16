package executor

import (
	"context"
	"time"
)

// PrePostStages splits an order's pre-post latency into the steps it spent
// it on. A stage the order's path skipped stays zero; the remainder of the
// total is bookkeeping between stages.
type PrePostStages struct {
	Size    time.Duration // equity sizing of a fraction-sized open
	Insert  time.Duration // persisting the intent
	Plan    time.Duration // market rules, drift check and planning
	Reserve time.Duration // reserving exposure
	Sign    time.Duration // signing, with its metadata lookups
	Persist time.Duration // committing the signed order as SUBMITTING
	Publish time.Duration // publishing the submit-requested transition
}

type stagesKey struct{}

// withStages attaches a fresh stage recorder unless ctx already carries one.
func withStages(ctx context.Context) context.Context {
	if _, ok := ctx.Value(stagesKey{}).(*PrePostStages); ok {
		return ctx
	}
	return context.WithValue(ctx, stagesKey{}, &PrePostStages{})
}

// forkStages gives a goroutine its own copy of the stages so far. A recorder
// is owned by one goroutine; orders placed in parallel each fork one.
func forkStages(ctx context.Context) context.Context {
	stages := &PrePostStages{}
	if parent, ok := ctx.Value(stagesKey{}).(*PrePostStages); ok {
		*stages = *parent
	}
	return context.WithValue(ctx, stagesKey{}, stages)
}

// resetStages starts a fresh recorder, for an order timed from now rather
// than from the request that led to it.
func resetStages(ctx context.Context) context.Context {
	return context.WithValue(ctx, stagesKey{}, &PrePostStages{})
}

// stage adds the time since start to the stage field pick selects. Use as
// defer stage(ctx, time.Now(), func(s *PrePostStages) *time.Duration { return &s.Sign }).
func stage(ctx context.Context, start time.Time, pick func(*PrePostStages) *time.Duration) {
	if stages, ok := ctx.Value(stagesKey{}).(*PrePostStages); ok {
		*pick(stages) += time.Since(start)
	}
}

func stagesOf(ctx context.Context) PrePostStages {
	if stages, ok := ctx.Value(stagesKey{}).(*PrePostStages); ok {
		return *stages
	}
	return PrePostStages{}
}

func stageSize(s *PrePostStages) *time.Duration    { return &s.Size }
func stageInsert(s *PrePostStages) *time.Duration  { return &s.Insert }
func stagePlan(s *PrePostStages) *time.Duration    { return &s.Plan }
func stageReserve(s *PrePostStages) *time.Duration { return &s.Reserve }
func stageSign(s *PrePostStages) *time.Duration    { return &s.Sign }
func stagePersist(s *PrePostStages) *time.Duration { return &s.Persist }
func stagePublish(s *PrePostStages) *time.Duration { return &s.Publish }
