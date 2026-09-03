package trading

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

const fillEpsilon = 1e-8

func (t *Trader) Execute(ctx context.Context, req Request) (result Result, retErr error) {
	if err := validate(t.Broker, req); err != nil {
		return result, err
	}
	defaults(&req)
	if req.Side == clobclient.SideSell && req.PositionGuard != nil {
		if !req.PositionGuard.ReserveSell(req.TokenID, req.TargetShares) {
			return result, fmt.Errorf("insufficient sellable shares")
		}
		defer func() {
			if result.FilledShares > 0 {
				req.PositionGuard.ConsumeSell(req.TokenID, result.FilledShares)
			}
			if result.RemainingShares > fillEpsilon {
				req.PositionGuard.ReleaseSell(req.TokenID, result.RemainingShares)
			}
		}()
	}
	result = Result{TokenID: req.TokenID, Side: req.Side, RequestedShares: req.TargetShares,
		RemainingShares: req.TargetShares, StartedAt: time.Now()}
	deadlineCtx, cancel := context.WithTimeout(ctx, req.CompleteWithin)
	defer cancel()

	var totalCost float64
	for attempt := 1; result.RemainingShares > fillEpsilon; attempt++ {
		if req.MaxAttempts > 0 && attempt > req.MaxAttempts {
			retErr = ErrIncomplete
			break
		}
		if result.RemainingShares < req.MinShares {
			retErr = ErrRemainderTooSmall
			break
		}
		price, err := quote(deadlineCtx, req, result, attempt)
		if err != nil {
			retErr = err
			break
		}
		child := ChildOrder{Attempt: attempt, Price: price, RequestedShares: result.RemainingShares, SubmittedAt: time.Now()}
		response, err := t.Broker.SubmitOrder(deadlineCtx, clobclient.UserOrder{TokenID: req.TokenID, Side: req.Side,
			Price: price, Shares: child.RequestedShares, OrderType: req.OrderType, PostOnly: req.PostOnly})
		if err != nil {
			child.Error, child.FinishedAt = err, time.Now()
			result.Orders = append(result.Orders, child)
			retErr = err
			break
		}
		child.OrderID = response.OrderID
		emit(req, Event{Type: EventSubmitted, At: time.Now(), Attempt: attempt, OrderID: child.OrderID, Price: price, Remaining: result.RemainingShares})
		filled, cost, status, canceled, watchErr := t.watchChild(deadlineCtx, req, &child)
		child.FilledShares, child.Status, child.FinishedAt = filled, status, time.Now()
		if filled > 0 {
			child.AveragePrice = cost / filled
			result.FilledShares += filled
			totalCost += cost
		}
		result.Orders = append(result.Orders, child)
		result.RemainingShares = math.Max(0, req.TargetShares-result.FilledShares)
		result.Canceled = result.Canceled || canceled
		if watchErr != nil {
			if errors.Is(watchErr, context.DeadlineExceeded) || errors.Is(watchErr, context.Canceled) {
				result.Expired = errors.Is(deadlineCtx.Err(), context.DeadlineExceeded)
				if ctx.Err() != nil {
					retErr = ctx.Err()
				} else {
					retErr = ErrExpired
				}
			} else {
				retErr = watchErr
			}
			break
		}
		if result.RemainingShares <= fillEpsilon {
			break
		}
		if req.PartialFill == KeepAndStop {
			break
		}
		if req.PartialFill == RequireComplete {
			retErr = ErrIncomplete
			break
		}
	}
	result.Completed = result.RemainingShares <= fillEpsilon
	if result.FilledShares > 0 {
		result.AveragePrice = totalCost / result.FilledShares
	}
	result.FinishedAt = time.Now()
	if !result.Completed && retErr == nil && req.PartialFill == RequireComplete {
		retErr = ErrIncomplete
	}
	return result, retErr
}

func (t *Trader) watchChild(ctx context.Context, req Request, child *ChildOrder) (filled, cost float64, status string, canceled bool, retErr error) {
	started := time.Now()
	lastFilled := 0.0
	for {
		shouldCancel := req.RequoteEvery > 0 && time.Since(started) >= req.RequoteEvery
		if ctx.Err() != nil || shouldCancel {
			canceled = true
			child.CancelRequested = true
			emit(req, Event{Type: EventCanceling, At: time.Now(), Attempt: child.Attempt, OrderID: child.OrderID, Filled: filled})
			cleanupCtx, cancel := context.WithTimeout(context.Background(), req.CancelTimeout)
			cancelErr := t.Broker.CancelOrder(cleanupCtx, child.OrderID)
			// Always query once after cancellation: the order may have filled while cancellation raced.
			resolved := false
			if final, queryErr := t.Broker.Order(cleanupCtx, child.OrderID); queryErr == nil {
				exec, parseErr := clobclient.ExecutionFromOrder(child.OrderID, final, child.RequestedShares, time.Now())
				if parseErr != nil {
					cancel()
					return filled, cost, status, canceled, parseErr
				}
				status = string(exec.Status)
				current := exec.MatchedShares
				if current > child.RequestedShares {
					current = child.RequestedShares
				}
				if current > lastFilled {
					delta := current - lastFilled
					avg := exec.AveragePrice
					if avg <= 0 {
						avg = child.Price
						cost += delta * avg
					} else {
						cost = current * avg
					}
					filled += delta
				}
				resolved = exec.Terminal
			}
			cancel()
			if cancelErr != nil && !resolved {
				return filled, cost, status, canceled, cancelErr
			}
			if ctx.Err() != nil {
				return filled, cost, status, canceled, ctx.Err()
			}
			return filled, cost, status, canceled, nil
		}
		order, err := t.Broker.Order(ctx, child.OrderID)
		if err != nil {
			return filled, cost, status, canceled, err
		}
		exec, err := clobclient.ExecutionFromOrder(child.OrderID, order, child.RequestedShares, time.Now())
		if err != nil {
			return filled, cost, status, canceled, err
		}
		status = string(exec.Status)
		current := exec.MatchedShares
		if current > child.RequestedShares {
			current = child.RequestedShares
		}
		if current > lastFilled {
			delta := current - lastFilled
			avg := exec.AveragePrice
			if avg <= 0 {
				avg = child.Price
				cost += delta * avg
			} else {
				// avg_price is cumulative for this child order, so replace the
				// cumulative cost snapshot instead of pricing only the delta.
				cost = current * avg
			}
			filled += delta
			lastFilled = current
			emit(req, Event{Type: EventFill, At: time.Now(), Attempt: child.Attempt, OrderID: child.OrderID, Price: avg, Filled: filled})
		}
		if exec.Terminal || filled >= child.RequestedShares-fillEpsilon {
			emit(req, Event{Type: EventTerminal, At: time.Now(), Attempt: child.Attempt, OrderID: child.OrderID, Filled: filled})
			return filled, cost, status, canceled, nil
		}
		if !wait(ctx, req.PollInterval) {
			continue
		}
	}
}

func validate(b Broker, r Request) error {
	if b == nil {
		return fmt.Errorf("broker is required")
	}
	if r.TokenID == "" {
		return fmt.Errorf("token ID is required")
	}
	if r.Side != clobclient.SideBuy && r.Side != clobclient.SideSell {
		return fmt.Errorf("side must be BUY or SELL")
	}
	if !finitePositive(r.TargetShares) || !finitePositive(r.LimitPrice) || r.LimitPrice >= 1 {
		return fmt.Errorf("positive shares and a price between 0 and 1 are required")
	}
	if r.CompleteWithin <= 0 {
		return fmt.Errorf("complete within must be positive")
	}
	return nil
}

func defaults(r *Request) {
	if r.OrderType == "" {
		r.OrderType = clobclient.OrderTypeGTC
	}
	if r.PollInterval <= 0 {
		r.PollInterval = 250 * time.Millisecond
	}
	if r.CancelTimeout <= 0 {
		r.CancelTimeout = 3 * time.Second
	}
	if r.RetryDelay <= 0 {
		r.RetryDelay = 100 * time.Millisecond
	}
}

func quote(ctx context.Context, r Request, result Result, attempt int) (float64, error) {
	p := r.LimitPrice
	if r.Quote != nil {
		var err error
		p, err = r.Quote(ctx, QuoteInput{TokenID: r.TokenID, Side: r.Side, LimitPrice: r.LimitPrice, FilledShares: result.FilledShares, Remaining: result.RemainingShares, Attempt: attempt, PreviousPrice: lastPrice(result)})
		if err != nil {
			return 0, err
		}
	}
	if !finitePositive(p) || p >= 1 {
		return 0, fmt.Errorf("quote returned invalid price %.8f", p)
	}
	if r.Side == clobclient.SideBuy && p > r.LimitPrice+fillEpsilon {
		return 0, fmt.Errorf("buy quote %.8f exceeds limit %.8f", p, r.LimitPrice)
	}
	if r.Side == clobclient.SideSell && p < r.LimitPrice-fillEpsilon {
		return 0, fmt.Errorf("sell quote %.8f is below limit %.8f", p, r.LimitPrice)
	}
	return p, nil
}

func finitePositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func lastPrice(r Result) float64 {
	if len(r.Orders) == 0 {
		return 0
	}
	return r.Orders[len(r.Orders)-1].Price
}
func emit(r Request, e Event) {
	if r.OnEvent != nil {
		r.OnEvent(e)
	}
}
