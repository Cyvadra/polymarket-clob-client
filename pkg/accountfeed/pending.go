package accountfeed

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultPendingTimeout bounds how long events wait for an order's row. The
// executor always resolves an order (Stored or Forget) once its commit
// returns, so this only catches a bug.
const DefaultPendingTimeout = 10 * time.Second

// PendingOrders holds account events for orders posted before their row
// committed. The executor commits an order's row alongside its POST, so the
// exchange can report the order first; such an event would otherwise find no
// row and be dropped.
//
// The executor calls Expect before the POST and Stored once the row exists,
// or Forget if it never will. A consumer that finds no row calls Hold; while
// the order is expected, the event waits and is replayed by Stored, in the
// order events arrived. A nil *PendingOrders holds nothing.
type PendingOrders struct {
	ctx     context.Context
	timeout time.Duration
	onError func(error)

	mu      sync.Mutex
	pending map[string]*pendingOrder
}

type pendingOrder struct {
	held  []func(context.Context)
	timer *time.Timer
}

// NewPendingOrders replays held events on ctx. onError, if set, hears of an
// order left unresolved past the timeout.
func NewPendingOrders(ctx context.Context, onError func(error)) *PendingOrders {
	return &PendingOrders{ctx: ctx, timeout: DefaultPendingTimeout, onError: onError, pending: map[string]*pendingOrder{}}
}

// Expect marks orderID as posted with its row not yet committed.
func (p *PendingOrders) Expect(orderID string) {
	if p == nil || orderID == "" {
		return
	}
	entry := &pendingOrder{}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[orderID]; ok {
		return
	}
	entry.timer = time.AfterFunc(p.timeout, func() { p.expire(orderID, entry) })
	p.pending[orderID] = entry
}

// Hold queues fn for orderID if the order is expected, reporting whether it
// did. A false answer means the order is not awaited and the caller handles
// the event as it would any unknown order.
func (p *PendingOrders) Hold(orderID string, fn func(context.Context)) bool {
	if p == nil || orderID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.pending[orderID]
	if !ok {
		return false
	}
	entry.held = append(entry.held, fn)
	return true
}

// Stored replays what was held for orderID, in arrival order, on the calling
// goroutine. The order stays expected until the queue is drained, so an event
// arriving during the replay queues behind the earlier ones instead of racing
// them.
func (p *PendingOrders) Stored(orderID string) {
	if p == nil || orderID == "" {
		return
	}
	for {
		p.mu.Lock()
		entry, ok := p.pending[orderID]
		if !ok {
			p.mu.Unlock()
			return
		}
		if len(entry.held) == 0 {
			entry.timer.Stop()
			delete(p.pending, orderID)
			p.mu.Unlock()
			return
		}
		batch := entry.held
		entry.held = nil
		p.mu.Unlock()
		for _, fn := range batch {
			fn(p.ctx)
		}
	}
}

// Forget drops orderID and anything held for it: its row will never exist.
func (p *PendingOrders) Forget(orderID string) {
	if p == nil || orderID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.pending[orderID]; ok {
		entry.timer.Stop()
		delete(p.pending, orderID)
	}
}

func (p *PendingOrders) expire(orderID string, entry *pendingOrder) {
	p.mu.Lock()
	if p.pending[orderID] != entry {
		p.mu.Unlock()
		return
	}
	delete(p.pending, orderID)
	dropped := len(entry.held)
	p.mu.Unlock()
	if p.onError != nil {
		p.onError(fmt.Errorf("order %s was never stored or forgotten within %s; dropped %d held account events", orderID, p.timeout, dropped))
	}
}
