// Package protocol defines versioned JSON messages exchanged by executiond
// over NATS. The external contract is documented in docs/protocol/nats-v1.md.
package protocol

import (
	"fmt"
	"strconv"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type ExecutionEventPublisher interface {
	PublishJSON(string, any) error
}

const (
	SchemaVersionV1 = "execution.v1"

	SubjectStrategyExecutionOpen          = "strategy.execution.open"
	SubjectExecutionOpenResult            = "execution.open.result"
	SubjectStrategyExecutionClose         = "strategy.execution.close"
	SubjectExecutionCloseResult           = "execution.close.result"
	SubjectExecutionOrderEvent            = "execution.order.event"
	SubjectMarketQuotes                   = "pmm.market.quotes"
	SubjectMarketLevels                   = "pmm.market.levels"
	SubjectPositionFeaturesPrefix         = "position.features"
	SubjectStrategyExecutionPositionQuery = "strategy.execution.position.query"
	SubjectStrategyExecutionBalanceQuery  = "strategy.execution.balance.query"
	// SubjectStrategyExecutionQuote carries a quote lane's wanted resting
	// levels (ExecutionQuoteRequest), and SubjectExecutionQuoteResult the
	// first fill that ends the quote (ExecutionQuoteResult).
	SubjectStrategyExecutionQuote = "strategy.execution.quote"
	SubjectExecutionQuoteResult   = "execution.quote.result"
	// SubjectStrategyExecutionInventory carries a split_preserve lane's
	// inventory requests (ExecutionInventoryRequest: SPLIT tops the paired
	// inventory up to a target, MERGE returns it to pUSD), and
	// SubjectExecutionInventoryResult each one's outcome.
	SubjectStrategyExecutionInventory = "strategy.execution.inventory"
	SubjectExecutionInventoryResult   = "execution.inventory.result"
)

type Side = clobclient.Side

const (
	SideBuy  = clobclient.SideBuy
	SideSell = clobclient.SideSell
)

type TimeInForce = clobclient.OrderType

const (
	TimeInForceGTC = clobclient.OrderTypeGTC
	TimeInForceFOK = clobclient.OrderTypeFOK
	TimeInForceFAK = clobclient.OrderTypeFAK
	TimeInForceGTD = clobclient.OrderTypeGTD
)

type IntentKind string

const (
	IntentOpen  IntentKind = "OPEN"
	IntentClose IntentKind = "CLOSE"
)

type ExecutionStyle string

const (
	ExecutionStyleLimit           ExecutionStyle = "LIMIT"
	ExecutionStyleMakerPostOnly   ExecutionStyle = "MAKER_POST_ONLY"
	ExecutionStyleTakerAggressive ExecutionStyle = "TAKER_AGGRESSIVE"
	// ExecutionStyleMultiLevelLimit posts a post-only GTC buy at every cent of
	// bottom_price..limit_price; see docs/protocol/nats-v1.md.
	ExecutionStyleMultiLevelLimit ExecutionStyle = "MULTI_LEVEL_LIMIT"
	// ExecutionStyleQuote marks the stored intent of a quote lane, whose
	// post-only GTC buy levels follow ExecutionQuoteRequest. No request
	// carries it: it is never accepted on strategy.execution.open.
	ExecutionStyleQuote ExecutionStyle = "QUOTE"
	// ExecutionStyleSplitPreserve opens a BUY of one outcome by selling the
	// other outcome out of the lane's split inventory at 1 - limit_price. The
	// request and its result read as an ordinary BUY of the requested token;
	// see docs/protocol/nats-v1.md.
	ExecutionStyleSplitPreserve ExecutionStyle = "SPLIT_PRESERVE"
)

// InventoryTagSuffix marks the lane that holds a strategy lane's split
// inventory: unique_tag + InventoryTagSuffix.
const InventoryTagSuffix = "|inv"

// InventoryTag is the inventory lane of a strategy lane.
func InventoryTag(uniqueTag string) string { return uniqueTag + InventoryTagSuffix }

// IsInventoryTag reports whether a lane holds split inventory rather than a
// strategy's position.
func IsInventoryTag(uniqueTag string) bool {
	return len(uniqueTag) > len(InventoryTagSuffix) && uniqueTag[len(uniqueTag)-len(InventoryTagSuffix):] == InventoryTagSuffix
}

// PreserveLeg is what executiond records on a SPLIT_PRESERVE intent, whose
// stored token, side and lane are those of the complement SELL it places:
// the BUY the strategy asked for, so fills and results map back to it. No
// request may carry it.
type PreserveLeg struct {
	UniqueTag       string `json:"unique_tag"`
	TokenID         string `json:"token_id"`
	Outcome         string `json:"outcome"`
	LimitPrice      string `json:"limit_price"`
	SignalPrice     string `json:"signal_price,omitempty"`
	RequestedShares string `json:"requested_shares"`
	Shares          string `json:"shares"`
}

// ScalingModeFlat sizes every level of a multi-level intent at the full
// target_usd. It is the only scaling mode.
const ScalingModeFlat = "flat"

type ExecutionPolicy struct {
	CompleteWithinMillis int64 `json:"complete_within_ms,omitempty"`
	// RestAfterAckMillis is how long a GTC open may rest once the exchange
	// acknowledged it, so a slow submission does not eat its window.
	// expires_at still caps it, counted from the decision.
	RestAfterAckMillis         int64          `json:"rest_after_ack_ms,omitempty"`
	CancelTimeoutMillis        int64          `json:"cancel_timeout_ms,omitempty"`
	MaxFeatureAgeMillis        int64          `json:"max_feature_age_ms,omitempty"`
	Style                      ExecutionStyle `json:"style,omitempty"`
	MidPrice                   string         `json:"mid_price,omitempty"`
	InitialPrice               string         `json:"initial_price,omitempty"`
	MaxPrice                   string         `json:"max_price,omitempty"`
	MinPrice                   string         `json:"min_price,omitempty"`
	PriceStep                  string         `json:"price_step,omitempty"`
	QuoteOffset                string         `json:"quote_offset,omitempty"`
	RepriceIntervalMillis      int64          `json:"reprice_interval_ms,omitempty"`
	MaxReprices                int            `json:"max_reprices,omitempty"`
	QuoteMaxAgeMillis          int64          `json:"quote_max_age_ms,omitempty"`
	PostOnlyCrossRetry         bool           `json:"post_only_cross_retry,omitempty"`
	SoftCloseAfterMillis       int64          `json:"soft_close_after_ms,omitempty"`
	ForceCloseAfterMillis      int64          `json:"force_close_after_ms,omitempty"`
	CancelReplaceTimeoutMillis int64          `json:"cancel_replace_timeout_ms,omitempty"`
	// Preserve is set by executiond on a SPLIT_PRESERVE intent; a request
	// carrying it is refused.
	Preserve *PreserveLeg `json:"preserve,omitempty"`
}

type ExecutionIntent struct {
	SchemaVersion string     `json:"schema_version"`
	IntentID      string     `json:"intent_id"`
	RequestID     string     `json:"request_id,omitempty"`
	UniqueTag     string     `json:"unique_tag"`
	Strategy      string     `json:"strategy"`
	Kind          IntentKind `json:"kind"`
	MarketID      string     `json:"market_id,omitempty"`
	EventSlug     string     `json:"event_slug,omitempty"`
	ConditionID   string     `json:"condition_id"`
	TokenID       string     `json:"token_id"`
	Outcome       string     `json:"outcome"`
	Side          Side       `json:"side"`
	TargetUSD     string     `json:"target_usd,omitempty"`
	// TargetEquityFraction and SizedEquityUSD record how an equity-sized open
	// resolved to TargetUSD.
	TargetEquityFraction string      `json:"target_equity_fraction,omitempty"`
	SizedEquityUSD       string      `json:"sized_equity_usd,omitempty"`
	LimitPrice           string      `json:"limit_price"`
	BottomPrice          string      `json:"bottom_price,omitempty"`
	ScalingMode          string      `json:"scaling_mode,omitempty"`
	TimeInForce          TimeInForce `json:"time_in_force"`
	PostOnly             bool        `json:"post_only"`
	FeatureSeq           int64       `json:"feature_seq,omitempty"`
	FeatureCompletedAt   time.Time   `json:"feature_completed_at"`
	CreatedAt            time.Time   `json:"created_at"`
	ExpiresAt            time.Time   `json:"expires_at"`
	// SignalPrice is the side's mid on the frame that decided an open, and
	// MaxSignalDrift how far the market may move against the entry from it
	// before the order is worth cancelling unfilled: for a BUY, the mid falling
	// more than MaxSignalDrift under SignalPrice; for a SELL, rising more than
	// it above. A move in the entry's favour never cancels it. Both are empty
	// when the strategy sets no bound, and only a GTC order may carry one.
	SignalPrice    string          `json:"signal_price,omitempty"`
	MaxSignalDrift string          `json:"max_signal_drift,omitempty"`
	Policy         ExecutionPolicy `json:"policy,omitempty"`
}

type ExecutionOpenRequest struct {
	SchemaVersion string `json:"schema_version"`
	// RequestID is the requester's name for this request, echoed on every
	// result it produces so a requester talking to several instances can
	// match each answer to its question. Opaque here; may be empty.
	RequestID   string `json:"request_id,omitempty"`
	UniqueTag   string `json:"unique_tag"`
	Strategy    string `json:"strategy"`
	MarketID    string `json:"market_id,omitempty"`
	EventSlug   string `json:"event_slug,omitempty"`
	ConditionID string `json:"condition_id"`
	TokenID     string `json:"token_id"`
	Outcome     string `json:"outcome"`
	Side        Side   `json:"side"`
	TargetUSD   string `json:"target_usd,omitempty"`
	// TargetEquityFraction sizes the open as a fraction of wallet equity
	// (0.03 = 3%) instead of TargetUSD. Exactly one of the two must be set.
	TargetEquityFraction string      `json:"target_equity_fraction,omitempty"`
	LimitPrice           string      `json:"limit_price"`
	BottomPrice          string      `json:"bottom_price,omitempty"`
	ScalingMode          string      `json:"scaling_mode,omitempty"`
	TimeInForce          TimeInForce `json:"time_in_force"`
	PostOnly             bool        `json:"post_only"`
	FeatureSeq           int64       `json:"feature_seq,omitempty"`
	FeatureCompletedAt   time.Time   `json:"feature_completed_at"`
	CreatedAt            time.Time   `json:"created_at"`
	ExpiresAt            time.Time   `json:"expires_at"`
	// SignalPrice and MaxSignalDrift are ExecutionIntent's drift bound.
	SignalPrice    string          `json:"signal_price,omitempty"`
	MaxSignalDrift string          `json:"max_signal_drift,omitempty"`
	Policy         ExecutionPolicy `json:"policy,omitempty"`
}

// ExecutionQuoteRequest states which whole cents a quote lane (unique_tag,
// condition_id, token_id) wants a post-only GTC buy resting at. executiond
// keeps a resting level that is still wanted, so it holds its queue place,
// cancels one no longer wanted and posts a new one. See docs/protocol/nats-v1.md.
type ExecutionQuoteRequest struct {
	SchemaVersion string `json:"schema_version"`
	RequestID     string `json:"request_id,omitempty"`
	UniqueTag     string `json:"unique_tag"`
	Strategy      string `json:"strategy"`
	MarketID      string `json:"market_id,omitempty"`
	EventSlug     string `json:"event_slug,omitempty"`
	ConditionID   string `json:"condition_id"`
	TokenID       string `json:"token_id"`
	Outcome       string `json:"outcome"`
	Side          Side   `json:"side"`
	// Seq orders a lane's requests: one applies only when its Seq is above
	// the last one applied.
	Seq int64 `json:"seq"`
	// Levels are whole-cent prices, highest first; empty means nothing rests.
	Levels               []string  `json:"levels"`
	TargetUSD            string    `json:"target_usd,omitempty"`
	TargetEquityFraction string    `json:"target_equity_fraction,omitempty"`
	PostOnly             bool      `json:"post_only"`
	ExpiresAt            time.Time `json:"expires_at"`
	SignalPrice          string    `json:"signal_price,omitempty"`
	MaxSignalDrift       string    `json:"max_signal_drift,omitempty"`
	FeatureSeq           int64     `json:"feature_seq,omitempty"`
	FeatureCompletedAt   time.Time `json:"feature_completed_at,omitzero"`
	CreatedAt            time.Time `json:"created_at,omitzero"`
	Trigger              string    `json:"trigger,omitempty"`
	TickVisibleAt        time.Time `json:"tick_visible_at,omitzero"`
}

// QuoteStatusFilled is an ExecutionQuoteResult's only status.
const QuoteStatusFilled = "FILLED"

// ExecutionQuoteResult reports the first fill that ended a quote lane. It is
// published once per lane per wallet; nothing else is published for a quote.
type ExecutionQuoteResult struct {
	SchemaVersion string    `json:"schema_version"`
	RequestID     string    `json:"request_id,omitempty"`
	UniqueTag     string    `json:"unique_tag"`
	Strategy      string    `json:"strategy,omitempty"`
	WalletAddress string    `json:"wallet_address,omitempty"`
	SignerAddress string    `json:"signer_address,omitempty"`
	ConditionID   string    `json:"condition_id"`
	TokenID       string    `json:"token_id"`
	Outcome       string    `json:"outcome"`
	Side          Side      `json:"side"`
	Status        string    `json:"status"`
	Price         string    `json:"price,omitempty"`
	FilledShares  float64   `json:"filled_shares"`
	AveragePrice  float64   `json:"average_price,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

func PublishExecutionQuoteResult(publisher ExecutionEventPublisher, result ExecutionQuoteResult) error {
	if publisher == nil {
		return nil
	}
	result.SchemaVersion = SchemaVersionV1
	result.Status = QuoteStatusFilled
	result.OccurredAt = result.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionQuoteResult, result)
}

type InventoryAction string

const (
	InventorySplit InventoryAction = "SPLIT"
	InventoryMerge InventoryAction = "MERGE"
)

// ExecutionInventoryRequest manages the split inventory of a strategy lane
// (unique_tag, condition_id). Shares are outcome-token shares, one per pUSD.
//
// SPLIT tops the lane's remaining inventory - the pairs still free to back a
// SPLIT_PRESERVE open - up to TargetShares, splitting only the shortfall, so
// a resent request splits nothing and a later one refills what entries used.
// It never shrinks inventory. MERGE cancels the lane's resting preserves and
// merges Shares pairs back into pUSD, all of them when Shares is empty; after
// it the lane takes no further SPLIT.
type ExecutionInventoryRequest struct {
	SchemaVersion string          `json:"schema_version"`
	RequestID     string          `json:"request_id,omitempty"`
	UniqueTag     string          `json:"unique_tag"`
	Strategy      string          `json:"strategy"`
	MarketID      string          `json:"market_id,omitempty"`
	ConditionID   string          `json:"condition_id"`
	Action        InventoryAction `json:"action"`
	TargetShares  string          `json:"target_shares,omitempty"`
	Shares        string          `json:"shares,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     time.Time       `json:"expires_at,omitzero"`
}

// ExecutionInventoryResult reports one inventory request. Shares is how many
// pairs it split or merged (0 for a SPLIT already at target) and
// RemainingShares the lane's remaining inventory afterwards.
type ExecutionInventoryResult struct {
	SchemaVersion   string          `json:"schema_version"`
	RequestID       string          `json:"request_id,omitempty"`
	UniqueTag       string          `json:"unique_tag"`
	Strategy        string          `json:"strategy,omitempty"`
	WalletAddress   string          `json:"wallet_address,omitempty"`
	SignerAddress   string          `json:"signer_address,omitempty"`
	ConditionID     string          `json:"condition_id"`
	Action          InventoryAction `json:"action"`
	Status          ResultStatus    `json:"status"`
	ReasonCode      string          `json:"reason_code,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Shares          float64         `json:"shares"`
	RemainingShares float64         `json:"remaining_shares"`
	TxHash          string          `json:"tx_hash,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
}

func PublishExecutionInventoryResult(publisher ExecutionEventPublisher, result ExecutionInventoryResult) error {
	if publisher == nil {
		return nil
	}
	result.SchemaVersion = SchemaVersionV1
	result.OccurredAt = result.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionInventoryResult, result)
}

type ExecutionCloseMode string

const (
	ExecutionCloseModeLimit ExecutionCloseMode = "LIMIT_CLOSE"
	ExecutionCloseModeForce ExecutionCloseMode = "FORCE_CLOSE"
	// ExecutionCloseModeCancelOpen withdraws a working entry without selling
	// anything. It rides the close subject because that is where a working BUY
	// is already cancelled — a FORCE_CLOSE does it before it sells — rather
	// than on a subject of its own every consumer would have to learn. It
	// needs no limit price, touches no position, and a lane with no working
	// open order is a no-op. It publishes no close result: the cancelled
	// entry's own open result still reports how much of it filled.
	ExecutionCloseModeCancelOpen ExecutionCloseMode = "CANCEL_OPEN"
)

type ExecutionCloseRequest struct {
	SchemaVersion string             `json:"schema_version"`
	RequestID     string             `json:"request_id,omitempty"` // echoed on its results, as for an open
	UniqueTag     string             `json:"unique_tag"`
	Strategy      string             `json:"strategy"`
	ConditionID   string             `json:"condition_id"`
	AssetID       string             `json:"asset_id"`
	Outcome       string             `json:"outcome"`
	Mode          ExecutionCloseMode `json:"mode"`
	LimitPrice    string             `json:"limit_price,omitempty"`
	TimeInForce   TimeInForce        `json:"time_in_force,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	Policy        ExecutionPolicy    `json:"policy,omitempty"`
}

// Reason codes carried by result messages. They are part of the wire contract,
// so they are declared once here rather than derived from error text.
const (
	ReasonInvalidIntent         = "INVALID_INTENT"
	ReasonUnsupportedStyle      = "UNSUPPORTED_EXECUTION_STYLE"
	ReasonUnimplementedPolicy   = "UNIMPLEMENTED_POLICY"
	ReasonNoPosition            = "NO_POSITION"
	ReasonActiveSellReservation = "ACTIVE_SELL_RESERVATION"
	ReasonExposureLimit         = "EXPOSURE_LIMIT"
	ReasonDrawdownLimit         = "DRAWDOWN_LIMIT"
	ReasonBelowMinOrderSize     = "BELOW_MIN_ORDER_SIZE"
	ReasonUnplannable           = "UNPLANNABLE"
	ReasonOrderRejected         = "ORDER_REJECTED"
	ReasonExecutionFailed       = "EXECUTION_FAILED"
	// ReasonSignalDrift fails an open the market moved away from: refused
	// before submission, or cancelled unfilled while it rested. The code is
	// the one pmm's backtest venue reports for the same decision.
	ReasonSignalDrift = "SIGNAL_DRIFT"
	// ReasonExpired fails an order that rested unfilled until its deadline:
	// executiond cancelled it there, or the exchange expired it.
	ReasonExpired = "EXPIRED"
	// ReasonCanceled fails an unfilled order cancelled for any other cause:
	// the strategy withdrew it, a force close superseded it, or the exchange
	// cancelled it on its own.
	ReasonCanceled = "CANCELED"
	// ReasonInsufficientInventory fails a SPLIT_PRESERVE open that found no
	// free inventory before its deadline.
	ReasonInsufficientInventory = "INSUFFICIENT_INVENTORY"
	// ReasonInventoryCapped marks a SPLIT_PRESERVE open, or a SPLIT, cut
	// short by the inventory or the free cash. Its status is still SUCCEEDED
	// when anything filled or split.
	ReasonInventoryCapped = "INVENTORY_CAPPED"
	// ReasonInventoryClosed refuses a SPLIT on a lane already merged, or on a
	// market that has closed.
	ReasonInventoryClosed = "INVENTORY_CLOSED"
	// ReasonRelayerFailed fails an inventory request the relayer refused or
	// failed. ReasonRelayerUnknown fails one whose submission outcome is
	// unknown: it may still land, and executiond resolves it from the chain.
	ReasonRelayerFailed  = "RELAYER_FAILED"
	ReasonRelayerUnknown = "RELAYER_UNKNOWN"
)

type ResultStatus string

const (
	ResultSucceeded ResultStatus = "SUCCEEDED"
	ResultFailed    ResultStatus = "FAILED"
)

type ExecutionOpenResult struct {
	SchemaVersion string       `json:"schema_version"`
	RequestID     string       `json:"request_id,omitempty"`
	UniqueTag     string       `json:"unique_tag"`
	Strategy      string       `json:"strategy,omitempty"`
	WalletAddress string       `json:"wallet_address,omitempty"`
	SignerAddress string       `json:"signer_address,omitempty"`
	ConditionID   string       `json:"condition_id"`
	TokenID       string       `json:"token_id"`
	Outcome       string       `json:"outcome"`
	Side          Side         `json:"side"`
	Status        ResultStatus `json:"status"`
	ReasonCode    string       `json:"reason_code,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	FilledShares  float64      `json:"filled_shares,omitempty"`
	AveragePrice  float64      `json:"average_price,omitempty"`
	OccurredAt    time.Time    `json:"occurred_at"`
}

type ExecutionCloseResult struct {
	SchemaVersion string `json:"schema_version"`
	RequestID     string `json:"request_id,omitempty"`
	// Mode is the close mode the result answers. Only a LIMIT_CLOSE produces
	// one today (a FORCE_CLOSE is confirmed from the position, a CANCEL_OPEN
	// by its open's own result), but a reader should not have to know that.
	Mode          ExecutionCloseMode `json:"mode,omitempty"`
	UniqueTag     string             `json:"unique_tag"`
	Strategy      string             `json:"strategy,omitempty"`
	WalletAddress string             `json:"wallet_address,omitempty"`
	SignerAddress string             `json:"signer_address,omitempty"`
	ConditionID   string             `json:"condition_id"`
	AssetID       string             `json:"asset_id"`
	Outcome       string             `json:"outcome"`
	Side          Side               `json:"side"`
	Status        ResultStatus       `json:"status"`
	ReasonCode    string             `json:"reason_code,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	FilledShares  float64            `json:"filled_shares,omitempty"`
	AveragePrice  float64            `json:"average_price,omitempty"`
	OccurredAt    time.Time          `json:"occurred_at"`
}

func PublishExecutionOpenResult(publisher ExecutionEventPublisher, result ExecutionOpenResult) error {
	if publisher == nil {
		return nil
	}
	result.SchemaVersion = SchemaVersionV1
	result.OccurredAt = result.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionOpenResult, result)
}

func PublishExecutionCloseResult(publisher ExecutionEventPublisher, result ExecutionCloseResult) error {
	if publisher == nil {
		return nil
	}
	result.SchemaVersion = SchemaVersionV1
	result.OccurredAt = result.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionCloseResult, result)
}

type ExecutionOrderEvent struct {
	SchemaVersion   string    `json:"schema_version"`
	IntentID        string    `json:"intent_id"`
	UniqueTag       string    `json:"unique_tag,omitempty"`
	Strategy        string    `json:"strategy,omitempty"`
	WalletAddress   string    `json:"wallet_address,omitempty"`
	SignerAddress   string    `json:"signer_address,omitempty"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"`
	State           string    `json:"state"`
	Reason          string    `json:"reason,omitempty"`
	MatchedShares   float64   `json:"matched_shares,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

// PublishExecutionOrderEvent emits one order-lifecycle event. uniqueTag is the
// lane the order belongs to and strategy the one that asked for it, so a
// strategy can filter the shared subject to its own orders; both may be empty
// for an event whose intent could not be resolved.
func PublishExecutionOrderEvent(publisher ExecutionEventPublisher, orderID string, orderState string, intentID, uniqueTag, strategy, matchedShares, reason string, occurredAt time.Time) error {
	if publisher == nil {
		return nil
	}
	parsedShares, err := strconv.ParseFloat(matchedShares, 64)
	if err != nil {
		return fmt.Errorf("invalid matched_shares %q: %w", matchedShares, err)
	}
	return publisher.PublishJSON(SubjectExecutionOrderEvent, ExecutionOrderEvent{
		SchemaVersion:   SchemaVersionV1,
		IntentID:        intentID,
		UniqueTag:       uniqueTag,
		Strategy:        strategy,
		ExchangeOrderID: orderID,
		State:           orderState,
		Reason:          reason,
		MatchedShares:   parsedShares,
		OccurredAt:      occurredAt.UTC(),
	})
}

type PositionQueryRequest struct {
	SchemaVersion string `json:"schema_version"`
	ConditionID   string `json:"condition_id,omitempty"`
	MarketID      string `json:"market_id,omitempty"`
	UniqueTag     string `json:"unique_tag,omitempty"`
}

// PositionQueryResponse lists the positions of one wallet. Every executiond
// on the bus answers a query, so the reply names the wallet it speaks for and
// the strategies that wallet trades.
type PositionQueryResponse struct {
	SchemaVersion     string            `json:"schema_version"`
	WalletAddress     string            `json:"wallet_address,omitempty"`
	SignerAddress     string            `json:"signer_address,omitempty"`
	AllowedStrategies []string          `json:"allowed_strategies"`
	Positions         []PositionFeature `json:"positions"`
	Error             string            `json:"error,omitempty"`
}

type PositionFeature struct {
	SchemaVersion     string     `json:"schema_version"`
	Seq               int64      `json:"seq"`
	UniqueTag         string     `json:"unique_tag,omitempty"`
	WalletAddress     string     `json:"wallet_address,omitempty"`
	SignerAddress     string     `json:"signer_address,omitempty"`
	MarketID          string     `json:"market_id,omitempty"`
	ConditionID       string     `json:"condition_id"`
	TokenID           string     `json:"token_id"`
	Outcome           string     `json:"outcome"`
	HasPosition       bool       `json:"has_position"`
	EntryPrice        *float64   `json:"entry_price"`
	EntryTime         *time.Time `json:"entry_time"`
	SecondsSinceEntry float64    `json:"seconds_since_entry"`
	PositionSize      float64    `json:"position_size,omitempty"`
	OpenLots          int        `json:"open_lots,omitempty"`
	ActualShares      float64    `json:"actual_shares,omitempty"`
	AvailableSize     float64    `json:"available_size,omitempty"`
	ReservedSize      float64    `json:"reserved_size,omitempty"`
	State             string     `json:"state,omitempty"`
	SourceRevision    int64      `json:"source_revision,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
	PublishedAt       time.Time  `json:"published_at"`
}

type BalanceQueryRequest struct {
	SchemaVersion string `json:"schema_version"`
}

// BalanceQueryResponse values the wallet: USDC cash plus recorded positions
// marked at the best bid. Positions with no fresh quote are valued at their
// entry price and counted in UnmarkedPositions, unless their market has
// resolved (SettledPositions). Every executiond on the bus answers a query, so
// the reply names the wallet it values and the strategies that wallet trades.
type BalanceQueryResponse struct {
	SchemaVersion     string    `json:"schema_version"`
	WalletAddress     string    `json:"wallet_address,omitempty"`
	SignerAddress     string    `json:"signer_address,omitempty"`
	AllowedStrategies []string  `json:"allowed_strategies"`
	CashUSD           float64   `json:"cash_usd"`
	PositionsValueUSD float64   `json:"positions_value_usd"`
	EquityUSD         float64   `json:"equity_usd"`
	Positions         int       `json:"positions"`
	SettledPositions  int       `json:"settled_positions"`
	UnmarkedPositions int       `json:"unmarked_positions"`
	CashAsOf          time.Time `json:"cash_as_of"`
	AsOf              time.Time `json:"as_of"`
	Error             string    `json:"error,omitempty"`
}
