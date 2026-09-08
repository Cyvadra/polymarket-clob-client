package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const (
	// forceCloseChildSequence is the child sequence reserved for the internal
	// force-close sell appended to a close request.
	forceCloseChildSequence = store.StrategyChildSequence + 1
	forceClosePrice         = "0.01"
)

func (e *Executor) ExecuteClose(ctx context.Context, req protocol.ExecutionCloseRequest) error {
	if err := validateCloseRequest(req); err != nil {
		e.publishCloseResult(e.closeResult(req, protocol.ResultFailed, "INVALID_CLOSE", err.Error()))
		return err
	}
	intent, err := e.closeIntent(ctx, req)
	if err != nil {
		_, code, reason := reasonFor(err)
		e.publishCloseResult(e.closeResult(req, protocol.ResultFailed, code, reason))
		return err
	}
	if req.Mode == protocol.ExecutionCloseModeForce {
		if err := e.closeForce(ctx, intent); err != nil {
			_, code, reason := reasonFor(err)
			e.publishCloseResult(e.closeResult(req, protocol.ResultFailed, code, reason))
			return err
		}
		e.publishCloseResult(e.closeResult(req, protocol.ResultSucceeded, "", "force close dispatched"))
		return nil
	}
	if err := e.executeClose(ctx, intent); err != nil {
		_, code, reason := reasonFor(err)
		e.publishCloseResult(e.closeResult(req, protocol.ResultFailed, code, reason))
		return err
	}
	e.publishCloseResult(e.closeResult(req, protocol.ResultSucceeded, "", "close dispatched"))
	return nil
}

// closeResult builds a strategy-facing close result carrying the position
// identity so the strategy can attribute it without a correlation key. Close
// always exits the whole position on the sell side.
func (e *Executor) closeResult(req protocol.ExecutionCloseRequest, status protocol.ResultStatus, code, reason string) protocol.ExecutionCloseResult {
	return protocol.ExecutionCloseResult{
		ConditionID: req.ConditionID, AssetID: req.AssetID, Outcome: req.Outcome, Side: protocol.SideSell,
		Status: status, ReasonCode: code, Reason: reason, OccurredAt: e.now(),
	}
}

func validateCloseRequest(req protocol.ExecutionCloseRequest) error {
	if req.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("close request requires schema_version %q", protocol.SchemaVersionV1)
	}
	if strings.TrimSpace(req.Strategy) == "" || strings.TrimSpace(req.ConditionID) == "" || strings.TrimSpace(req.AssetID) == "" || strings.TrimSpace(req.Outcome) == "" {
		return fmt.Errorf("close request strategy, condition ID, asset ID, and outcome are required")
	}
	if req.Mode != protocol.ExecutionCloseModeLimit && req.Mode != protocol.ExecutionCloseModeForce {
		return fmt.Errorf("invalid close mode %q", req.Mode)
	}
	if req.Mode == protocol.ExecutionCloseModeLimit {
		if _, err := decimal.Price(req.LimitPrice); err != nil {
			return fmt.Errorf("invalid limit price %q", req.LimitPrice)
		}
	}
	return nil
}

func (e *Executor) closeIntent(ctx context.Context, req protocol.ExecutionCloseRequest) (protocol.ExecutionIntent, error) {
	position, found, err := e.positionFor(ctx, req.ConditionID, req.AssetID)
	if err != nil {
		return protocol.ExecutionIntent{}, err
	}
	if !found || !decimal.Positive(position.AvailableSize) {
		return protocol.ExecutionIntent{}, rejection{code: protocol.ReasonNoPosition, reason: "no available position to close"}
	}
	// Force close exits at a nominal 0.01; limit close uses the caller's limit.
	price := forceClosePrice
	if req.Mode == protocol.ExecutionCloseModeLimit {
		price = req.LimitPrice
	}
	intent := protocol.ExecutionIntent{
		SchemaVersion: req.SchemaVersion,
		IntentID:      newExecutionID(),
		Strategy:      req.Strategy,
		Kind:          protocol.IntentClose,
		ConditionID:   req.ConditionID,
		TokenID:       req.AssetID,
		Outcome:       req.Outcome,
		Side:          protocol.SideSell,
		LimitPrice:    price,
		TimeInForce:   req.TimeInForce,
		Policy:        req.Policy,
	}
	if req.Mode == protocol.ExecutionCloseModeForce {
		intent.TimeInForce = protocol.TimeInForceFAK
		intent.PostOnly = false
		intent.Policy.Style = protocol.ExecutionStyleTakerAggressive
	} else if intent.TimeInForce == "" {
		intent.TimeInForce = protocol.TimeInForceGTC
	}
	if req.Mode == protocol.ExecutionCloseModeLimit && intent.Policy.Style == "" {
		intent.Policy.Style = protocol.ExecutionStyleLimit
	}
	return intent, nil
}

func (e *Executor) executeClose(ctx context.Context, intent protocol.ExecutionIntent) error {
	if live, existing, err := e.findCloseableOpenOrder(ctx, intent.ConditionID, intent.TokenID); err != nil {
		return err
	} else if live {
		if err := e.cancelOpenOrder(ctx, existing.intent, existing.order); err != nil {
			return err
		}
	}
	return e.Execute(ctx, intent)
}

type closeableOrder struct {
	intent store.OrderIntentRecord
	order  store.SignedOrderRecord
}

func (e *Executor) findCloseableOpenOrder(ctx context.Context, conditionID, tokenID string) (bool, closeableOrder, error) {
	orders, err := e.store.OpenOrders(ctx)
	if err != nil {
		return false, closeableOrder{}, fmt.Errorf("load open orders: %w", err)
	}
	for _, order := range orders {
		if order.ChildSequence != store.StrategyChildSequence {
			continue
		}
		intent, err := e.store.Intent(ctx, order.IntentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return false, closeableOrder{}, fmt.Errorf("load order intent: %w", err)
		}
		if intent.ConditionID == conditionID && intent.TokenID == tokenID {
			if order.State == statemachine.StateLive || order.State == statemachine.StatePartiallyFilled || order.State == statemachine.StateCancelRequested {
				return true, closeableOrder{intent: intent, order: order}, nil
			}
		}
	}
	return false, closeableOrder{}, nil
}

func (e *Executor) closeForce(ctx context.Context, intent protocol.ExecutionIntent) error {
	if live, existing, err := e.findCloseableOpenOrder(ctx, intent.ConditionID, intent.TokenID); err != nil {
		return err
	} else if live {
		if err := e.cancelOpenOrder(ctx, existing.intent, existing.order); err != nil {
			return err
		}
	}
	return e.forceClosePosition(ctx, store.OrderIntentRecord{
		IntentID:    intent.IntentID,
		Strategy:    intent.Strategy,
		Kind:        store.IntentClose,
		ConditionID: intent.ConditionID,
		TokenID:     intent.TokenID,
		Outcome:     intent.Outcome,
		Side:        store.Side(intent.Side),
		LimitPrice:  intent.LimitPrice,
		TimeInForce: store.TimeInForce(intent.TimeInForce),
		PostOnly:    intent.PostOnly,
		Status:      statemachine.StateIntentReceived,
		CreatedAt:   e.now().UTC(),
	})
}

// forceClosePosition dispatches a best-effort 0.01 FAK exit for the whole
// remaining position. Per the close contract, force close reports success even
// if the sell never fills; only genuine infrastructure failures return an
// error. A vanished position or a competing active sell means there is nothing
// left for us to exit, which is likewise treated as a completed close.
func (e *Executor) forceClosePosition(ctx context.Context, intent store.OrderIntentRecord) error {
	position, found, err := e.positionFor(ctx, intent.ConditionID, intent.TokenID)
	if err != nil {
		return err
	}
	if !found || !decimal.Positive(position.AvailableSize) {
		return nil
	}
	child := plannedChild{Sequence: forceCloseChildSequence, Shares: position.ActualShares, Price: forceClosePrice, TimeInForce: protocol.TimeInForceFAK, ReservationReason: "force close"}
	exit := mapping.ExecutionIntent(intent)
	exit.Side = protocol.SideSell
	exit.PostOnly = false
	if err := e.placeChild(ctx, exit, child); err != nil {
		var declared rejection
		if errors.As(err, &declared) {
			switch declared.code {
			case protocol.ReasonActiveSellReservation, protocol.ReasonNoPosition:
				return nil // a competing sell or vanished position: nothing to exit
			}
		}
		return err
	}
	return nil
}

func (e *Executor) positionFor(ctx context.Context, conditionID, tokenID string) (store.PositionRecord, bool, error) {
	positions, err := e.store.PositionFeatures(ctx)
	if err != nil {
		return store.PositionRecord{}, false, fmt.Errorf("load positions: %w", err)
	}
	for _, candidate := range positions {
		if candidate.ConditionID == conditionID && candidate.TokenID == tokenID {
			return candidate, true, nil
		}
	}
	return store.PositionRecord{}, false, nil
}
