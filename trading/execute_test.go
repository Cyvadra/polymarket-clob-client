package trading

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type orderScript struct {
	reads       []*clobclient.Order
	cancelFinal *clobclient.Order
	index       int
	canceled    bool
}

type fakeBroker struct {
	mu      sync.Mutex
	scripts []*orderScript
	orders  map[string]*orderScript
	submits []clobclient.UserOrder
	err     error
}

type temporaryError struct{}

func (temporaryError) Error() string   { return "temporary network failure" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

type retryBroker struct {
	fakeBroker
	attempts int
}

type cancelErrorBroker struct{ fakeBroker }

func (f *cancelErrorBroker) CancelOrder(ctx context.Context, id string) error {
	if err := f.fakeBroker.CancelOrder(ctx, id); err != nil {
		return err
	}
	return temporaryError{}
}

func (f *retryBroker) SubmitOrder(ctx context.Context, order clobclient.UserOrder) (*clobclient.OrderResponse, error) {
	f.attempts++
	if f.attempts == 1 {
		return nil, temporaryError{}
	}
	return f.fakeBroker.SubmitOrder(ctx, order)
}

func (f *fakeBroker) SubmitOrder(_ context.Context, order clobclient.UserOrder) (*clobclient.OrderResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	id := fmt.Sprintf("order-%d", len(f.submits)+1)
	f.submits = append(f.submits, order)
	if f.orders == nil {
		f.orders = map[string]*orderScript{}
	}
	f.orders[id] = f.scripts[len(f.submits)-1]
	return &clobclient.OrderResponse{Success: true, OrderID: id}, nil
}

func (f *fakeBroker) Order(_ context.Context, id string) (*clobclient.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.orders[id]
	if s.canceled && s.cancelFinal != nil {
		copy := *s.cancelFinal
		return &copy, nil
	}
	i := s.index
	if i >= len(s.reads) {
		i = len(s.reads) - 1
	} else {
		s.index++
	}
	copy := *s.reads[i]
	return &copy, nil
}

func (f *fakeBroker) CancelOrder(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders[id].canceled = true
	return nil
}

func TestExecuteFullFill(t *testing.T) {
	f := &fakeBroker{scripts: []*orderScript{{reads: []*clobclient.Order{
		{Status: "LIVE", SizeMatched: "2", AvgPrice: "0.40"},
		{Status: "MATCHED", SizeMatched: "5", AvgPrice: "0.42"},
	}}}}
	r, err := New(f).EstablishPosition(context.Background(), PositionRequest{TokenID: "token", Side: clobclient.SideBuy, TargetShares: 5, LimitPrice: .42, CompleteWithin: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Completed || r.FilledShares != 5 || r.AveragePrice < .42-1e-9 || r.AveragePrice > .42+1e-9 {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestExecutePartialFillThenRequotesRemainder(t *testing.T) {
	f := &fakeBroker{scripts: []*orderScript{
		{reads: []*clobclient.Order{{Status: "LIVE", SizeMatched: "2", AvgPrice: "0.40"}}, cancelFinal: &clobclient.Order{Status: "CANCELED", SizeMatched: "2", AvgPrice: "0.40"}},
		{reads: []*clobclient.Order{{Status: "MATCHED", SizeMatched: "3", AvgPrice: "0.42"}}},
	}}
	r, err := New(f).EstablishPosition(context.Background(), PositionRequest{TokenID: "token", Side: clobclient.SideBuy, TargetShares: 5, LimitPrice: .42, CompleteWithin: time.Second, RequoteEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Completed || len(f.submits) != 2 || f.submits[1].Shares != 3 {
		t.Fatalf("unexpected result/submits: %+v %+v", r, f.submits)
	}
	if diff := r.AveragePrice - .412; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("unexpected VWAP %.8f", r.AveragePrice)
	}
}

func TestCancellationRaceIncludesLateFill(t *testing.T) {
	f := &fakeBroker{scripts: []*orderScript{{reads: []*clobclient.Order{{Status: "LIVE", SizeMatched: "1", AvgPrice: "0.40"}}, cancelFinal: &clobclient.Order{Status: "CANCELED", SizeMatched: "2.5", AvgPrice: "0.41"}}}}
	r, err := New(f).PlaceLimitFor(context.Background(), LimitRequest{TokenID: "token", Side: clobclient.SideBuy, Shares: 5, Price: .42, ValidFor: 5 * time.Millisecond, PollInterval: time.Millisecond})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expiry, got %v", err)
	}
	if r.FilledShares != 2.5 || len(r.Orders) != 1 || !r.Orders[0].CancelRequested {
		t.Fatalf("late fill lost: %+v", r)
	}
}

func TestContextCancellationCancelsLiveOrder(t *testing.T) {
	f := &fakeBroker{scripts: []*orderScript{{reads: []*clobclient.Order{{Status: "LIVE"}}, cancelFinal: &clobclient.Order{Status: "CANCELED"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(3*time.Millisecond, cancel)
	r, err := New(f).PlaceLimitFor(ctx, LimitRequest{TokenID: "token", Side: clobclient.SideSell, Shares: 2, Price: .5, ValidFor: time.Second, PollInterval: time.Millisecond})
	if !errors.Is(err, context.Canceled) || !r.Canceled {
		t.Fatalf("expected canceled result: err=%v result=%+v", err, r)
	}
}

func TestDeadlineDuringPollStillCancelsLiveOrder(t *testing.T) {
	f := &fakeBroker{scripts: []*orderScript{{reads: []*clobclient.Order{{Status: "LIVE"}}, cancelFinal: &clobclient.Order{Status: "CANCELED"}}}}
	r, err := New(f).PlaceLimitFor(context.Background(), LimitRequest{TokenID: "token", Side: clobclient.SideSell, Shares: 2, Price: .5, ValidFor: time.Millisecond, PollInterval: time.Second})
	if !errors.Is(err, ErrExpired) || !r.Canceled {
		t.Fatalf("expected deadline cleanup: err=%v result=%+v", err, r)
	}
}

func TestSubmissionRejectionIsNotBlindlyRetried(t *testing.T) {
	want := errors.New("order rejected")
	f := &fakeBroker{err: want}
	r, err := New(f).EstablishPosition(context.Background(), PositionRequest{TokenID: "token", Side: clobclient.SideBuy, TargetShares: 2, LimitPrice: .4, CompleteWithin: time.Second, RequoteEvery: time.Millisecond})
	if !errors.Is(err, want) || len(r.Orders) != 1 || len(f.submits) != 0 {
		t.Fatalf("unexpected retry behavior: err=%v result=%+v", err, r)
	}
}

func TestTemporarySubmissionFailureIsNotRetried(t *testing.T) {
	f := &retryBroker{fakeBroker: fakeBroker{scripts: []*orderScript{{reads: []*clobclient.Order{{Status: "MATCHED", SizeMatched: "2", AvgPrice: "0.4"}}}}}}
	var retries int
	r, err := New(f).Execute(context.Background(), Request{
		TokenID: "token", Side: clobclient.SideBuy, TargetShares: 2, LimitPrice: .4,
		CompleteWithin: time.Second, RetryDelay: time.Millisecond, MaxAttempts: 2,
		OnEvent: func(event Event) {
			if event.Type == EventRetrying {
				retries++
			}
		},
	})
	if err == nil || r.Completed || f.attempts != 1 || retries != 0 {
		t.Fatalf("unexpected retry result: result=%+v err=%v attempts=%d retries=%d", r, err, f.attempts, retries)
	}
	if len(r.Orders) != 1 || r.Orders[0].Error == nil {
		t.Fatalf("failed attempt was not retained: %+v", r.Orders)
	}
}

func TestResolvedCancellationOverridesTransientCancelError(t *testing.T) {
	f := &cancelErrorBroker{fakeBroker: fakeBroker{scripts: []*orderScript{{
		reads:       []*clobclient.Order{{Status: "LIVE"}},
		cancelFinal: &clobclient.Order{Status: "CANCELED", SizeMatched: "1", AvgPrice: ".4"},
	}}}}
	r, err := New(f).Execute(context.Background(), Request{
		TokenID: "token", Side: clobclient.SideBuy, TargetShares: 2, LimitPrice: .4,
		CompleteWithin: time.Second, RequoteEvery: time.Millisecond, PartialFill: KeepAndStop,
	})
	if err != nil || r.FilledShares != 1 || !r.Canceled {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestQuoteCannotViolateLimit(t *testing.T) {
	f := &fakeBroker{}
	_, err := New(f).EstablishPosition(context.Background(), PositionRequest{TokenID: "token", Side: clobclient.SideBuy, TargetShares: 2, LimitPrice: .4, CompleteWithin: time.Second, Quote: func(context.Context, QuoteInput) (float64, error) { return .41, nil }})
	if err == nil {
		t.Fatal("expected limit validation error")
	}
}
