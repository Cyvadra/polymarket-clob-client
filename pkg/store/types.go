// Package store defines the durable execution state boundary.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
)

var (
	ErrDuplicate = errors.New("duplicate record")
	ErrConflict  = errors.New("state conflict")
	ErrNotFound  = errors.New("record not found")
)

type OrderIntentRecord struct {
	IntentID           string
	IdempotencyKey     string
	Strategy           string
	Kind               protocol.IntentKind
	MarketID           string
	EventSlug          string
	ConditionID        string
	TokenID            string
	Outcome            string
	Side               protocol.Side
	TargetShares       string
	LimitPrice         string
	TimeInForce        protocol.TimeInForce
	PostOnly           bool
	FeatureSeq         int64
	FeatureCompletedAt time.Time
	ExpiresAt          time.Time
	Status             statemachine.State
	Policy             protocol.ExecutionPolicy
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type SignedOrderRecord struct {
	IntentID        string
	ChildSequence   int
	SignedPayload   []byte
	SignedOrderHash string
	Salt            string
	ExchangeOrderID string
	RequestedShares string
	MatchedShares   string
	Price           string
	OrderType       protocol.TimeInForce
	PostOnly        bool
	State           statemachine.State
	Revision        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type OrderEventRecord struct {
	EventID         string
	IntentID        string
	ChildSequence   int
	ExchangeOrderID string
	FromState       statemachine.State
	ToState         statemachine.State
	Event           statemachine.Event
	Observation     []byte
	Reason          string
	ExchangeTime    time.Time
	ReceivedAt      time.Time
	CreatedAt       time.Time
}

type FillRecord struct {
	FillID          string
	ExchangeOrderID string
	IntentID        string
	MarketID        string
	ConditionID     string
	TokenID         string
	Outcome         string
	Side            protocol.Side
	Shares          string
	Price           string
	Fee             string
	FeeRateBps      string
	TradeStatus     string
	TraderSide      string
	ExchangeTime    time.Time
	ReceivedAt      time.Time
}

type PositionRecord struct {
	MarketID       string
	ConditionID    string
	TokenID        string
	Outcome        string
	PositionSize   string
	AvailableSize  string
	ReservedSize   string
	EntryPrice     string
	EntryTime      time.Time
	State          string
	SourceRevision int64
	UpdatedAt      time.Time
}

type ReservationRecord struct {
	ReservationID string
	IntentID      string
	ChildSequence int
	MarketID      string
	ConditionID   string
	TokenID       string
	Outcome       string
	Side          protocol.Side
	Shares        string
	Notional      string
	State         string
	Reason        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type IntentRepository interface {
	InsertIntent(context.Context, OrderIntentRecord) (inserted bool, err error)
	Intent(context.Context, string) (OrderIntentRecord, error)
}

type IntentLockRepository interface {
	WithIntentLock(context.Context, string, func(context.Context) error) error
}

type OrderRepository interface {
	PersistSignedOrder(context.Context, SignedOrderRecord) error
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OrderByIntent(context.Context, string, int) (SignedOrderRecord, error)
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
	OpenOrders(context.Context) ([]SignedOrderRecord, error)
}

type FillRepository interface {
	ApplyFill(context.Context, FillRecord) (inserted bool, err error)
}

type PositionRepository interface {
	Position(context.Context, string, string) (PositionRecord, error)
	PositionFeatures(context.Context) ([]PositionRecord, error)
}

type ReservationRepository interface {
	Reserve(context.Context, ReservationRecord) error
	Reservation(context.Context, string) (ReservationRecord, error)
	Release(context.Context, string, string) error
	ReservationsForPosition(context.Context, string, string) ([]ReservationRecord, error)
}
