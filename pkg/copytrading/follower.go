// Package copytrading mirrors trades of listened Polymarket wallets onto the
// local signer, sizing every copy with a fixed USD amount.
package copytrading

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
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
	Logf        func(format string, args ...any)
	Now         func() time.Time
}

// Follower copies each activity message it is handed.
type Follower struct {
	cfg    Config
	trader Trader
}

const (
	// sellPrice is the floor limit every copied sell uses: a FAK at the
	// minimum price takes whatever bids exist, like executiond's FORCE_CLOSE.
	sellPrice       = 0.01
	maxBuyPrice     = 0.99
	conditionalUnit = 1e6 // conditional token balances are reported in 6-decimal base units
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
	return &Follower{cfg: cfg, trader: trader}, nil
}

// Handle copies one activity. Every outcome, including a skip, is logged.
func (f *Follower) Handle(ctx context.Context, activity Activity) {
	label := fmt.Sprintf("%s %s %.4f %s @ %.4f %s", activity.Address, activity.Side, activity.Size, activity.Outcome, activity.Price, activity.EventSlug)
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
// price, capped by what the local wallet holds, at a 0.01 limit.
func (f *Follower) sellOrder(ctx context.Context, activity Activity) (clobclient.UserOrder, error) {
	balance, err := f.trader.BalanceAllowance(ctx, "CONDITIONAL", activity.AssetID)
	if err != nil {
		return clobclient.UserOrder{}, fmt.Errorf("read token balance: %w", err)
	}
	raw, err := strconv.ParseFloat(balance.Balance, 64)
	if err != nil {
		return clobclient.UserOrder{}, fmt.Errorf("parse token balance %q: %w", balance.Balance, err)
	}
	digits := clobclient.SharePrecisionDigits(clobclient.SideSell, clobclient.OrderTypeFAK)
	held := floorTo(raw/conditionalUnit, digits)
	if held <= 0 {
		return clobclient.UserOrder{}, errors.New("no local position to sell")
	}
	shares := floorTo(f.cfg.USD/activity.Price, digits)
	if shares >= held || (held-shares)*activity.Price < dustUSD {
		shares = held
	}
	if err := f.checkMinSize(ctx, activity.AssetID, shares); err != nil {
		return clobclient.UserOrder{}, err
	}
	return clobclient.UserOrder{TokenID: activity.AssetID, Side: clobclient.SideSell, Price: sellPrice, Shares: shares, OrderType: clobclient.OrderTypeFAK}, nil
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
