// Package positionfeatures publishes strategy-facing per-market position snapshots.
package positionfeatures

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const defaultInterval = 500 * time.Millisecond

type PublisherModule struct {
	store     store.PositionStore
	publisher protocol.ExecutionEventPublisher
	now       func() time.Time
	interval  time.Duration
	onError   func(error)

	mu        sync.Mutex
	sequence  int64
	published map[string]int64
}

func New(repository store.PositionStore, publisher protocol.ExecutionEventPublisher, now func() time.Time, interval time.Duration) (*PublisherModule, error) {
	if repository == nil || publisher == nil {
		return nil, fmt.Errorf("position store and publisher are required")
	}
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	return &PublisherModule{store: repository, publisher: publisher, now: now, interval: interval, published: make(map[string]int64)}, nil
}

func (p *PublisherModule) Init(context.Context) error  { return nil }
func (p *PublisherModule) Close(context.Context) error { return nil }

func (p *PublisherModule) Run(ctx context.Context) error {
	if err := p.Publish(ctx); err != nil && p.onError != nil {
		p.onError(err)
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.Publish(ctx); err != nil && p.onError != nil {
				p.onError(err)
			}
		}
	}
}

func (p *PublisherModule) SetErrorHandler(handler func(error)) {
	p.onError = handler
}

// Publish emits the latest durable state for every known market/token position.
// A frame is intentionally best-effort: the next configured frame supersedes it.
// One position's error does not stop the batch: every position is attempted,
// only the ones that actually publish are marked, and a failed position simply
// retries on the next tick instead of starving every position sorted after it.
func (p *PublisherModule) Publish(ctx context.Context) error {
	positions, err := p.store.PositionFeatures(ctx)
	if err != nil {
		return fmt.Errorf("load position features: %w", err)
	}
	publishedAt := p.now().UTC()
	var errs error
	for _, position := range positions {
		key := position.ConditionID + ":" + position.TokenID + ":" + position.UniqueTag
		if p.wasPublished(key, position.SourceRevision) {
			continue
		}
		subject, err := protocol.PositionFeaturesSubject(position.ConditionID, position.TokenID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("build position feature subject for %s/%s: %w", position.ConditionID, position.TokenID, err))
			continue
		}
		feature, err := mapping.PositionFeature(position, p.nextSequence(), publishedAt)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("map position feature %s: %w", subject, err))
			continue
		}
		if err := p.publisher.PublishJSON(subject, feature); err != nil {
			errs = errors.Join(errs, fmt.Errorf("publish position feature %s: %w", subject, err))
			continue
		}
		p.markPublished(key, position.SourceRevision)
	}
	return errs
}

func (p *PublisherModule) wasPublished(key string, revision int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	value, exists := p.published[key]
	return exists && value == revision
}

func (p *PublisherModule) markPublished(key string, revision int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published[key] = revision
}

func (p *PublisherModule) nextSequence() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	return p.sequence
}
