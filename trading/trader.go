package trading

import (
	"context"
	"fmt"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type Trader struct{ Broker Broker }

func New(broker Broker) *Trader { return &Trader{Broker: broker} }

// PlaceLimitFor places one fixed-price order, watches partial fills, and
// cancels any remainder when ValidFor elapses.
func (t *Trader) PlaceLimitFor(ctx context.Context, req LimitRequest) (Result, error) {
	return t.Execute(ctx, Request{
		TokenID: req.TokenID, Side: req.Side, TargetShares: req.Shares,
		LimitPrice: req.Price, CompleteWithin: req.ValidFor, OrderType: req.OrderType,
		PostOnly: req.PostOnly, PollInterval: req.PollInterval,
		PartialFill: KeepAndStop, MaxAttempts: 1, OnEvent: req.OnEvent,
	})
}

// EstablishPosition repeatedly quotes only the unfilled remainder until the
// requested position is established or its deadline is reached.
func (t *Trader) EstablishPosition(ctx context.Context, req PositionRequest) (Result, error) {
	return t.Execute(ctx, Request{
		TokenID: req.TokenID, Side: req.Side, TargetShares: req.TargetShares,
		LimitPrice: req.LimitPrice, CompleteWithin: req.CompleteWithin,
		RequoteEvery: req.RequoteEvery, PostOnly: req.PostOnly, Quote: req.Quote,
		PartialFill: KeepAndRequote, OnEvent: req.OnEvent,
	})
}

// EstablishPositionWithGuard runs a position workflow while reserving sellable
// inventory. Callers may use it for both buys and sells; the guard is only used
// for sells.
func (t *Trader) EstablishPositionWithGuard(ctx context.Context, req PositionRequest, guard PositionGuard) (Result, error) {
	return t.Execute(ctx, Request{
		TokenID: req.TokenID, Side: req.Side, TargetShares: req.TargetShares,
		LimitPrice: req.LimitPrice, CompleteWithin: req.CompleteWithin,
		RequoteEvery: req.RequoteEvery, PostOnly: req.PostOnly, Quote: req.Quote,
		PartialFill: KeepAndRequote, OnEvent: req.OnEvent, PositionGuard: guard,
	})
}

// ClosePosition is an explicit sell workflow. Shares should be the currently
// sellable quantity (or the smaller portion the caller intends to close).
func (t *Trader) ClosePosition(ctx context.Context, req CloseRequest) (Result, error) {
	return t.ClosePositionWithGuard(ctx, req, nil)
}

func (t *Trader) ClosePositionWithGuard(ctx context.Context, req CloseRequest, guard PositionGuard) (Result, error) {
	if req.Shares <= 0 {
		return Result{}, fmt.Errorf("close shares must be positive")
	}
	return t.EstablishPositionWithGuard(ctx, PositionRequest{
		TokenID: req.TokenID, Side: clobclient.SideSell, TargetShares: req.Shares,
		LimitPrice: req.LimitPrice, CompleteWithin: req.CompleteWithin,
		RequoteEvery: req.RequoteEvery, PostOnly: req.PostOnly, Quote: req.Quote,
		OnEvent: req.OnEvent,
	}, guard)
}
