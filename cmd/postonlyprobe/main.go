// Command postonlyprobe records the exchange's real wording for a post-only
// order that would cross the book, so pkg/executor.isPostOnlyCross can be
// checked against it. It posts one GTC post-only BUY priced at the best ask
// (it would match, so the exchange must refuse it) and prints the raw
// rejection: the Go error type, the message, the HTTP body if the refusal
// came as a non-200, and whether isPostOnlyCross's rules match it.
//
// A post-only order cannot take liquidity, so no fill is expected. If the
// exchange nevertheless accepts the order, the probe cancels it at once and
// says so.
//
// It loads the signing key from the environment (POLYMARKET_PRIVATE_KEY or
// POLYMARKET_PRIVATE_KEY_FILE, POLYMARKET_MAKER_ADDRESS,
// POLYMARKET_SIGNATURE_TYPE) and submits a real order without a prompt.
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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "postonlyprobe: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var tokenID string
	var shares float64
	flag.StringVar(&tokenID, "token-id", "", "outcome token to BUY")
	flag.Float64Var(&shares, "shares", 0, "shares; 0 sizes it to max(minimum shares, $1.50 notional)")
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

	book, err := clob.OrderBook(ctx, tokenID)
	if err != nil {
		return fmt.Errorf("read book: %w", err)
	}
	best := math.Inf(1)
	for _, level := range book.Asks {
		if p, err := strconv.ParseFloat(level.Price, 64); err == nil && p > 0 && p < best {
			best = p
		}
	}
	if math.IsInf(best, 1) {
		return errors.New("abort: the book has no asks to cross")
	}
	if best >= 1 {
		return fmt.Errorf("abort: best ask %g is not a valid buy price", best)
	}
	if shares == 0 {
		minSize, err := clob.MinOrderSize(ctx, tokenID)
		if err != nil {
			return fmt.Errorf("read minimum order size: %w", err)
		}
		shares = math.Max(minSize, math.Ceil(1.5/best*100)/100)
	}
	fmt.Printf("posting post-only GTC BUY %g @ %g (the best ask, so it crosses)\n", shares, best)

	resp, err := clob.SubmitOrder(ctx, clobclient.UserOrder{
		TokenID:   tokenID,
		Side:      clobclient.SideBuy,
		Price:     best,
		Shares:    shares,
		PostOnly:  true,
		OrderType: clobclient.OrderTypeGTC,
	})
	if resp != nil {
		fmt.Printf("response: success=%t status=%q orderID=%q errorMsg=%q\n", resp.Success, resp.Status, resp.OrderID, resp.ErrorMsg)
	}
	if err == nil {
		fmt.Println("UNEXPECTED: the exchange accepted a crossing post-only order")
		if resp != nil && resp.OrderID != "" {
			if cerr := clob.CancelOrder(ctx, resp.OrderID); cerr != nil {
				return fmt.Errorf("cancel %s: %w (cancel it by hand)", resp.OrderID, cerr)
			}
			fmt.Printf("canceled %s\n", resp.OrderID)
		}
		return nil
	}

	var rejected *clobclient.OrderRejectedError
	var apiErr *clobclient.APIError
	switch {
	case errors.As(err, &rejected):
		fmt.Printf("error type: *OrderRejectedError\nmessage: %q\n", rejected.Message)
	case errors.As(err, &apiErr):
		fmt.Printf("error type: *APIError (HTTP %d)\nbody: %q\n", apiErr.StatusCode, string(apiErr.Body))
	default:
		fmt.Printf("error type: %T\nerror: %q\n", err, err.Error())
	}
	fmt.Printf("isPostOnlyCross rules match: %t\n", wouldMatch(err.Error()))
	return nil
}

// wouldMatch mirrors pkg/executor.isPostOnlyCross, which is unexported.
func wouldMatch(reason string) bool {
	value := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(reason))
	return strings.Contains(value, "post only") && (strings.Contains(value, "cross") || strings.Contains(value, "marketable"))
}
