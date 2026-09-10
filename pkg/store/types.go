// Package store defines the durable execution state boundary.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
)

var (
	ErrDuplicate             = errors.New("duplicate record")
	ErrConflict              = errors.New("state conflict")
	ErrNotFound              = errors.New("record not found")
	ErrActiveSellReservation = errors.New("active sell reservation already exists")
	ErrExposureLimit         = errors.New("reservation exceeds the configured open exposure limit")
)

// Side is the store-local order side. It is deliberately independent of the
// NATS wire types so the persistence layer does not change when the wire
// protocol evolves.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// TimeInForce is the store-local order time-in-force.
type TimeInForce string

const (
	TimeInForceGTC TimeInForce = "GTC"
	TimeInForceFOK TimeInForce = "FOK"
	TimeInForceFAK TimeInForce = "FAK"
	TimeInForceGTD TimeInForce = "GTD"
)

// IntentKind is the store-local execution intent kind.
type IntentKind string

const (
	IntentOpen  IntentKind = "OPEN"
	IntentClose IntentKind = "CLOSE"
)

// IntentStatusSuperseded marks a close intent that was replaced by a newer
// close request on the same lane. Its terminal result must not be surfaced.
const IntentStatusSuperseded = "SUPERSEDED"

// StrategyChildSequence is the child order a strategy intent maps onto.
// Higher sequences are internal children (such as a force-close exit) that the
// strategy never acknowledged and must not receive intent acknowledgements for.
const StrategyChildSequence = 1

type OrderIntentRecord struct {
	IntentID           string
	UniqueTag          string
	Strategy           string
	Kind               IntentKind
	MarketID           string
	EventSlug          string
	ConditionID        string
	TokenID            string
	Outcome            string
	Side               Side
	TargetUSD          string
	LimitPrice         string
	TimeInForce        TimeInForce
	PostOnly           bool
	FeatureSeq         int64
	FeatureCompletedAt time.Time
	ExpiresAt          time.Time
	Status             statemachine.State
	Policy             json.RawMessage
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
	OrderType       TimeInForce
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
	UniqueTag       string
	MarketID        string
	ConditionID     string
	TokenID         string
	Outcome         string
	Side            Side
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
	UniqueTag      string
	Outcome        string
	PositionSize   string
	ActualShares   string
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
	UniqueTag     string
	MarketID      string
	ConditionID   string
	TokenID       string
	Outcome       string
	Side          Side
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
	UpdateIntentStatus(context.Context, string, string) error
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
	Intent(context.Context, string) (OrderIntentRecord, error)
}

type AccountOrderStore interface {
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
	Intent(context.Context, string) (OrderIntentRecord, error)
}

type ReconcileStore interface {
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OpenOrders(context.Context) ([]SignedOrderRecord, error)
	Intent(context.Context, string) (OrderIntentRecord, error)
	FilledShares(context.Context, string) (string, error)
}

type Store interface {
	ExecutionStore
	FillStore
	PositionStore
}
