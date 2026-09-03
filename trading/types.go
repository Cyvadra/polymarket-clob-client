// Package trading provides deadline-bound, partial-fill-aware trading workflows.
package trading

import (
	"context"
	"errors"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

var (
	ErrExpired           = errors.New("trading instruction expired")
	ErrIncomplete        = errors.New("trading instruction was not fully filled")
	ErrRemainderTooSmall = errors.New("remaining order is below the minimum shares")
)

type Broker interface {
	SubmitOrder(context.Context, clobclient.UserOrder) (*clobclient.OrderResponse, error)
	Order(context.Context, string) (*clobclient.Order, error)
	CancelOrder(context.Context, string) error
}

// PositionGuard prevents concurrent sell workflows from using the same shares.
// A nil guard leaves position management to the caller.
type PositionGuard interface {
	ReserveSell(tokenID string, shares float64) bool
	ConsumeSell(tokenID string, shares float64)
	ReleaseSell(tokenID string, shares float64)
}

// QuoteFunc returns the price for the next child order. It must respect the
// request limit; Execute validates this before submitting the order.
type QuoteFunc func(context.Context, QuoteInput) (float64, error)

type QuoteInput struct {
	TokenID       string
	Side          clobclient.Side
	LimitPrice    float64
	FilledShares  float64
	Remaining     float64
	Attempt       int
	PreviousPrice float64
}

type PartialFillPolicy int

const (
	// KeepAndRequote retains fills and submits the remaining quantity again.
	KeepAndRequote PartialFillPolicy = iota
	// KeepAndStop returns after the first child order becomes terminal.
	KeepAndStop
	// RequireComplete returns ErrIncomplete when the target is not reached.
	RequireComplete
)

type Request struct {
	TokenID        string
	Side           clobclient.Side
	TargetShares   float64
	LimitPrice     float64
	CompleteWithin time.Duration
	OrderType      clobclient.OrderType
	PostOnly       bool

	// RequoteEvery cancels a live child order and quotes the remaining shares.
	// Zero leaves the child live until the overall deadline.
	RequoteEvery  time.Duration
	PollInterval  time.Duration
	CancelTimeout time.Duration
	RetryDelay    time.Duration
	MaxAttempts   int
	MinShares     float64
	PartialFill   PartialFillPolicy
	Quote         QuoteFunc
	OnEvent       func(Event)
	PositionGuard PositionGuard
}

type LimitRequest struct {
	TokenID      string
	Side         clobclient.Side
	Shares       float64
	Price        float64
	ValidFor     time.Duration
	OrderType    clobclient.OrderType
	PostOnly     bool
	PollInterval time.Duration
	OnEvent      func(Event)
}

type PositionRequest struct {
	TokenID        string
	Side           clobclient.Side
	TargetShares   float64
	LimitPrice     float64
	CompleteWithin time.Duration
	RequoteEvery   time.Duration
	PostOnly       bool
	Quote          QuoteFunc
	OnEvent        func(Event)
}

type CloseRequest struct {
	TokenID        string
	Shares         float64
	LimitPrice     float64
	CompleteWithin time.Duration
	RequoteEvery   time.Duration
	PostOnly       bool
	Quote          QuoteFunc
	OnEvent        func(Event)
}

type EventType string

const (
	EventSubmitted EventType = "submitted"
	EventFill      EventType = "fill"
	EventCanceling EventType = "canceling"
	EventTerminal  EventType = "terminal"
	EventRetrying  EventType = "retrying"
)

type Event struct {
	Type      EventType
	At        time.Time
	Attempt   int
	OrderID   string
	Price     float64
	Filled    float64
	Remaining float64
	Err       error
}

type ChildOrder struct {
	Attempt         int
	OrderID         string
	Price           float64
	RequestedShares float64
	FilledShares    float64
	AveragePrice    float64
	Status          string
	SubmittedAt     time.Time
	FinishedAt      time.Time
	CancelRequested bool
	Error           error
}

type Result struct {
	TokenID         string
	Side            clobclient.Side
	RequestedShares float64
	FilledShares    float64
	RemainingShares float64
	AveragePrice    float64
	Completed       bool
	Expired         bool
	Canceled        bool
	StartedAt       time.Time
	FinishedAt      time.Time
	Orders          []ChildOrder
}
