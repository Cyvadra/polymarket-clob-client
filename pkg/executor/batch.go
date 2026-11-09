package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

// A batch gathers the exchange calls of several goroutines, one per order,
// into a single call. Each goroutine keeps its own path (persist, state walk,
// result handling) and meets the others only at the exchange: it either
// joins the batch with its request and waits for its own answer, or leaves
// without one. The call goes out when the last member has joined or left, so
// every member must do one or the other; leave after join does nothing,
// which lets a member defer leave.
//
// A ladder's levels go out as one POST /orders, and the cancels due in one
// deadline pass or one ladder stop as one DELETE /orders: one round trip
// instead of one per order, and no per-order queueing on the connection.

type batchAnswer[Resp any] struct {
	resp Resp
	err  error
}

type batch[Req, Resp any] struct {
	mu      sync.Mutex
	pending int
	reqs    []Req
	answers []chan batchAnswer[Resp]
	fire    func(context.Context, []Req) ([]Resp, []error)
}

func newBatch[Req, Resp any](members int, fire func(context.Context, []Req) ([]Resp, []error)) *batch[Req, Resp] {
	return &batch[Req, Resp]{pending: members, fire: fire}
}

// batchMember is one goroutine's place in a batch.
type batchMember[Req, Resp any] struct {
	batch *batch[Req, Resp]
	once  sync.Once
}

func (b *batch[Req, Resp]) member() *batchMember[Req, Resp] {
	return &batchMember[Req, Resp]{batch: b}
}

// call joins the batch with req and waits for its answer. The last member to
// arrive sends the batch on its own ctx. ok is false when the member has
// already joined or left: a ctx carrying a member can outlive its batch (a
// follow-up started from it), and such a call goes out on its own instead.
func (m *batchMember[Req, Resp]) call(ctx context.Context, req Req) (resp Resp, err error, ok bool) {
	answer := make(chan batchAnswer[Resp], 1)
	m.once.Do(func() {
		ok = true
		b := m.batch
		b.mu.Lock()
		b.reqs = append(b.reqs, req)
		b.answers = append(b.answers, answer)
		b.pending--
		last := b.pending == 0
		b.mu.Unlock()
		if last {
			b.send(ctx)
		}
	})
	if !ok {
		return resp, nil, false
	}
	a := <-answer
	return a.resp, a.err, true
}

// leave gives up the member's place, so the batch need not wait for it.
func (m *batchMember[Req, Resp]) leave() {
	m.once.Do(func() {
		b := m.batch
		b.mu.Lock()
		b.pending--
		last := b.pending == 0
		b.mu.Unlock()
		if last {
			// The batch can be empty when every member left; send skips
			// the call then. Leave has no ctx of its own, and the call
			// carries its own timeout.
			b.send(context.Background())
		}
	})
}

func (b *batch[Req, Resp]) send(ctx context.Context) {
	b.mu.Lock()
	reqs, answers := b.reqs, b.answers
	b.mu.Unlock()
	if len(reqs) == 0 {
		return
	}
	resps, errs := b.fire(ctx, reqs)
	for i, answer := range answers {
		var a batchAnswer[Resp]
		if i < len(resps) {
			a.resp = resps[i]
		}
		if i < len(errs) {
			a.err = errs[i]
		}
		answer <- a
	}
}

// chunks splits n members into batches of at most clobclient.MaxBatchOrders,
// returning each member's batch index.
func chunkSizes(n int) []int {
	var sizes []int
	for n > 0 {
		size := min(n, clobclient.MaxBatchOrders)
		sizes = append(sizes, size)
		n -= size
	}
	return sizes
}

// batchUnsupported reports whether the exchange refused a batch endpoint as
// unknown, so the caller falls back to one call per order.
func batchUnsupported(err error) bool {
	var apiErr *clobclient.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed)
}

func fillErrors(n int, err error) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}
	return errs
}

// ---- placing ----

type placeRequest struct {
	signed   clobclient.SignedOrderV2
	tif      clobclient.OrderType
	postOnly bool
}

type placeBatch = batch[placeRequest, *clobclient.OrderResponse]
type placeMember = batchMember[placeRequest, *clobclient.OrderResponse]

type placeMemberKey struct{}

// newPlaceBatches returns one member per order, grouped into batches.
func (e *Executor) newPlaceMembers(n int) []*placeMember {
	members := make([]*placeMember, 0, n)
	for _, size := range chunkSizes(n) {
		b := newBatch(size, e.submitBatch)
		for range size {
			members = append(members, b.member())
		}
	}
	return members
}

// submit posts one order, through the batch its ctx names, if any.
func (e *Executor) submit(ctx context.Context, signed clobclient.SignedOrderV2, child plannedChild) (*clobclient.OrderResponse, error) {
	if member, ok := ctx.Value(placeMemberKey{}).(*placeMember); ok {
		if resp, err, joined := member.call(ctx, placeRequest{signed, child.TimeInForce, child.PostOnly}); joined {
			return resp, err
		}
	}
	return e.clob.SubmitSignedOrder(ctx, signed, child.TimeInForce, child.PostOnly)
}

// submitBatch posts reqs in one call. A single order goes through the single
// endpoint; one the exchange answers per order. A whole-call error is every
// order's: a 4xx means none was booked, anything else leaves each unknown.
func (e *Executor) submitBatch(ctx context.Context, reqs []placeRequest) ([]*clobclient.OrderResponse, []error) {
	resps := make([]*clobclient.OrderResponse, len(reqs))
	if len(reqs) == 1 || e.batchPlaceOff.Load() {
		return e.submitEach(ctx, reqs)
	}
	orders := make([]clobclient.BatchOrder, len(reqs))
	for i, req := range reqs {
		orders[i] = clobclient.BatchOrder{Signed: req.signed, OrderType: req.tif, PostOnly: req.postOnly}
	}
	out, err := e.clob.SubmitSignedOrders(ctx, orders)
	if batchUnsupported(err) {
		e.batchPlaceOff.Store(true)
		e.reportError(fmt.Errorf("batch order placement unsupported, posting one by one from now on: %w", err))
		return e.submitEach(ctx, reqs)
	}
	if err != nil {
		return resps, fillErrors(len(reqs), err)
	}
	errs := make([]error, len(reqs))
	for i := range reqs {
		resps[i] = &out[i]
		errs[i] = clobclient.BatchOrderError(out[i])
	}
	return resps, errs
}

func (e *Executor) submitEach(ctx context.Context, reqs []placeRequest) ([]*clobclient.OrderResponse, []error) {
	resps := make([]*clobclient.OrderResponse, len(reqs))
	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Go(func() {
			resps[i], errs[i] = e.clob.SubmitSignedOrder(ctx, req.signed, req.tif, req.postOnly)
		})
	}
	wg.Wait()
	return resps, errs
}

// ---- cancelling ----

type cancelRequest struct {
	orderID string
	timeout time.Duration
}

type cancelMember = batchMember[cancelRequest, struct{}]

type cancelMemberKey struct{}

func (e *Executor) newCancelMembers(n int) []*cancelMember {
	members := make([]*cancelMember, 0, n)
	for _, size := range chunkSizes(n) {
		b := newBatch(size, e.cancelBatch)
		for range size {
			members = append(members, b.member())
		}
	}
	return members
}

// cancelExchangeOrder cancels one exchange order, through the batch its ctx
// names, if any. A refusal is a *clobclient.CancelRefusedError either way.
func (e *Executor) cancelExchangeOrder(ctx context.Context, orderID string, timeout time.Duration) error {
	if member, ok := ctx.Value(cancelMemberKey{}).(*cancelMember); ok {
		if _, err, joined := member.call(ctx, cancelRequest{orderID, timeout}); joined {
			return err
		}
	}
	cancelCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	err := e.clob.CancelOrder(cancelCtx, orderID)
	e.observeCancel(1, time.Since(started), err)
	return err
}

// cancelBatch cancels reqs in one call, under the longest of their timeouts.
func (e *Executor) cancelBatch(ctx context.Context, reqs []cancelRequest) ([]struct{}, []error) {
	if len(reqs) == 1 || e.batchCancelOff.Load() {
		return nil, e.cancelEach(ctx, reqs)
	}
	ids := make([]string, len(reqs))
	var timeout time.Duration
	for i, req := range reqs {
		ids[i] = req.orderID
		timeout = max(timeout, req.timeout)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	out, err := e.clob.CancelOrdersResult(cancelCtx, ids)
	e.observeCancel(len(reqs), time.Since(started), err)
	if batchUnsupported(err) {
		e.batchCancelOff.Store(true)
		e.reportError(fmt.Errorf("batch cancel unsupported, cancelling one by one from now on: %w", err))
		return nil, e.cancelEach(ctx, reqs)
	}
	if err != nil {
		return nil, fillErrors(len(reqs), err)
	}
	errs := make([]error, len(reqs))
	for i, id := range ids {
		switch reason, refused := out.Refused(id); {
		case refused:
			errs[i] = &clobclient.CancelRefusedError{OrderID: id, Reason: reason}
		case !out.Has(id):
			// Neither canceled nor refused: retried like any failed cancel.
			errs[i] = fmt.Errorf("batch cancel answer does not mention order %s", id)
		}
	}
	return nil, errs
}

func (e *Executor) cancelEach(ctx context.Context, reqs []cancelRequest) []error {
	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Go(func() {
			cancelCtx, cancel := context.WithTimeout(ctx, req.timeout)
			defer cancel()
			started := time.Now()
			errs[i] = e.clob.CancelOrder(cancelCtx, req.orderID)
			e.observeCancel(1, time.Since(started), errs[i])
		})
	}
	wg.Wait()
	return errs
}

func (e *Executor) observeCancel(orders int, took time.Duration, err error) {
	if e.onCancel != nil {
		e.onCancel(orders, took, err)
	}
}
