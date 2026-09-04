// Package marketquotes owns the latest PMM market quote snapshots used by execution tactics.
package marketquotes

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type Subscriber interface {
	Subscribe(string, natsbus.Handler) error
}

type Cache struct {
	mu     sync.RWMutex
	quotes map[string]contracts.MarketQuotes
}

func New() *Cache {
	return &Cache{quotes: make(map[string]contracts.MarketQuotes)}
}

func (c *Cache) Put(quotes contracts.MarketQuotes) error {
	if err := Validate(quotes); err != nil {
		return err
	}
	quotes.MarketID = strings.TrimSpace(quotes.MarketID)
	quotes.At = quotes.At.UTC()
	quotes.Up.Timestamp = quotes.Up.Timestamp.UTC()
	quotes.Down.Timestamp = quotes.Down.Timestamp.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.quotes[quotes.MarketID]; ok && existing.At.After(quotes.At) {
		return nil
	}
	c.quotes[quotes.MarketID] = quotes
	return nil
}

func (c *Cache) Get(marketID string) (contracts.MarketQuotes, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	quotes, ok := c.quotes[strings.TrimSpace(marketID)]
	return quotes, ok
}

func Subscribe(bus Subscriber, cache *Cache) error {
	if bus == nil || cache == nil {
		return fmt.Errorf("NATS subscriber and market quote cache are required")
	}
	return bus.Subscribe(contracts.SubjectPMMMarketQuotes, func(_ context.Context, payload []byte) error {
		quotes, err := natsbus.DecodeJSON[contracts.MarketQuotes](payload)
		if err != nil {
			return err
		}
		return cache.Put(quotes)
	})
}

func Validate(quotes contracts.MarketQuotes) error {
	if strings.TrimSpace(quotes.MarketID) == "" {
		return fmt.Errorf("market quote market ID is required")
	}
	if quotes.At.IsZero() {
		return fmt.Errorf("market quote timestamp is required")
	}
	if err := validateSide("up", quotes.Up); err != nil {
		return err
	}
	return validateSide("down", quotes.Down)
}

func validateSide(name string, quote contracts.MarketQuote) error {
	if quote.Timestamp.IsZero() {
		return fmt.Errorf("%s quote timestamp is required", name)
	}
	if !validPrice(quote.Bid) || !validPrice(quote.Ask) || !validPrice(quote.Mid) {
		return fmt.Errorf("%s quote prices must be finite values between zero and one", name)
	}
	if quote.Bid > quote.Ask {
		return fmt.Errorf("%s quote bid exceeds ask", name)
	}
	if quote.Mid < quote.Bid || quote.Mid > quote.Ask {
		return fmt.Errorf("%s quote mid is outside bid/ask", name)
	}
	return nil
}

func validPrice(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0 && value < 1
}
