// Package store defines the durable execution state boundary.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
)

var (
	ErrDuplicate             = errors.New("duplicate record")
	ErrConflict              = errors.New("state conflict")
	ErrNotFound              = errors.New("record not found")
	ErrIdempotencyConflict   = errors.New("idempotency key already belongs to another intent")
	ErrActiveSellReservation = errors.New("active sell reservation already exists")
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

type IntentStore interface {
	WithIntentLock(context.Context, string, func(context.Context) error) error
	InsertIntent(context.Context, OrderIntentRecord) (inserted bool, err error)
	Intent(context.Context, string) (OrderIntentRecord, error)
}

type OrderStore interface {
	PersistSignedOrder(context.Context, SignedOrderRecord) error
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OrderByIntent(context.Context, string, int) (SignedOrderRecord, error)
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
	OpenOrders(context.Context) ([]SignedOrderRecord, error)
}

type FillStore interface {
	ApplyFill(context.Context, FillRecord) (inserted bool, err error)
}

type PositionStore interface {
	PositionFeatures(context.Context) ([]PositionRecord, error)
}

type ReservationStore interface {
	Reserve(context.Context, ReservationRecord) error
	Reservation(context.Context, string) (ReservationRecord, error)
	Release(context.Context, string, string) error
}

type ExecutionStore interface {
	IntentStore
	OrderStore
	ReservationStore
}

type AccountFillStore interface {
	FillStore
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
}

type AccountOrderStore interface {
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
}

type ReconcileStore interface {
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OpenOrders(context.Context) ([]SignedOrderRecord, error)
}

type Store interface {
	ExecutionStore
	FillStore
	PositionStore
}

func TerminalAckForOrder(order SignedOrderRecord, reason string, occurredAt time.Time) (protocol.ExecutionIntentAck, bool) {
	ack := protocol.ExecutionIntentAck{IntentID: order.IntentID, Reason: reason, FilledShares: order.MatchedShares, OccurredAt: occurredAt}
	switch order.State {
	case statemachine.StateFilled:
		ack.Status = protocol.IntentCompleted
	case statemachine.StateCanceled:
		ack.Status = protocol.IntentExpired
		if hasMatchedShares(order.MatchedShares) {
			ack.Status = protocol.IntentPartial
		}
	case statemachine.StateRejected:
		ack.Status = protocol.IntentRejected
		ack.ReasonCode = "ORDER_REJECTED"
	case statemachine.StateExpired:
		ack.Status = protocol.IntentExpired
	case statemachine.StateFailed:
		ack.Status = protocol.IntentFailed
		ack.ReasonCode = "EXECUTION_FAILED"
	default:
		return protocol.ExecutionIntentAck{}, false
	}
	return ack, true
}

func hasMatchedShares(value string) bool {
	return decimal.Positive(value)
}
