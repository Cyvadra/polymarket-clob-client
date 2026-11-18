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
	// ErrInventoryClosed refuses a split on a lane that has merged.
	ErrInventoryClosed = errors.New("inventory lane has merged")
	// ErrInsufficientInventory refuses a preserve reservation the lane's
	// remaining inventory cannot back.
	ErrInsufficientInventory = errors.New("not enough split inventory")
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

// IntentStatusSignalDrift marks an open intent whose order executiond
// cancelled because the market drifted from the signal. Like
// IntentStatusSuperseded it outlives the order's later transitions, so the
// terminal result can name the cause rather than a bare cancel. The one
// exception is FILLED: an order that filled before the cancel landed replaces
// the marker.
const IntentStatusSignalDrift = "SIGNAL_DRIFT"

// IntentStatusDeadline marks an intent whose order executiond cancelled at its
// deadline, so the result can say EXPIRED rather than a bare cancel. It
// outlives later transitions exactly as IntentStatusSignalDrift does.
const IntentStatusDeadline = "DEADLINE_CANCEL"

// StrategyChildSequence is the child order a strategy intent maps onto.
// Higher sequences are internal children (such as a force-close exit) that the
// strategy never acknowledged and must not receive intent acknowledgements for.
const StrategyChildSequence = 1

type OrderIntentRecord struct {
	IntentID    string
	RequestID   string // the requester's id, echoed on results; empty if none was sent
	UniqueTag   string
	Strategy    string
	Kind        IntentKind
	MarketID    string
	EventSlug   string
	ConditionID string
	TokenID     string
	Outcome     string
	Side        Side
	TargetUSD   string
	// TargetEquityFraction and SizedEquityUSD record how an equity-sized open
	// resolved to TargetUSD; both are empty for a dollar-sized one.
	TargetEquityFraction string
	SizedEquityUSD       string
	LimitPrice           string
	BottomPrice          string
	ScalingMode          string
	TimeInForce          TimeInForce
	PostOnly             bool
	FeatureSeq           int64
	FeatureCompletedAt   time.Time
	ExpiresAt            time.Time
	// SignalPrice and MaxSignalDrift are the open's drift bound
	// (protocol.ExecutionIntent); both are empty when it has none.
	SignalPrice    string
	MaxSignalDrift string
	Status         statemachine.State
	Policy         json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	MarketID      string
	ConditionID   string
	TokenID       string
	UniqueTag     string
	Outcome       string
	PositionSize  string
	ActualShares  string
	AvailableSize string
	ReservedSize  string
	EntryPrice    string
	EntryTime     time.Time
	// OpenLots is the number of BUY intents filled since EntryTime; see
	// positionSelectSQL.
	OpenLots       int
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
	// LatestLimitCloseIntent returns the lane's most recent limit close intent,
	// the one whose price a maintenance pass re-places against. Force closes
	// are excluded: they carry a nominal exit price, not a strategy target.
	// Returns ErrNotFound when the lane has never been closed at a limit.
	LatestLimitCloseIntent(ctx context.Context, conditionID, tokenID, uniqueTag string) (OrderIntentRecord, error)
}

type OrderStore interface {
	PersistSignedOrder(context.Context, SignedOrderRecord) error
	// PersistSubmittingOrder is PersistSignedOrder then TransitionOrder with
	// EventSubmitStarted, committed together; it returns the SUBMITTING row.
	PersistSubmittingOrder(ctx context.Context, record SignedOrderRecord, reason string) (SignedOrderRecord, error)
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OrderByIntent(context.Context, string, int) (SignedOrderRecord, error)
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
	OpenOrders(context.Context) ([]SignedOrderRecord, error)
}

type FillStore interface {
	ApplyFill(context.Context, FillRecord) (inserted bool, err error)
}

// LaneKey identifies a lane.
type LaneKey struct{ ConditionID, TokenID, UniqueTag string }

// LaneStrategyReader maps the lanes holding shares to the strategy of their
// first opening intent; a lane nothing opened is absent. It is kept apart from
// PositionStore because only equity snapshots need it.
type LaneStrategyReader interface {
	LaneStrategies(context.Context) (map[LaneKey]string, error)
}

type PositionStore interface {
	PositionFeatures(context.Context) ([]PositionRecord, error)
}

// EquitySnapshotRecord is one recorded valuation of the wallet. TradeCashUSD
// is the cash that recorded trading moved since the previous snapshot,
// ExternalFlowUSD what the cash change left unexplained (a deposit when
// positive, a withdrawal when negative), and TradeIndex the time-weighted
// return of trading alone, chained from 1 at the first snapshot.
type EquitySnapshotRecord struct {
	ID                int64
	TakenAt           time.Time
	Reason            string
	CashUSD           string
	PositionsUSD      string
	EquityUSD         string
	TradeCashUSD      string
	ExternalFlowUSD   string
	TradeIndex        string
	UnmarkedPositions int
	// Strategies splits the trading by strategy; see StrategyEquityRecord.
	Strategies []StrategyEquityRecord
}

// StrategyEquityRecord is one strategy's part of an equity snapshot: the
// trade cash its lanes moved since the previous snapshot, what its lanes held
// were worth, and TradeIndex, its share of the wallet's trading return
// chained from 1. A strategy's drawdown limit is measured on that index.
type StrategyEquityRecord struct {
	Strategy     string
	TradeCashUSD string
	PositionsUSD string
	TradeIndex   string
}

// PendingTradeCash is the cash moved by fills and settlement payouts that no
// snapshot has counted yet: sells and payouts in, buys and fees out. The IDs
// are the rows it was summed from.
type PendingTradeCash struct {
	USD       string
	FillIDs   []string
	PayoutIDs []int64
	// ByStrategy splits USD by the strategy of each row's lane, rows of a
	// lane with no recorded strategy under "".
	ByStrategy map[string]string
	// Lost reports whether any of the payouts is a lane settled as a loser.
	Lost bool
}

// ErrTradeCashCounted is returned when a snapshot would count trade cash
// another snapshot already has.
var ErrTradeCashCounted = errors.New("trade cash already counted by another snapshot")

type EquityStore interface {
	// PendingTradeCash reads the trade cash not yet counted. Read it before
	// the wallet's balance, so every row it counts is already in that balance.
	PendingTradeCash(context.Context) (PendingTradeCash, error)
	// RecordEquitySnapshot inserts the snapshot and marks the rows of counted
	// as counted by it, together. It fails with ErrTradeCashCounted, saving
	// nothing, when any of them already were.
	RecordEquitySnapshot(ctx context.Context, record EquitySnapshotRecord, counted PendingTradeCash) (EquitySnapshotRecord, error)
	// LatestEquitySnapshot reports false when nothing has been recorded yet.
	LatestEquitySnapshot(context.Context) (EquitySnapshotRecord, bool, error)
	// LatestEquitySnapshotWithReason is the latest snapshot recorded under
	// any of reasons; it reports false when there is none.
	LatestEquitySnapshotWithReason(ctx context.Context, reasons ...string) (EquitySnapshotRecord, bool, error)
	// ExternalFlowAfter sums ExternalFlowUSD over the snapshots recorded
	// after the one with id.
	ExternalFlowAfter(ctx context.Context, id int64) (string, error)
	// PeakTradeIndexSince is the highest TradeIndex among the snapshots at or
	// after since and the last one before it, the level trading at since
	// started from; it reports false when there is none.
	PeakTradeIndexSince(ctx context.Context, since time.Time) (string, bool, error)
	// StrategyEquity is the per-strategy split of the snapshot with id.
	StrategyEquity(ctx context.Context, snapshotID int64) ([]StrategyEquityRecord, error)
	// StrategyReturnsSince lists each interval's trading return of strategy,
	// or with prefix of every strategy starting with it added together, for
	// the snapshots from the first at or after since through snapshotID, in
	// order. A strategy's return is the change in its TradeIndex, from 1 when
	// it is new, and zero across the gaps TradingPnLSince counts nothing for.
	// It reports false as TradingPnLSince does.
	StrategyReturnsSince(ctx context.Context, strategy string, prefix bool, since time.Time, snapshotID int64) ([]float64, bool, error)
	// TradingPnLSince lists each interval's trading profit and loss, in
	// dollars, for the snapshots from the first at or after since through
	// snapshotID, in order. An interval's dollars are the equity change less
	// the external flow; a strategy's (strategy "" is the wallet) are its
	// trade cash plus the change in what it holds, and zero for an interval
	// whose previous snapshot held positions it has no split for. With prefix,
	// the dollars of every strategy starting with strategy are added. It
	// reports false when snapshotID is before since or, for a strategy,
	// records no row for it.
	TradingPnLSince(ctx context.Context, strategy string, prefix bool, since time.Time, snapshotID int64) ([]TradingPnL, bool, error)
}

// TradingPnL is one interval of TradingPnLSince. ExternalFlowUSD is the
// wallet's flow in the interval, even for a strategy's.
type TradingPnL struct {
	SnapshotID      int64
	PnLUSD          string
	ExternalFlowUSD string
}

// PositionReconciler adopts the exchange's own view of a lane's size. It is
// kept apart from PositionStore because most consumers only read positions.
type PositionReconciler interface {
	// ReconcilePositionSize clamps a lane's recorded size down to what the
	// exchange reports it holds. The local sizes are derived from fills, which
	// can overstate the wallet if a trade failed on chain or was never seen;
	// the exchange's figure is authoritative when it is lower. It never raises
	// a position: an unseen fill must arrive as a fill, not as a clamp.
	ReconcilePositionSize(ctx context.Context, conditionID, tokenID, uniqueTag, shares string) error
}

// PositionSettler ends a lane whose market has resolved.
type PositionSettler interface {
	// SettlePosition lowers a lane to shares: what the wallet still holds of a
	// winning token, or nothing for a loser. Like ReconcilePositionSize it
	// never raises a lane. A lane with shares reserved is left untouched, in
	// the same statement, since a close is working on it and owns it until the
	// reservation ends. Settling to zero clears the lane like a sell that
	// empties it does, so entry_time and the open-lot count reset with it.
	// A winner's removed shares are recorded as a $1-a-share payout in the
	// same statement, since the wallet redeemed them for cash.
	// It reports whether the lane changed.
	SettlePosition(ctx context.Context, conditionID, tokenID, uniqueTag, shares string, winner bool) (bool, error)
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

// OrderPriceStore reports the fill-weighted average price of one exchange
// order; an empty string means no fill is recorded yet.
type OrderPriceStore interface {
	OrderAveragePrice(context.Context, string) (string, error)
}

// LadderPhase is where a multi-level intent is in its life. It is its own
// column because the intent status follows every child order's state: a
// marker kept in the status would be overwritten by the next child transition.
type LadderPhase string

const (
	// LadderNone is an intent that is not a ladder, or one not yet started.
	LadderNone LadderPhase = ""
	// LadderPlacing: children are still being planned and posted, so an
	// all-terminal set of children is not yet the end of the intent.
	LadderPlacing LadderPhase = "PLACING"
	// LadderActive: every child that will ever exist has been posted.
	LadderActive LadderPhase = "ACTIVE"
	// LadderStopping: a CANCEL_OPEN, force close or expiry is withdrawing
	// the ladder; nothing is reposted.
	LadderStopping LadderPhase = "STOPPING"
	// LadderPublished: the one aggregated result has been claimed.
	LadderPublished LadderPhase = "PUBLISHED"
)

// AggregateOrderStore is the durable view that folds a multi-level intent's
// children into one result. It is separate from AccountOrderStore so that
// consumers of a single exchange order do not have to carry it.
type AggregateOrderStore interface {
	Intent(context.Context, string) (OrderIntentRecord, error)
	// IntentOrders returns every child of the intent, lowest sequence first.
	IntentOrders(context.Context, string) ([]SignedOrderRecord, error)
	LadderPhase(context.Context, string) (LadderPhase, error)
	// MoveLadderPhase sets the phase only while it is one of from, and
	// reports whether it did. It is the compare-and-set that makes cancel,
	// placement and result publication agree without holding a lock.
	MoveLadderPhase(ctx context.Context, intentID string, to LadderPhase, from ...LadderPhase) (moved bool, err error)
	OrderPriceStore
}

// LadderStore is what the executor needs to run a multi-level intent.
type LadderStore interface {
	AggregateOrderStore
	// ResignRetryOrder replaces a RETRY_PENDING order's signed payload, hash,
	// salt and exchange order ID and moves it to SUBMITTING in one commit. A
	// post-only cross is a definite rejection, but a fresh signature keeps
	// the repost from being refused as a duplicate of the rejected hash.
	ResignRetryOrder(ctx context.Context, order SignedOrderRecord, payload []byte, hash, salt, exchangeOrderID, reason string) (SignedOrderRecord, error)
	// PendingLadders lists the intents whose placement has ended, whose
	// children have all ended, and whose result has not been published, so a
	// result lost to a crash or a failed publish is found again.
	PendingLadders(ctx context.Context) ([]string, error)
	// PlacingLadders lists the intents still marked PLACING, so a restart can
	// end the placement a crash interrupted, even when no child is left open.
	PlacingLadders(ctx context.Context) ([]string, error)
}

// QuoteLaneKey names a quote lane: every rule of a quote applies per lane.
type QuoteLaneKey struct {
	UniqueTag   string
	ConditionID string
	TokenID     string
}

// QuoteResultState is whether a quote lane's one result is owed.
type QuoteResultState string

const (
	QuoteResultNone      QuoteResultState = ""
	QuoteResultOwed      QuoteResultState = "OWED"
	QuoteResultPublished QuoteResultState = "PUBLISHED"
)

// QuoteLaneRecord is what a quote lane keeps across a restart. Its levels are
// the child orders of IntentID, the lane's one QUOTE intent, which is empty
// until the lane first posts.
type QuoteLaneRecord struct {
	Key      QuoteLaneKey
	IntentID string
	LastSeq  int64
	Deadline time.Time
	// Done is set by the first fill; the lane then refuses every request.
	Done           bool
	ResultState    QuoteResultState
	FilledSequence int
	UpdatedAt      time.Time
}

// QuoteStore is the durable state of quote lanes.
type QuoteStore interface {
	// QuoteLane reads one lane; found is false when it has never quoted.
	QuoteLane(ctx context.Context, key QuoteLaneKey) (lane QuoteLaneRecord, found bool, err error)
	// QuoteLaneByIntent reads the lane whose QUOTE intent is intentID.
	QuoteLaneByIntent(ctx context.Context, intentID string) (lane QuoteLaneRecord, found bool, err error)
	// SaveQuoteLane records a newly applied request: its seq, the deadline
	// and the lane's intent. It never lowers last_seq and never touches a
	// lane that is done.
	SaveQuoteLane(ctx context.Context, lane QuoteLaneRecord) error
	// MarkQuoteDone ends a lane at its first fill and makes its result owed,
	// once: it reports false when the lane was already done.
	MarkQuoteDone(ctx context.Context, key QuoteLaneKey, intentID string, filledSequence int) (marked bool, err error)
	// MoveQuoteResult is a compare-and-set on the result state.
	MoveQuoteResult(ctx context.Context, key QuoteLaneKey, to, from QuoteResultState) (moved bool, err error)
	// OwedQuoteResults lists the lanes whose result is owed.
	OwedQuoteResults(ctx context.Context) ([]QuoteLaneRecord, error)
	// RecordQuoteLevel stores the request that posted a level.
	RecordQuoteLevel(ctx context.Context, intentID string, childSequence int, requestID string) error
	// QuoteLevelRequest reads it back; empty when none was recorded.
	QuoteLevelRequest(ctx context.Context, intentID string, childSequence int) (string, error)
}

type AccountOrderStore interface {
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OrderByExchangeID(context.Context, string) (SignedOrderRecord, error)
	Intent(context.Context, string) (OrderIntentRecord, error)
	OrderPriceStore
}

type ReconcileStore interface {
	TransitionOrder(context.Context, SignedOrderRecord, statemachine.Event, string, string, string) (SignedOrderRecord, error)
	OpenOrders(context.Context) ([]SignedOrderRecord, error)
	Intent(context.Context, string) (OrderIntentRecord, error)
	FilledShares(context.Context, string) (string, error)
	OrderPriceStore
}

type Store interface {
	ExecutionStore
	FillStore
	PositionStore
}

// DaemonRun is one executiond process: who it signs as and its non-secret
// settings.
type DaemonRun struct {
	Version    string
	Signer     string
	Maker      string
	Strategies []string
	Config     map[string]any
}

// SweepRecord is one settlement sweep pass. A zero RunID is stored as NULL.
type SweepRecord struct {
	RunID                                                    int64
	At                                                       time.Time
	Checked, Lost, Redeemed, Dust, Shrunk, Closing, Settling int
	Failed                                                   bool
}

// Risk event kinds.
const (
	RiskSuspend          = "suspend"
	RiskResume           = "resume"
	RiskSizingBase       = "sizing_base"
	RiskSnapshotDeferred = "snapshot_deferred"
	RiskEquityRecorded   = "equity_recorded"
)

// RiskEvent is a risk or sizing decision. A zero RunID is stored as NULL.
type RiskEvent struct {
	RunID   int64
	At      time.Time
	Kind    string
	Subject string
	Reason  string
	Details map[string]any
}

// CTFAction is a split or a merge of a binary condition's outcome tokens.
type CTFAction string

const (
	CTFSplit CTFAction = "SPLIT"
	CTFMerge CTFAction = "MERGE"
)

// CTFOpState is where a split or merge is on its way through the relayer.
type CTFOpState string

const (
	CTFPending   CTFOpState = "PENDING"
	CTFSubmitted CTFOpState = "SUBMITTED"
	CTFConfirmed CTFOpState = "CONFIRMED"
	CTFFailed    CTFOpState = "FAILED"
	// CTFUnknown is a submission whose outcome is unknown: it may still land,
	// so it counts as in flight until the chain says otherwise.
	CTFUnknown CTFOpState = "UNKNOWN"
)

// Fill sources (fills.source).
const (
	FillSourceCLOB     = "CLOB"
	FillSourceSplit    = "SPLIT"
	FillSourceMerge    = "MERGE"
	FillSourceTransfer = "TRANSFER"
)

// CTFOperationRecord is one split or merge. UniqueTag is the strategy lane;
// the tokens are booked on its inventory lane, InventoryTag.
type CTFOperationRecord struct {
	OpID         string
	RequestID    string
	Strategy     string
	UniqueTag    string
	InventoryTag string
	MarketID     string
	ConditionID  string
	Action       CTFAction
	TargetShares string
	Shares       string
	Tokens       []string
	Outcomes     []string
	State        CTFOpState
	RelayerTxID  string
	TxHash       string
	Error        string
	// Baseline is each token's wallet balance in base units just before
	// submission, ArmedAt when it was read, and Deadline the signed batch's
	// deadline (ArmCTFOperation).
	Baseline  []string
	ArmedAt   time.Time
	Deadline  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InventoryLevel is the split inventory of one lane. Remaining is the pairs
// free to back a new SPLIT_PRESERVE open: the smaller side held, less what
// resting preserves have reserved. InFlight is the pairs of splits not yet
// booked, an unknown submission included. Merged means a merge was asked for
// on the lane, whatever became of it: the lane takes no further split, and a
// merge that failed leaves its pairs to a retried merge or to settlement.
type InventoryLevel struct {
	Remaining string
	// Reserved is the shares resting preserves hold across both tokens.
	Reserved string
	InFlight string
	Merged   bool
}

// InventoryStore books split inventory. Every write takes the inventory
// lane's lock, so a split sized from the level never counts the same gap
// twice and a preserve never reserves a pair another one holds.
type InventoryStore interface {
	// BeginSplit records a split of the pairs that close the gap between the
	// lane's remaining-plus-in-flight inventory and op.TargetShares, at most
	// maxShares when it is set. It returns the recorded operation, or
	// recorded=false when the lane is already at target.
	// gap is the shortfall before the maxShares cap.
	BeginSplit(ctx context.Context, op CTFOperationRecord, maxShares string) (recorded CTFOperationRecord, gap string, ok bool, err error)
	// CloseInventory records a merge of no shares yet (PENDING), so the lane
	// is merged at once: splits are refused and preserves reserve nothing.
	CloseInventory(ctx context.Context, op CTFOperationRecord) (CTFOperationRecord, error)
	// BeginMerge records a merge of op.Shares pairs, all remaining when it is
	// empty, and marks the lane merged. ok is false when nothing is left.
	BeginMerge(ctx context.Context, op CTFOperationRecord) (recorded CTFOperationRecord, ok bool, err error)
	// ArmCTFOperation records the balances an operation is submitted
	// against, when they were read, and its deadline, before it is submitted.
	ArmCTFOperation(ctx context.Context, opID string, baseline []string, armedAt, deadline time.Time) error
	// ExchangeNetShares is, for each token, the shares the wallet's exchange
	// fills matched at or after since bought less those they sold, in base
	// units: what trades, not splits or merges, moved its balance by.
	ExchangeNetShares(ctx context.Context, tokens []string, since time.Time) ([]string, error)
	MarkCTFOperation(ctx context.Context, opID string, state CTFOpState, relayerTxID, txHash, errText string) error
	// ConfirmCTFOperation books a landed operation's fills and marks it
	// CONFIRMED in one transaction; booking it again changes nothing.
	ConfirmCTFOperation(ctx context.Context, opID, txHash string, at time.Time) error
	// OpenCTFOperations lists the operations still PENDING, SUBMITTED or
	// UNKNOWN, for recovery.
	OpenCTFOperations(ctx context.Context) ([]CTFOperationRecord, error)
	InventoryLevel(ctx context.Context, uniqueTag, conditionID string) (InventoryLevel, error)
	// InventoryLevelAndPosition is InventoryLevel plus the inventory lane's
	// position in tokenID, read in one round trip; found is false when the
	// lane holds no row for the token.
	InventoryLevelAndPosition(ctx context.Context, uniqueTag, conditionID, tokenID string) (InventoryLevel, PositionRecord, bool, error)
	// CTFOperationsSettling reports whether any operation is in flight or
	// was confirmed after since: cash may not reflect it yet.
	CTFOperationsSettling(ctx context.Context, since time.Time) (bool, error)
}
