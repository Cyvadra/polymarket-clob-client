package executor

import (
	"context"
	"errors"
	"strconv"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

// EntrySizer resolves a fraction of wallet equity into a dollar target.
type EntrySizer interface {
	Size(ctx context.Context, fraction string) (equity.Entry, error)
}

// sizeEntry fills intent.TargetUSD for an open sized by
// target_equity_fraction. The returned release ends the entry's hold on free
// cash; it is a no-op for dollar-sized opens.
func (e *Executor) sizeEntry(ctx context.Context, req protocol.ExecutionOpenRequest, intent *protocol.ExecutionIntent) (func(), error) {
	if req.TargetEquityFraction == "" {
		return func() {}, nil
	}
	if req.TargetUSD != "" {
		return nil, invalid("target_usd and target_equity_fraction are mutually exclusive")
	}
	if req.Side != protocol.SideBuy {
		return nil, invalid("target_equity_fraction sizes buys only")
	}
	if e.sizer == nil {
		return nil, invalid("equity sizing is not enabled")
	}
	entry, err := e.sizer.Size(ctx, req.TargetEquityFraction)
	switch {
	case errors.Is(err, equity.ErrInvalidFraction):
		return nil, rejection{code: protocol.ReasonInvalidIntent, reason: err.Error(), cause: err}
	case errors.Is(err, equity.ErrInsufficientCash):
		return nil, rejection{code: protocol.ReasonExposureLimit, reason: err.Error(), cause: err}
	case err != nil:
		return nil, rejection{code: protocol.ReasonExecutionFailed, reason: "size entry from equity: " + err.Error(), cause: err}
	}
	intent.TargetUSD = entry.TargetUSD
	intent.TargetEquityFraction = req.TargetEquityFraction
	intent.SizedEquityUSD = strconv.FormatFloat(entry.EquityUSD, 'f', 6, 64)
	return entry.Release, nil
}

type cashHoldKey struct{}

// withCashHold carries an entry's free-cash release to placeChild, which calls
// it once the store reservation has taken the claim over.
func withCashHold(ctx context.Context, release func()) context.Context {
	return context.WithValue(ctx, cashHoldKey{}, release)
}

// releaseCashHold ends the entry's hold on free cash, if ctx carries one.
// The sizer's release is idempotent, so the deferred call in ExecuteOpen is
// then a no-op.
func releaseCashHold(ctx context.Context) {
	if release, ok := ctx.Value(cashHoldKey{}).(func()); ok {
		release()
	}
}
