package nats

import (
	"fmt"
	"strings"
)

// Allowlist holds the strategy name prefixes one executiond trades for: an
// entry allows every strategy whose name starts with it, so an exact name
// allows itself. Several
// executiond instances share one NATS bus and each receives every command, so
// a command for a strategy outside the list belongs to another wallet.
type Allowlist struct {
	names    []string
	allowed  map[string]struct{}
	wildcard bool
}

// Wildcard is the allowlist entry that allows every strategy.
const Wildcard = "*"

// NewAllowlist builds an allowlist from strategy name prefixes. Entries are
// trimmed and matched case-sensitively as prefixes; an empty list is an error, so trading every strategy takes
// an explicit "*". A wildcard wallet repeats the trades of every other wallet
// on the bus, so "*" must stand alone. Nor may one entry be a prefix of
// another: the longer would do nothing, and a list written when names
// matched exactly would silently widen.
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
	if _, ok := list.allowed[Wildcard]; ok {
		if len(list.names) > 1 {
			return nil, fmt.Errorf("%q allows every strategy; list no others", Wildcard)
		}
		list.wildcard = true
	}
	for i, name := range list.names {
		for _, other := range list.names[:i] {
			if strings.HasPrefix(name, other) || strings.HasPrefix(other, name) {
				return nil, fmt.Errorf("allowed strategies %q and %q overlap: each entry is a name prefix, so the longer one adds nothing", other, name)
			}
		}
	}
	return list, nil
}

// ParseAllowlist builds an allowlist from a comma-separated list of names.
func ParseAllowlist(value string) (*Allowlist, error) {
	return NewAllowlist(strings.Split(value, ","))
}

// Allows reports whether strategy is traded here: some entry is a prefix of it. A blank name is never
// allowed, not even by a wildcard.
func (a *Allowlist) Allows(strategy string) bool {
	if a == nil || strategy == "" {
		return false
	}
	if a.wildcard {
		return true
	}
	for _, prefix := range a.names {
		if strings.HasPrefix(strategy, prefix) {
			return true
		}
	}
	return false
}

// Wildcard reports whether every strategy is allowed.
func (a *Allowlist) Wildcard() bool { return a != nil && a.wildcard }

// Names returns the allowed prefixes in the order they were configured,
// or just "*" for a wildcard.
func (a *Allowlist) Names() []string {
	if a == nil {
		return []string{}
	}
	return append([]string{}, a.names...)
}
