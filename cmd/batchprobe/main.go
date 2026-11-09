// Command batchprobe checks the exchange's batch endpoints against what
// pkg/executor assumes before they carry real ladders:
//
//  1. POST /orders with two post-only GTC BUYs: one at the best ask (it would
//     cross, so the exchange must refuse it) and one well under the best bid
//     (it rests). It prints the raw per-order answers: does the crossing
//     order come back as its own success=false entry, with wording that
//     isPostOnlyCross matches, while the other rests? Or does the whole call
//     fail?
//  2. DELETE /orders with the resting order's ID and one made-up ID. It prints
//     canceled and not_canceled, and whether the refusal reads as OrderGone.
//
// Each request's timing (ttfb, total) is printed from the client trace.
//
// It loads the signing key from the environment like postonlyprobe and posts
// real orders without a prompt. Nothing is meant to fill: one order is
// refused, the other rests below the bid and is cancelled at once. If any
// step goes wrong it prints the order IDs to cancel by hand.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "batchprobe: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var tokenID string
	var shares, below float64
	flag.StringVar(&tokenID, "token-id", "", "outcome token to BUY")
	flag.Float64Var(&shares, "shares", 0, "shares per order; 0 sizes it to max(minimum shares, $1.50 notional at the ask)")
	flag.Float64Var(&below, "below", 0.05, "how far under the best bid the resting order sits")
	flag.Parse()
	if tokenID == "" {
		return errors.New("--token-id is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := clobclient.ConfigFromEnv()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.PrivateKey) == "" {
		return errors.New("a signing key is required: set POLYMARKET_PRIVATE_KEY_FILE (plus POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE) or POLYMARKET_PRIVATE_KEY")
	}
	clob, err := clobclient.New(cfg)
	if err != nil {
		return fmt.Errorf("create CLOB client: %w", err)
	}
	if _, err := clob.EnsureCredentials(ctx); err != nil {
		return err
	}
	clob.SetRequestTraceHook(func(t transport.Timing) {
		status := "ok"
		if t.Err != nil {
			status = t.Err.Error()
		}
		fmt.Printf("  http %s %s reused=%t ttfb=%s total=%s status=%s\n", t.Method, t.Path, t.Reused, t.FirstByte, t.Total, status)
	})

	book, err := clob.OrderBook(ctx, tokenID)
	if err != nil {
		return fmt.Errorf("read book: %w", err)
	}
	ask, bid := bestPrices(book)
	if math.IsInf(ask, 1) || ask >= 1 {
		return fmt.Errorf("abort: no usable best ask (%g)", ask)
	}
	tick, err := clob.TickSize(ctx, tokenID)
	if err != nil {
		return fmt.Errorf("read tick size: %w", err)
	}
	rest := math.Floor((bid-below)/tick) * tick
	if bid <= 0 || rest < tick {
		return fmt.Errorf("abort: best bid %g leaves no resting price %g under it", bid, below)
	}
	if shares == 0 {
		minSize, err := clob.MinOrderSize(ctx, tokenID)
		if err != nil {
			return fmt.Errorf("read minimum order size: %w", err)
		}
		shares = math.Max(minSize, math.Ceil(1.5/ask*100)/100)
	}

	var orders []clobclient.BatchOrder
	for _, price := range []float64{ask, rest} {
		signed, err := clob.CreateOrder(ctx, clobclient.UserOrder{TokenID: tokenID, Side: clobclient.SideBuy, Price: price, Shares: shares, PostOnly: true, OrderType: clobclient.OrderTypeGTC})
		if err != nil {
			return fmt.Errorf("sign order at %g: %w", price, err)
		}
		orders = append(orders, clobclient.BatchOrder{Signed: signed, OrderType: clobclient.OrderTypeGTC, PostOnly: true})
	}
	fmt.Printf("1. POST /orders: post-only BUY %g @ %g (best ask, crosses) and @ %g (rests; bid %g)\n", shares, ask, rest, bid)
	out, err := clob.SubmitSignedOrders(ctx, orders)
	if err != nil {
		describe("whole batch failed", err)
		if len(out) > 0 {
			fmt.Printf("   partial answer: %+v\n", out)
		}
		return nil
	}
	var resting []string
	for i, resp := range out {
		label := []string{"crossing", "resting"}[i]
		fmt.Printf("   [%s] success=%t status=%q orderID=%q errorMsg=%q\n", label, resp.Success, resp.Status, resp.OrderID, resp.ErrorMsg)
		refused := clobclient.BatchOrderError(resp) != nil
		if !refused && resp.OrderID != "" {
			resting = append(resting, resp.OrderID)
		}
		if refused {
			fmt.Printf("   [%s] isPostOnlyCross rules match: %t\n", label, wouldMatch(resp.ErrorMsg))
		}
	}
	if clobclient.BatchOrderError(out[0]) == nil {
		fmt.Println("   UNEXPECTED: the exchange accepted a crossing post-only order in a batch")
	}

	unknown := "0x" + strings.Repeat("0", 63) + "1"
	ids := append(resting, unknown)
	fmt.Printf("2. DELETE /orders: %v (the last is made up)\n", ids)
	result, err := clob.CancelOrdersResult(ctx, ids)
	if err != nil {
		describe("batch cancel failed", err)
		return fmt.Errorf("cancel %v by hand", resting)
	}
	fmt.Printf("   canceled=%v\n", result.Canceled)
	for id, reason := range result.NotCanceled {
		fmt.Printf("   not_canceled %s: %q (OrderGone=%t)\n", id, reason, (&clobclient.CancelRefusedError{Reason: reason}).OrderGone())
	}
	for _, id := range resting {
		if !result.Has(id) {
			fmt.Printf("   WARNING: %s was not cancelled; cancel it by hand\n", id)
		}
	}
	if result.Has(unknown) {
		fmt.Println("   UNEXPECTED: a made-up order ID was reported cancelled")
	}
	if _, refused := result.Refused(unknown); !refused && !result.Has(unknown) {
		fmt.Println("   NOTE: the made-up ID is in neither list; the executor retries such an order next pass")
	}
	return nil
}

func bestPrices(book *clobclient.OrderBook) (ask, bid float64) {
	ask = math.Inf(1)
	for _, level := range book.Asks {
		if p, err := strconv.ParseFloat(level.Price, 64); err == nil && p > 0 && p < ask {
			ask = p
		}
	}
	for _, level := range book.Bids {
		if p, err := strconv.ParseFloat(level.Price, 64); err == nil && p > bid {
			bid = p
		}
	}
	return ask, bid
}

func describe(what string, err error) {
	var apiErr *clobclient.APIError
	if errors.As(err, &apiErr) {
		fmt.Printf("   %s: HTTP %d body=%q\n", what, apiErr.StatusCode, string(apiErr.Body))
		return
	}
	fmt.Printf("   %s: %T %q\n", what, err, err.Error())
}

// wouldMatch mirrors pkg/executor.isPostOnlyCross, which is unexported.
func wouldMatch(reason string) bool {
	value := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(reason))
	return strings.Contains(value, "post only") && (strings.Contains(value, "cross") || strings.Contains(value, "marketable"))
}
