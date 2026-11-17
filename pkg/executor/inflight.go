package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// inflightCancelInterval is how often a cancel is re-sent for an order whose
// POST has not been answered. Before the exchange has the order it refuses
// the cancel; cmd/takerholdtest probes the same window (at 10ms by default).
const inflightCancelInterval = 25 * time.Millisecond

// maxInflightCancelSends bounds the cancels outstanding at once for one
// in-flight POST, so a slow POST does not pile up requests: a tick that finds
// them all still waiting sends nothing.
const maxInflightCancelSends = 4

// inflightPost is an order whose POST this process has sent and not yet had
// answered. Its row is SUBMITTING, carrying the order ID computed at signing,
// so a cancel can go out by that ID before the POST returns.
type inflightPost struct {
	done chan struct{}
	// cancelWanted is set by a CANCEL_OPEN that arrived during the POST.
	// Once the POST is answered, the submitting goroutine cancels the order
	// if the answer left it working or unknown.
	cancelWanted atomic.Bool
	// answered is set by the poster before it reads cancelWanted. A
	// CANCEL_OPEN that sets cancelWanted and then finds answered set cannot
	// tell whether the poster saw it, so it runs the follow-up itself; a
	// second cancel is harmless, a missed one is not.
	answered atomic.Bool
	// firing is set while a goroutine is sending cancels at the POST.
	firing atomic.Bool
	// canceled is set once an in-flight cancel was accepted.
	canceled atomic.Bool
}

type inflightPosts struct {
	mu    sync.Mutex
	posts map[string]*inflightPost
}

func inflightKey(orderID string) string { return strings.ToLower(orderID) }

func (p *inflightPosts) start(orderID string) *inflightPost {
	post := &inflightPost{done: make(chan struct{})}
	if orderID == "" {
		return post
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.posts == nil {
		p.posts = make(map[string]*inflightPost)
	}
	p.posts[inflightKey(orderID)] = post
	return post
}

func (p *inflightPosts) finish(orderID string, post *inflightPost) {
	p.mu.Lock()
	if current := p.posts[inflightKey(orderID)]; current == post {
		delete(p.posts, inflightKey(orderID))
	}
	p.mu.Unlock()
	close(post.done)
}

func (p *inflightPosts) get(orderID string) (*inflightPost, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	post, ok := p.posts[inflightKey(orderID)]
	return post, ok
}

// postGateKey carries the hook sendOrder calls once its order is SUBMITTING
// and registered as in flight; see WithPostGate.
type postGateKey struct{}

// WithPostGate returns ctx carrying gate, which sendOrder calls just before
// the POST, once the order's SUBMITTING row is written and the POST is
// registered as in flight. gate reports whether a CANCEL_OPEN was taken for
// the order earlier, while it was being signed and persisted and so could not
// be found; if so the order is cancelled as though the CANCEL_OPEN had found
// it in flight. The NATS open dispatcher installs it.
func WithPostGate(ctx context.Context, gate func() bool) context.Context {
	return context.WithValue(ctx, postGateKey{}, gate)
}

// cancelSubmittingOrder withdraws an order whose POST may still be in
// flight. When this process is the one posting it, cancels go out at once
// and again every inflightCancelInterval until the POST is answered, one
// accepted, or the policy's cancel timeout passes; the poster then follows up
// once it has the answer (finishInflightCancel). An order this process is not
// posting (another instance, one left over from a restart, or one whose POST
// ended unknown) is cancelled until the exchange accepts or the cancel
// timeout passes.
//
// It returns without waiting, so the lane lock is not held across the POST.
func (e *Executor) cancelSubmittingOrder(ctx context.Context, policy protocol.ExecutionPolicy, order store.SignedOrderRecord) error {
	if order.ExchangeOrderID == "" {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	post, ok := e.inflight.get(order.ExchangeOrderID)
	if !ok {
		go e.cancelUntilAccepted(ctx, policy, order.ExchangeOrderID)
		return nil
	}
	e.wantInflightCancel(ctx, policy, order.IntentID, order.ChildSequence, order.ExchangeOrderID, post)
	return nil
}

// wantInflightCancel marks post for cancel and starts firing at it unless a
// goroutine already is. If the poster has already taken its answer it may
// have missed the mark, so the follow-up runs here too.
func (e *Executor) wantInflightCancel(ctx context.Context, policy protocol.ExecutionPolicy, intentID string, sequence int, orderID string, post *inflightPost) {
	post.cancelWanted.Store(true)
	if post.answered.Load() {
		go e.followUpCancel(ctx, policy, intentID, sequence, post)
		return
	}
	if post.firing.CompareAndSwap(false, true) {
		go e.fireInflightCancel(ctx, policy, orderID, post)
	}
}

// fireInflightCancel sends a cancel for orderID now and every
// inflightCancelInterval, each on its own goroutine so one round trip never
// delays the next (at most maxInflightCancelSends outstanding), until the
// POST is answered, a cancel is accepted, or the cancel timeout passes.
func (e *Executor) fireInflightCancel(ctx context.Context, policy protocol.ExecutionPolicy, orderID string, post *inflightPost) {
	defer post.firing.Store(false)
	ctx, cancel := context.WithTimeout(ctx, cancelTimeout(policy))
	defer cancel()
	var sends sync.WaitGroup
	defer sends.Wait()
	slots := make(chan struct{}, maxInflightCancelSends)
	var lastRefusal atomic.Value
	send := func() {
		defer sends.Done()
		defer func() { <-slots }()
		result, err := e.clob.CancelOrderResult(ctx, orderID)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				lastRefusal.Store(err.Error())
			}
		case result.Has(orderID):
			post.canceled.Store(true)
		default:
			if reason, refused := result.Refused(orderID); refused {
				lastRefusal.Store(reason)
			}
		}
	}
	ticker := time.NewTicker(inflightCancelInterval)
	defer ticker.Stop()
	for {
		if post.canceled.Load() {
			return
		}
		select {
		case slots <- struct{}{}:
			sends.Add(1)
			go send()
		default:
		}
		select {
		case <-post.done:
			return
		case <-ctx.Done():
			if reason, ok := lastRefusal.Load().(string); ok && !post.canceled.Load() {
				e.reportError(fmt.Errorf("cancel of in-flight order %s not accepted within %s: %s", orderID, cancelTimeout(policy), reason))
			}
			return
		case <-ticker.C:
		}
	}
}

// cancelUntilAccepted cancels orderID one request at a time, every
// inflightCancelInterval, until the exchange accepts or the cancel timeout
// passes. It serves orders whose POST outcome is unknown: a refusal may only
// mean the exchange does not have the order yet, so none ends the attempt.
// It reports whether the exchange accepted the cancel.
func (e *Executor) cancelUntilAccepted(ctx context.Context, policy protocol.ExecutionPolicy, orderID string) bool {
	ctx, cancel := context.WithTimeout(ctx, cancelTimeout(policy))
	defer cancel()
	ticker := time.NewTicker(inflightCancelInterval)
	defer ticker.Stop()
	var last string
	for {
		result, err := e.clob.CancelOrderResult(ctx, orderID)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				last = err.Error()
			}
		case result.Has(orderID):
			return true
		default:
			if reason, refused := result.Refused(orderID); refused {
				last = reason
			}
		}
		select {
		case <-ctx.Done():
			if last != "" {
				e.reportError(fmt.Errorf("cancel of order %s not accepted within %s: %s", orderID, cancelTimeout(policy), last))
			}
			return false
		case <-ticker.C:
		}
	}
}

// finishInflightCancel runs on the posting goroutine after the POST's answer
// is recorded. An order a CANCEL_OPEN asked to withdraw during the POST is
// cancelled now if the answer left it working, or retried until accepted if
// the answer was unknown; one already canceled, filled, or refused needs
// nothing.
func (e *Executor) finishInflightCancel(ctx context.Context, intent protocol.ExecutionIntent, sequence int, post *inflightPost) {
	post.answered.Store(true)
	e.followUpCancel(ctx, intent.Policy, intent.IntentID, sequence, post)
}

func (e *Executor) followUpCancel(ctx context.Context, policy protocol.ExecutionPolicy, intentID string, sequence int, post *inflightPost) {
	if !post.cancelWanted.Load() || post.canceled.Load() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	order, err := e.store.OrderByIntent(ctx, intentID, sequence)
	if err != nil {
		e.reportError(fmt.Errorf("load order %s/%d for requested cancel: %w", intentID, sequence, err))
		return
	}
	switch order.State {
	case statemachine.StateLive, statemachine.StatePartiallyFilled, statemachine.StateCancelRequested:
	case statemachine.StateSubmitting, statemachine.StateSubmitUnknown:
		// The POST failed without saying whether the exchange took the
		// order: keep cancelling it in case it did.
		if order.ExchangeOrderID != "" {
			e.cancelUntilAccepted(ctx, policy, order.ExchangeOrderID)
		}
		return
	default:
		return
	}
	if err := e.cancelOpenOrder(ctx, policy, order, cancelRequested); err != nil {
		e.reportError(fmt.Errorf("cancel order %s after its POST: %w", order.ExchangeOrderID, err))
	}
}

// cancelGone reports whether a cancel error says the order has already ended,
// so the cancel has nothing left to do.
func cancelGone(err error) bool {
	var refused *clobclient.CancelRefusedError
	return errors.As(err, &refused) && refused.OrderGone()
}
