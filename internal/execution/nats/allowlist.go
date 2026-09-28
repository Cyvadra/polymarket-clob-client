package nats

import (
	"fmt"
	"strings"
)

// Allowlist holds the strategies one executiond trades for. Several
// executiond instances share one NATS bus and each receives every command, so
// a command for a strategy outside the list belongs to another wallet.
type Allowlist struct {
	names   []string
	allowed map[string]struct{}
}

// NewAllowlist builds an allowlist from strategy names. Names are trimmed and
// matched exactly; an empty list is an error, because a wallet that accepts
// every strategy would repeat the trades of every other wallet on the bus.
func NewAllowlist(names []string) (*Allowlist, error) {
	list := &Allowlist{allowed: make(map[string]struct{}, len(names))}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, seen := list.allowed[name]; seen {
			continue
		}
		list.allowed[name] = struct{}{}
		list.names = append(list.names, name)
	}
	if len(list.names) == 0 {
		return nil, fmt.Errorf("at least one allowed strategy is required")
	}
	return list, nil
}

// ParseAllowlist builds an allowlist from a comma-separated list of names.
func ParseAllowlist(value string) (*Allowlist, error) {
	return NewAllowlist(strings.Split(value, ","))
}

func (a *Allowlist) Allows(strategy string) bool {
	if a == nil {
		return false
	}
	_, ok := a.allowed[strategy]
	return ok
}

// Names returns the allowed strategies in the order they were configured.
func (a *Allowlist) Names() []string {
	if a == nil {
		return []string{}
	}
	return append([]string{}, a.names...)
}
