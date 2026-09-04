// Package marketquotes owns the latest PMM market quote snapshots used by execution tactics.
package marketquotes

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

type Cache struct {
	mu     sync.RWMutex
	quotes map[string]Snapshot
}

func New() *Cache {
	return &Cache{quotes: make(map[string]Snapshot)}
}

func (c *Cache) Put(quotes Snapshot) error {
	if err := Validate(quotes); err != nil {
		return err
	}
	quotes.ConditionID = strings.TrimSpace(quotes.ConditionID)
	quotes.Up.AssetID = strings.TrimSpace(quotes.Up.AssetID)
	quotes.Down.AssetID = strings.TrimSpace(quotes.Down.AssetID)
	quotes.At = quotes.At.UTC()
	quotes.Up.Timestamp = quotes.Up.Timestamp.UTC()
	quotes.Down.Timestamp = quotes.Down.Timestamp.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.quotes[quotes.ConditionID]; ok && existing.At.After(quotes.At) {
		return nil
	}
	c.quotes[quotes.ConditionID] = quotes
	return nil
}

func (c *Cache) Get(conditionID string) (Snapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	quotes, ok := c.quotes[strings.TrimSpace(conditionID)]
	return quotes, ok
}

func Validate(quotes Snapshot) error {
	if strings.TrimSpace(quotes.ConditionID) == "" {
		return fmt.Errorf("market quote condition ID is required")
	}
	if quotes.At.IsZero() {
		return fmt.Errorf("market quote timestamp is required")
	}
	if err := validateSide("up", quotes.Up); err != nil {
		return err
	}
	return validateSide("down", quotes.Down)
}

func validateSide(name string, quote Quote) error {
	if strings.TrimSpace(quote.AssetID) == "" {
		return fmt.Errorf("%s quote asset ID is required", name)
	}
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
