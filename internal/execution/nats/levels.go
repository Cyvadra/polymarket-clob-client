package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// marketLevels is pmm.market.levels as the collector publishes it
// (pmm's messaging.MarketLevels): one market's two books at a flush, sent
// only when clob.levels is "publish" on the collector. The depth figures it
// also carries are not read here.
type marketLevels struct {
	ConditionID string    `json:"condition_id"`
	At          time.Time `json:"at"`
	Up          levelTop  `json:"up"`
	Down        levelTop  `json:"down"`
}

type levelTop struct {
	AssetID   string    `json:"asset_id"`
	Bid       float64   `json:"bid"` // 0 is an empty side
	Ask       float64   `json:"ask"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SubscribeLevels keeps cache at the top of each market's maintained order
// book. Unlike pmm.market.quotes, the book moves only when makers add or pull
// orders, never on a print, so it is what the drift check reads first.
func SubscribeLevels(bus Subscriber, cache *marketquotes.Cache) error {
	if bus == nil || cache == nil {
		return fmt.Errorf("NATS subscriber and book cache are required")
	}
	return bus.Subscribe(protocol.SubjectMarketLevels, func(_ context.Context, payload []byte) error {
		levels, err := natsbus.DecodeJSON[marketLevels](payload)
		if err != nil {
			return err
		}
		// The cache holds two-sided prices only. A book with an empty side
		// is not stored: the market's last stored book ages out and the
		// quote stream decides, as it does without levels at all.
		if !levels.Up.twoSided() || !levels.Down.twoSided() {
			return nil
		}
		return cache.Put(levels.snapshot())
	})
}

func (t levelTop) twoSided() bool { return t.Bid > 0 && t.Ask > 0 }

func (m marketLevels) snapshot() marketquotes.Snapshot {
	return marketquotes.Snapshot{ConditionID: m.ConditionID, At: m.At, Up: m.Up.quote(m.At), Down: m.Down.quote(m.At)}
}

// quote is a two-sided side as a Quote, stamped with its own last change or,
// lacking one, the flush.
func (t levelTop) quote(at time.Time) marketquotes.Quote {
	q := marketquotes.Quote{AssetID: t.AssetID, Bid: t.Bid, Ask: t.Ask, Mid: (t.Bid + t.Ask) / 2, Timestamp: t.UpdatedAt}
	if q.Timestamp.IsZero() {
		q.Timestamp = at
	}
	return q
}
