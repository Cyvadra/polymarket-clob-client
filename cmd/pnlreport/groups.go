package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadGroups reads pmm strategies.yaml files and maps each strategy name to
// its exclusive_group. Strategies without one are left out and report as
// their own group.
func LoadGroups(paths []string) (map[string]string, error) {
	groups := map[string]string{}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var raw struct {
			Strategies []struct {
				Name           string `yaml:"name"`
				ExclusiveGroup string `yaml:"exclusive_group"`
			} `yaml:"strategies"`
		}
		if err := yaml.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, s := range raw.Strategies {
			if s.Name != "" && s.ExclusiveGroup != "" {
				groups[s.Name] = s.ExclusiveGroup
			}
		}
	}
	return groups, nil
}

// Grouped is one reporting group: its combined series and its members.
type Grouped struct {
	Series  *Series
	Members []*Series
}

// GroupSeries combines the strategy series (everything after the wallet) by
// exclusive_group, sorted by group key. A strategy with no group is its own
// group with a single member.
func GroupSeries(strategies []*Series, groups map[string]string) []Grouped {
	byKey := map[string][]*Series{}
	for _, s := range strategies {
		key := s.Key
		if g := groups[s.Key]; g != "" {
			key = g
		}
		byKey[key] = append(byKey[key], s)
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Grouped, 0, len(keys))
	for _, key := range keys {
		members := byKey[key]
		combined := members[0]
		if len(members) > 1 {
			combined = Combine(key, members)
		}
		out = append(out, Grouped{Series: combined, Members: members})
	}
	return out
}

// ParsePaths splits a comma-separated path list.
func ParsePaths(list string) []string {
	var paths []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}
