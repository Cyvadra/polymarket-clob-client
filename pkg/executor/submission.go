package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor/tactics"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// plannedChild is one child order about to be reserved, signed, and submitted.
type plannedChild struct {
	Sequence          int
	Shares            string
	Price             string
	PostOnly          bool
	TimeInForce       protocol.TimeInForce
	ReservationReason string
}

// newExecutionID returns a server-side unique execution identity. Signals are
// delivered at most once and never replayed, so no client-supplied idempotency
// key is needed; a random ID keeps every execution independent.
func newExecutionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("exec-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func (e *Executor) ExecuteOpen(ctx context.Context, req protocol.ExecutionOpenRequest) error {
	intent := protocol.ExecutionIntent{
		SchemaVersion:      req.SchemaVersion,
		IntentID:           newExecutionID(),
		UniqueTag:          req.UniqueTag,
		Strategy:           req.Strategy,
		Kind:               protocol.IntentOpen,
		MarketID:           req.MarketID,
		EventSlug:          req.EventSlug,
		ConditionID:        req.ConditionID,
		TokenID:            req.TokenID,
		Outcome:            req.Outcome,
		Side:               req.Side,
		TargetUSD:          req.TargetUSD,
		LimitPrice:         req.LimitPrice,
		TimeInForce:        req.TimeInForce,
		PostOnly:           req.PostOnly,
		FeatureSeq:         req.FeatureSeq,
		FeatureCompletedAt: req.FeatureCompletedAt,
		CreatedAt:          req.CreatedAt,
		ExpiresAt:          req.ExpiresAt,
		Policy:             req.Policy,
	}
	if err := e.Execute(ctx, intent); err != nil {
		var unresolved unresolvedSubmission
		if !errors.As(err, &unresolved) {
			e.publishOpenRejection(intent, err)
		}
		return err
	}
	return nil
}

// Execute plans, reserves, signs, and submits an intent's child order under the
// intent's advisory lock. It returns an error without publishing: each caller
// owns result publication so open and close intents report on their own result
// subjects.
func (e *Executor) Execute(ctx context.Context, intent protocol.ExecutionIntent) error {
	if err := validateIntentAt(intent, e.now().UTC()); err != nil {
		return err
	}
	return e.store.WithIntentLock(ctx, intent.IntentID, func(ctx context.Context) error {
		return e.persistAndSubmit(ctx, intent)
	})
}

// persistAndSubmit durably persists the intent and then plans and submits its
// strategy child. Callers that already hold an intent-scoped advisory lock (the
// close lane lock in ExecuteClose) call this directly so the child is placed
// without nesting a second lock transaction on a second pool connection.
func (e *Executor) persistAndSubmit(ctx context.Context, intent protocol.ExecutionIntent) error {
	if _, err := e.store.InsertIntent(ctx, mapping.IntentRecord(intent, e.now().UTC())); err != nil {
		return fmt.Errorf("persist intent: %w", err)
	}
	return e.prepareAndSubmit(ctx, intent)
}

func (e *Executor) publishOpenRejection(intent protocol.ExecutionIntent, err error) {
	status, code, reason := reasonFor(err)
	e.publishOpenResult(intent, status, code, reason, "", "")
}

func (e *Executor) publishOpenResult(intent protocol.ExecutionIntent, status protocol.ResultStatus, code, reason, filledShares, averagePrice string) {
	filled, err := decimal.NonNegativeFloat(filledShares)
	if err != nil {
		filled = 0
	}
	average, err := decimal.Price(averagePrice)
	if err != nil {
		average = 0
	}
	if err := protocol.PublishExecutionOpenResult(e.publish, protocol.ExecutionOpenResult{
		UniqueTag: intent.UniqueTag, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome, Side: intent.Side,
		Status: status, ReasonCode: code, Reason: reason,
		FilledShares: filled, AveragePrice: average, OccurredAt: e.now(),
	}); err != nil && e.onError != nil {
		e.onError(fmt.Errorf("publish open result: %w", err))
	}
}

// submitSignedOrder resumes a child order that was persisted as signed but
// whose submission outcome was never recorded.
func (e *Executor) submitSignedOrder(ctx context.Context, intent protocol.ExecutionIntent, order store.SignedOrderRecord) error {
	reservation, err := e.store.Reservation(ctx, reservationID(intent.IntentID, order.ChildSequence))
	if err != nil {
		return fmt.Errorf("load signed order reservation: %w", err)
	}
	if reservation.State != "active" {
		return fmt.Errorf("signed order reservation is not active")
	}
	var signed clobclient.SignedOrderV2
	if err := json.Unmarshal(order.SignedPayload, &signed); err != nil {
		return fmt.Errorf("decode persisted signed order: %w", err)
	}
	child := plannedChild{
		Sequence: order.ChildSequence, Shares: order.RequestedShares, Price: order.Price,
		PostOnly: order.PostOnly, TimeInForce: protocol.TimeInForce(order.OrderType),
	}
	return e.submitOrder(ctx, intent, signed, child, order.Revision)
}

func (e *Executor) prepareAndSubmit(ctx context.Context, intent protocol.ExecutionIntent) error {
	child, err := e.planInitialChild(ctx, intent)
	if err != nil {
		return err
	}
	return e.placeChild(ctx, intent, child)
}

// placeChild reserves exposure, signs, persists, and submits one child order.
// It is the single path to the exchange: both a strategy intent and an
// internal force-close exit go through it.
func (e *Executor) placeChild(ctx context.Context, intent protocol.ExecutionIntent, child plannedChild) error {
	reservationID := reservationID(intent.IntentID, child.Sequence)
	if err := e.reserveChild(ctx, intent, child, reservationID); err != nil {
		return err
	}
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			_ = e.store.Release(context.Background(), reservationID, "prepare order failed")
		}
	}()
	order, err := userOrder(intent, child)
	if err != nil {
		return err
	}
	signed, err := e.clob.CreateOrder(ctx, order)
	if err != nil {
		return fmt.Errorf("sign order: %w", err)
	}
	payload, err := json.Marshal(signed)
	if err != nil {
		return fmt.Errorf("marshal signed order: %w", err)
	}
	hash := sha256.Sum256(payload)
	now := e.now().UTC()
	if err := e.store.PersistSignedOrder(ctx, store.SignedOrderRecord{
		IntentID: intent.IntentID, ChildSequence: child.Sequence, SignedPayload: payload, SignedOrderHash: fmt.Sprintf("%x", hash[:]),
		Salt: strconv.FormatInt(signed.Salt, 10), ExchangeOrderID: signed.OrderID, RequestedShares: child.Shares, Price: child.Price,
		OrderType: store.TimeInForce(child.TimeInForce), PostOnly: child.PostOnly, State: statemachine.StateSigned, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("persist signed order: %w", err)
	}
	releaseOnFailure = false
	return e.submitOrder(ctx, intent, signed, child, 1)
}

// reserveChild locks the child's exposure, tolerating a reservation this same
// intent already owns from an interrupted attempt.
func (e *Executor) reserveChild(ctx context.Context, intent protocol.ExecutionIntent, child plannedChild, reservationID string) error {
	err := e.store.Reserve(ctx, reservationRecord(intent, child, reservationID, e.now().UTC()))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrDuplicate):
		reservation, loadErr := e.store.Reservation(ctx, reservationID)
		if loadErr != nil {
			return fmt.Errorf("load duplicate reservation: %w", loadErr)
		}
		if reservation.State != "active" || reservation.IntentID != intent.IntentID {
			return fmt.Errorf("duplicate reservation is not an active reservation for this intent")
		}
		return nil
	case errors.Is(err, store.ErrConflict):
		return rejection{code: protocol.ReasonNoPosition, reason: "no available position for this intent", cause: err}
	case errors.Is(err, store.ErrActiveSellReservation):
		return rejection{code: protocol.ReasonActiveSellReservation, reason: "active sell reservation already exists", cause: err}
	case errors.Is(err, store.ErrExposureLimit):
		return rejection{code: protocol.ReasonExposureLimit, reason: "order exceeds the configured open exposure limit", cause: err}
	default:
		return fmt.Errorf("reserve order exposure: %w", err)
	}
}

func (e *Executor) planInitialChild(ctx context.Context, intent protocol.ExecutionIntent) (plannedChild, error) {
	market, err := e.marketRules(ctx, intent.TokenID)
	if err != nil {
		return plannedChild{}, err
	}
	request := tactics.Request{Intent: intent, Market: market, Now: e.now().UTC()}
	if intent.Side == protocol.SideSell {
		position, found, err := e.positionFor(ctx, intent.ConditionID, intent.TokenID, intent.UniqueTag)
		if err != nil {
			return plannedChild{}, err
		}
		if !found {
			return plannedChild{}, rejection{code: protocol.ReasonNoPosition, reason: "no position held for this token"}
		}
		request.AvailableShares = position.ActualShares
	}
	if e.quotes != nil {
		request.Quote, request.HasQuote = e.quotes.Get(intent.ConditionID)
	}
	decision, err := tactics.Plan(request)
	if err != nil {
		return plannedChild{}, rejection{code: protocol.ReasonUnplannable, reason: err.Error(), cause: err}
	}
	return plannedChild{
		Sequence: store.StrategyChildSequence, Shares: decision.Shares, Price: decision.Price,
		PostOnly: decision.PostOnly, TimeInForce: decision.TimeInForce,
		ReservationReason: "execution intent accepted",
	}, nil
}

func (e *Executor) marketRules(ctx context.Context, tokenID string) (tactics.Market, error) {
	tick, err := e.clob.TickSize(ctx, tokenID)
	if err != nil {
		return tactics.Market{}, fmt.Errorf("load tick size for %s: %w", tokenID, err)
	}
	return tactics.Market{TickSize: tick}, nil
}

func (e *Executor) submitOrder(ctx context.Context, intent protocol.ExecutionIntent, signed clobclient.SignedOrderV2, child plannedChild, revision int64) error {
	order := store.SignedOrderRecord{
		IntentID: intent.IntentID, ChildSequence: child.Sequence, State: statemachine.StateSigned,
		Revision: revision, MatchedShares: "0", RequestedShares: child.Shares,
	}
	updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitStarted, "0", "", "submit requested")
	if err != nil {
		return fmt.Errorf("mark submitting: %w", err)
	}
	e.publishTransition(updated, intent.UniqueTag, "submit requested")
	response, submitErr := e.clob.SubmitSignedOrder(ctx, signed, child.TimeInForce, child.PostOnly)
	if submitErr != nil {
		if isOrderRejected(submitErr) {
			return e.markSubmitRejected(ctx, updated, intent.UniqueTag, submitErr)
		}
		return e.markSubmitUnknown(ctx, updated, intent.UniqueTag, submitErr.Error())
	}
	if response == nil || response.OrderID == "" {
		return e.markSubmitUnknown(ctx, updated, intent.UniqueTag, "successful submission missing exchange order ID")
	}
	if updated.ExchangeOrderID != "" && response.OrderID != updated.ExchangeOrderID {
		return e.markSubmitUnknown(ctx, updated, intent.UniqueTag, "submission returned unexpected exchange order ID")
	}
	live, err := e.store.TransitionOrder(ctx, updated, statemachine.EventSubmitAcknowledged, "0", response.OrderID, "submit acknowledged")
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return e.resolveSubmitObservation(ctx, intent, child, response.OrderID)
		}
		return fmt.Errorf("mark order live: %w", err)
	}
	e.publishTransition(live, intent.UniqueTag, "submit acknowledged")
	e.settleFromResponse(ctx, intent, child, live, response)
	return nil
}

// settleFromResponse applies the match a submission response reports, so an
// order that crossed the book on arrival does not sit LIVE until the user
// stream or the next reconcile pass notices. A FAK or FOK order cannot rest:
// its match is final, filled or canceled. A resting order matched in full is
// filled; one matched in part is partially filled and its remainder rests.
// Any status other than "matched" is left to those observers.
//
// "matched" is the CLOB's off-chain match, not on-chain settlement, which can
// lag or fail. This only advances the order and its result; the position still
// moves only with fills, which track their trade's settlement and are reversed
// if it fails. The order is already acknowledged, so a failure here is
// reported, not returned.
func (e *Executor) settleFromResponse(ctx context.Context, intent protocol.ExecutionIntent, child plannedChild, order store.SignedOrderRecord, response *clobclient.OrderResponse) {
	if !strings.EqualFold(strings.TrimSpace(response.Status), "matched") {
		return
	}
	// The maker amount is what the order gives and the taker amount what it
	// receives, so shares are the maker amount of a SELL and the taker amount
	// of a BUY.
	matched := response.MakingAmount
	if intent.Side == protocol.SideBuy {
		matched = response.TakingAmount
	}
	if !decimal.Positive(matched) {
		return
	}
	// Compare against the size actually signed, not the unfloored plan, or a
	// complete fill of a floored order reads as partial.
	requested, ok := decimal.FloorTo(child.Shares, clobclient.SharePrecisionDigits(intent.Side, child.TimeInForce))
	if !ok {
		requested = child.Shares
	}
	event, _ := statemachine.EventForOrderObservation("MATCHED", matched, requested, statemachine.Immediate(string(child.TimeInForce)))
	const reason = "submit response MATCHED"
	updated, err := e.store.TransitionOrder(ctx, order, event, matched, order.ExchangeOrderID, reason)
	if errors.Is(err, store.ErrConflict) {
		// A concurrent observer already advanced the order and reports it.
		return
	}
	if err != nil {
		e.reportError(fmt.Errorf("settle order %s from submit response: %w", order.ExchangeOrderID, err))
		return
	}
	e.publishTransition(updated, intent.UniqueTag, reason)
	if !statemachine.IsTerminal(updated.State) {
		// A partly matched resting order is still working; its result comes
		// when an observer sees it finish.
		return
	}
	record, err := e.store.Intent(ctx, updated.IntentID)
	if err != nil {
		e.reportError(fmt.Errorf("load intent for submit response result: %w", err))
		return
	}
	// The collateral side of the match is the taker amount of a SELL and the
	// maker amount of a BUY; collateral over shares is the average price.
	collateral := response.MakingAmount
	if intent.Side == protocol.SideSell {
		collateral = response.TakingAmount
	}
	average, _ := decimal.DivideAndRoundDown(collateral, matched, 6)
	if err := accountfeed.PublishTerminalResult(e.publish, record, updated, reason, average, e.now()); err != nil {
		e.reportError(err)
	}
}

func (e *Executor) reportError(err error) {
	if e.onError != nil {
		e.onError(err)
	}
}

// resolveSubmitObservation reconciles the case where a concurrent observer
// (the account user stream) already advanced the order past SUBMITTING before
// the REST submission response returned. That observer already emits terminal
// open results, so this path only verifies the concurrent observation matches
// the exchange order we submitted and does not publish a second result.
func (e *Executor) resolveSubmitObservation(ctx context.Context, intent protocol.ExecutionIntent, child plannedChild, exchangeOrderID string) error {
	current, err := e.store.OrderByIntent(ctx, intent.IntentID, child.Sequence)
	if err != nil {
		return fmt.Errorf("load concurrently observed order: %w", err)
	}
	if current.ExchangeOrderID != exchangeOrderID {
		return fmt.Errorf("mark order live: %w", store.ErrConflict)
	}
	return nil
}

// isOrderRejected reports whether the exchange definitively refused the order.
// A 200 response carrying success=false is one form; a 4xx is the other. The
// CLOB does not book an order it answers with 4xx (a geoblocked region, a
// malformed or unauthorized request), so treating those as an unknown outcome
// strands the order in a submit-timeout state waiting for a reconciliation
// that can never find it on the exchange. 408 and 429 are excluded: those may
// have reached the matching engine before the response was produced.
func isOrderRejected(err error) bool {
	var rejected *clobclient.OrderRejectedError
	if errors.As(err, &rejected) {
		return true
	}
	var apiErr *clobclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 &&
			apiErr.StatusCode != http.StatusRequestTimeout && apiErr.StatusCode != http.StatusTooManyRequests
	}
	return false
}

// rejectionReason keeps the exchange's own wording. For an HTTP rejection that
// is the status, path, and response body, which is what an operator needs to
// tell a geoblock apart from a malformed order.
func rejectionReason(err error) string {
	var rejected *clobclient.OrderRejectedError
	if errors.As(err, &rejected) {
		return rejected.Message
	}
	return err.Error()
}

func (e *Executor) markSubmitUnknown(ctx context.Context, order store.SignedOrderRecord, uniqueTag, reason string) error {
	unknown, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitTimedOut, order.MatchedShares, order.ExchangeOrderID, reason)
	if err != nil {
		return fmt.Errorf("mark submit unknown: %w", err)
	}
	e.publishTransition(unknown, uniqueTag, reason)
	return unresolvedSubmission{cause: fmt.Errorf("order submission outcome is unknown: %s", reason)}
}

// unresolvedSubmission marks an error whose order may in fact have reached
// the exchange despite the response indicating otherwise. The order is left
// in SUBMIT_UNKNOWN for the reconciler to resolve from REST truth, so no
// result is published for it here: a premature FAILED would contradict a
// SUCCEEDED the reconciler later reports once it learns the real outcome.
type unresolvedSubmission struct{ cause error }

func (e unresolvedSubmission) Error() string { return e.cause.Error() }
func (e unresolvedSubmission) Unwrap() error { return e.cause }

func (e *Executor) markSubmitRejected(ctx context.Context, order store.SignedOrderRecord, uniqueTag string, submitErr error) error {
	reason := rejectionReason(submitErr)
	rejected, err := e.store.TransitionOrder(ctx, order, statemachine.EventRejectedObserved, order.MatchedShares, order.ExchangeOrderID, reason)
	if err != nil {
		return fmt.Errorf("mark submit rejected: %w", err)
	}
	e.publishTransition(rejected, uniqueTag, reason)
	return rejection{code: protocol.ReasonOrderRejected, reason: reason, cause: submitErr}
}
