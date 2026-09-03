// Package lifecycle contains optional, strategy-neutral inventory accounting.
package lifecycle

import (
	"fmt"
	"math"
	"sync"
	"time"
)

type State uint8

const (
	StateUnconfirmed State = iota
	StateConfirmed
	StateUnknown
)

type Position struct {
	TokenID   string
	Shares    float64
	Reserved  float64
	State     State
	UpdatedAt time.Time
}

type Inventory struct {
	mu        sync.RWMutex
	positions map[string]Position
	now       func() time.Time
}

func NewInventory(now func() time.Time) *Inventory {
	if now == nil {
		now = time.Now
	}
	return &Inventory{positions: make(map[string]Position), now: now}
}

func (i *Inventory) RecordFill(tokenID string, shares float64) error {
	if tokenID == "" || shares <= 0 || math.IsNaN(shares) || math.IsInf(shares, 0) {
		return fmt.Errorf("invalid fill")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	position := i.positions[tokenID]
	position.TokenID = tokenID
	position.Shares += shares
	position.State = StateUnconfirmed
	position.UpdatedAt = i.now()
	i.positions[tokenID] = position
	return nil
}

func (i *Inventory) Confirm(tokenID string, shares float64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	position := i.positions[tokenID]
	position.TokenID = tokenID
	position.Shares = max(shares, 0)
	if position.Reserved > position.Shares {
		position.Reserved = position.Shares
	}
	position.State = StateConfirmed
	position.UpdatedAt = i.now()
	i.positions[tokenID] = position
}

func (i *Inventory) ReserveSell(tokenID string, shares float64) bool {
	if shares <= 0 {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	position, ok := i.positions[tokenID]
	if !ok || position.State == StateUnknown || position.Shares-position.Reserved+1e-8 < shares {
		return false
	}
	position.Reserved += shares
	position.UpdatedAt = i.now()
	i.positions[tokenID] = position
	return true
}

func (i *Inventory) ConsumeSell(tokenID string, shares float64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	position := i.positions[tokenID]
	position.Shares = max(position.Shares-shares, 0)
	position.Reserved = max(position.Reserved-shares, 0)
	position.UpdatedAt = i.now()
	i.positions[tokenID] = position
}

func (i *Inventory) ReleaseSell(tokenID string, shares float64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	position := i.positions[tokenID]
	position.Reserved = max(position.Reserved-shares, 0)
	position.UpdatedAt = i.now()
	i.positions[tokenID] = position
}

func (i *Inventory) MarkUnknown(tokenID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	position := i.positions[tokenID]
	position.TokenID = tokenID
	position.State = StateUnknown
	position.UpdatedAt = i.now()
	i.positions[tokenID] = position
}

func (i *Inventory) Position(tokenID string) (Position, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	position, ok := i.positions[tokenID]
	return position, ok
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
