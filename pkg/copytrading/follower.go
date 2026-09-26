// Package copytrading mirrors trades of listened Polymarket wallets onto the
// local signer, sizing every copy with a fixed USD amount.
package copytrading

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
)

// DefaultSubject is where pmm forwards the listened wallets' trades.
const DefaultSubject = "pmm.user.activity"

// Activity is one listened wallet's trade as forwarded by pmm.
type Activity struct {
	ConditionID string  `json:"condition_id"`
	EventSlug   string  `json:"event_slug"`
	AssetID     string  `json:"asset_id"`
	Address     string  `json:"address"`
	Outcome     string  `json:"outcome"`
	Side        string  `json:"side"`
	Price       float64 `json:"price"`
	Size        float64 `json:"size"`
	Timestamp   int64   `json:"timestamp"`   // unix ms
	ReceivedAt  int64   `json:"received_at"` // unix ms
}

// Trader is the subset of the CLOB client the follower places orders with.
type Trader interface {
	TickSize(ctx context.Context, tokenID string) (float64, error)
	MinOrderSize(ctx context.Context, tokenID string) (float64, error)
	SubmitOrder(ctx context.Context, order clobclient.UserOrder) (*clobclient.OrderResponse, error)
	BalanceAllowance(ctx context.Context, assetType, tokenID string) (*clobclient.BalanceAllowance, error)
}

// Config tunes the follower.
type Config struct {
	// USD is the fixed amount every copied trade is sized to.
	USD float64
	// InitialDiff is added to the upstream fill price to form the copy's buy
	// limit, so the copy can still cross a book that moved after the fill.
	InitialDiff float64
	// MaxTradeAge drops trades older than this when they arrive, so a late
	// message never chases a stale price.
	MaxTradeAge time.Duration
	DryRun      bool
	// CopyTimeout bounds one queued copy. It is not tied to shutdown, so an
	// order already being submitted is allowed to finish.
	CopyTimeout time.Duration
	// SettleWait is how long a sell waits for a recent copied buy on the
	// same token to show up in the balance; SettlePoll is how often it looks.
	SettleWait time.Duration
	SettlePoll time.Duration
	Logf       func(format string, args ...any)
	Now        func() time.Time
}

// Follower copies each activity message it is handed.
type Follower struct {
	cfg    Config
	trader Trader

	mu      sync.Mutex
	queues  map[string][]Activity // pending copies per token, in arrival order
	bought  map[string]recentBuy  // last matched copy buy per token
	closed  bool
	workers sync.WaitGroup
}

// recentBuy is a matched copy buy whose fill may not be in the balance yet.
type recentBuy struct {
	at     time.Time
	shares float64
}

const (
	// sellPrice is the floor limit every copied sell uses: a FAK at the
	// minimum price takes whatever bids exist, like executiond's FORCE_CLOSE.
	sellPrice   = 0.01
	maxBuyPrice = 0.99
	// dustUSD folds a leftover position worth less than this into the sell,
	// so a copied exit does not strand an unsellable remainder.
	dustUSD = 1.0
)

func New(cfg Config, trader Trader) (*Follower, error) {
	if !(cfg.USD > 0) {
		return nil, errors.New("a positive USD amount is required")
	}
	if cfg.InitialDiff < 0 || cfg.InitialDiff >= 1 || cfg.MaxTradeAge <= 0 {
		return nil, errors.New("initial diff must be in [0, 1) and max trade age positive")
	}
	if trader == nil {
		return nil, errors.New("trader is required")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CopyTimeout <= 0 {
		cfg.CopyTimeout = 30 * time.Second
	}
	if cfg.SettleWait <= 0 {
		cfg.SettleWait = 10 * time.Second
	}
	if cfg.SettlePoll <= 0 {
		cfg.SettlePoll = 500 * time.Millisecond
	}
	return &Follower{cfg: cfg, trader: trader, queues: map[string][]Activity{}, bought: map[string]recentBuy{}}, nil
}

// Enqueue copies activity in the background. Copies on the same token run one
// at a time in arrival order, so a quick buy-then-sell is never reordered;
// different tokens are copied concurrently. It reports false once the
// follower is closed.
func (f *Follower) Enqueue(activity Activity) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	key := activity.AssetID
	pending, running := f.queues[key]
	f.queues[key] = append(pending, activity)
	if !running {
		f.workers.Add(1)
		go f.drain(key)
	}
	return true
}

// Close stops accepting activities and waits for the queued copies to finish.
// Queued copies still run, so a sell queued behind an in-flight buy is not
// lost; the max trade age and copy timeout bound how long this takes.
func (f *Follower) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.workers.Wait()
}

func (f *Follower) drain(key string) {
	defer f.workers.Done()
	for {
		f.mu.Lock()
		pending := f.queues[key]
		if len(pending) == 0 {
			delete(f.queues, key)
			f.mu.Unlock()
			return
		}
		activity := pending[0]
		f.queues[key] = pending[1:]
		f.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), f.cfg.CopyTimeout)
		f.Handle(ctx, activity)
		cancel()
	}
}

func label(activity Activity) string {
	return fmt.Sprintf("%s %s %.4f %s @ %.4f %s", activity.Address, activity.Side, activity.Size, activity.Outcome, activity.Price, activity.EventSlug)
}

// Handle copies one activity. Every outcome, including a skip, is logged.
func (f *Follower) Handle(ctx context.Context, activity Activity) {
	label := label(activity)
	order, err := f.order(ctx, activity)
	if err != nil {
		f.cfg.Logf("skip %s: %v", label, err)
		return
	}
	action := fmt.Sprintf("%s %.4f shares @ %.2f FAK", order.Side, order.Shares, order.Price)
	if f.cfg.DryRun {
		f.cfg.Logf("dry run: would copy %s with %s", label, action)
		return
	}
	resp, err := f.trader.SubmitOrder(ctx, order)
	if err != nil {
		f.cfg.Logf("copy %s with %s failed: %v", label, action, err)
		return
	}
	f.recordFill(order, resp)
	f.cfg.Logf("copied %s with %s: order %s status %s", label, action, resp.OrderID, resp.Status)
}

func (f *Follower) order(ctx context.Context, activity Activity) (clobclient.UserOrder, error) {
	if activity.AssetID == "" {
		return clobclient.UserOrder{}, errors.New("asset id is not known yet")
	}
	if !(activity.Price > 0 && activity.Price < 1) {
		return clobclient.UserOrder{}, fmt.Errorf("invalid price %v", activity.Price)
	}
	if age := f.cfg.Now().Sub(time.UnixMilli(activity.Timestamp)); age > f.cfg.MaxTradeAge {
		return clobclient.UserOrder{}, fmt.Errorf("trade is %s old", age.Round(time.Millisecond))
	}
	switch activity.Side {
	case string(clobclient.SideBuy):
		return f.buyOrder(ctx, activity)
	case string(clobclient.SideSell):
		return f.sellOrder(ctx, activity)
	default:
		return clobclient.UserOrder{}, fmt.Errorf("unknown side %q", activity.Side)
	}
}

// buyOrder spends the fixed USD amount with a limit of the upstream price
// plus the initial diff, aligned down to the market tick.
func (f *Follower) buyOrder(ctx context.Context, activity Activity) (clobclient.UserOrder, error) {
	tick, err := f.trader.TickSize(ctx, activity.AssetID)
	if err != nil {
		return clobclient.UserOrder{}, fmt.Errorf("read tick size: %w", err)
	}
	price := math.Floor((activity.Price+f.cfg.InitialDiff)/tick+1e-9) * tick
	price = math.Min(price, maxBuyPrice)
	shares := floorTo(f.cfg.USD/price, clobclient.SharePrecisionDigits(clobclient.SideBuy, clobclient.OrderTypeFAK))
	if err := f.checkMinSize(ctx, activity.AssetID, shares); err != nil {
		return clobclient.UserOrder{}, err
	}
	return clobclient.UserOrder{TokenID: activity.AssetID, Side: clobclient.SideBuy, Price: price, Shares: shares, OrderType: clobclient.OrderTypeFAK}, nil
}

// sellOrder sells the fixed USD amount's worth of shares at the upstream
// price, capped by what the local wallet holds, at a 0.01 limit. After a
// recent copied buy on the token it waits for that fill to settle into the
// balance before capping.
func (f *Follower) sellOrder(ctx context.Context, activity Activity) (clobclient.UserOrder, error) {
	digits := clobclient.SharePrecisionDigits(clobclient.SideSell, clobclient.OrderTypeFAK)
	shares := floorTo(f.cfg.USD/activity.Price, digits)
	held, err := f.heldShares(ctx, activity.AssetID, digits)
	if err != nil {
		return clobclient.UserOrder{}, err
	}
	if wait := math.Min(shares, floorTo(f.recentBuyShares(activity.AssetID), digits)); held < wait {
		deadline := time.NewTimer(f.cfg.SettleWait)
		defer deadline.Stop()
		poll := time.NewTicker(f.cfg.SettlePoll)
		defer poll.Stop()
	settle:
		for held < wait {
			select {
			case <-ctx.Done():
				return clobclient.UserOrder{}, ctx.Err()
			case <-deadline.C:
				break settle
			case <-poll.C:
			}
			if held, err = f.heldShares(ctx, activity.AssetID, digits); err != nil {
				return clobclient.UserOrder{}, err
			}
		}
	}
	if held <= 0 {
		return clobclient.UserOrder{}, errors.New("no local position to sell")
	}
	if shares >= held || (held-shares)*activity.Price < dustUSD {
		shares = held
	}
	if err := f.checkMinSize(ctx, activity.AssetID, shares); err != nil {
		return clobclient.UserOrder{}, err
	}
	return clobclient.UserOrder{TokenID: activity.AssetID, Side: clobclient.SideSell, Price: sellPrice, Shares: shares, OrderType: clobclient.OrderTypeFAK}, nil
}

func (f *Follower) heldShares(ctx context.Context, tokenID string, digits int) (float64, error) {
	balance, err := f.trader.BalanceAllowance(ctx, "CONDITIONAL", tokenID)
	if err != nil {
		return 0, fmt.Errorf("read token balance: %w", err)
	}
	held, err := decimal.FromBaseUnits(balance.Balance)
	if err != nil {
		return 0, fmt.Errorf("token balance: %w", err)
	}
	return floorTo(held, digits), nil
}

// recordFill remembers the shares a copy buy matched, so a following sell on
// the token can wait for them to settle; a copy sell consumes that record.
// A matched BUY receives its shares as the taker amount.
func (f *Follower) recordFill(order clobclient.UserOrder, resp *clobclient.OrderResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if order.Side == clobclient.SideSell {
		delete(f.bought, order.TokenID)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(resp.Status), "matched") {
		return
	}
	if shares, err := strconv.ParseFloat(resp.TakingAmount, 64); err == nil && shares > 0 {
		f.bought[order.TokenID] = recentBuy{at: f.cfg.Now(), shares: shares}
	}
}

// recentBuyShares is the shares of a copy buy on the token matched within
// the settle window, or 0.
func (f *Follower) recentBuyShares(tokenID string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	buy, ok := f.bought[tokenID]
	if !ok || f.cfg.Now().Sub(buy.at) >= f.cfg.SettleWait {
		return 0
	}
	return buy.shares
}

func (f *Follower) checkMinSize(ctx context.Context, tokenID string, shares float64) error {
	minimum, err := f.trader.MinOrderSize(ctx, tokenID)
	if err != nil {
		return fmt.Errorf("read minimum order size: %w", err)
	}
	if shares <= 0 || shares+1e-9 < minimum {
		return fmt.Errorf("%.4f shares are below the minimum order size %.4f", shares, minimum)
	}
	return nil
}

func floorTo(value float64, digits int) float64 {
	scale := math.Pow10(digits)
	return math.Floor(value*scale+1e-9) / scale
}
