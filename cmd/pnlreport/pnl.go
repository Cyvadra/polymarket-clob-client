package main

import (
	"math"
	"sort"
	"strings"
	"time"
)

// dust is the share balance below which a lane counts as flat.
const dust = 1e-6

// WalletKey names the series that aggregates every unique_tag.
const WalletKey = "wallet"

// Group is the strategy a unique_tag belongs to: the part before the first
// ':' (tags look like "Strategy:asset:conditionID").
func Group(tag string) string {
	group, _, _ := strings.Cut(tag, ":")
	return group
}

// Event is one fill or settlement payout, in time order.
type Event struct {
	At        time.Time
	Tag       string
	Condition string
	Token     string
	Side      string // BUY, SELL, or SETTLE
	Shares    float64
	Price     float64
	Fee       float64
	Payout    float64 // SETTLE only; zero for a lost outcome
}

// Point is the cumulative all-time realized PnL right after an event.
type Point struct {
	At  time.Time
	PnL float64
}

// Metrics summarise one series over the report window.
type Metrics struct {
	PnL         float64
	MaxDrawdown float64
	Fees        float64
	BuyVolume   float64
	SellVolume  float64
	Fills       int
	Closed      int
	Wins        int
	OpenCost    float64
	AllTime     float64
}

// Series is one curve: the wallet or one strategy group of unique_tags.
type Series struct {
	Key     string
	Base    float64 // cumulative PnL at the window start
	Points  []Point // events inside the window only
	Metrics Metrics
	active  bool
}

type lane struct {
	group    string
	shares   float64
	cost     float64
	realized float64 // since the lane last opened
}

// Compute replays the full history with average-cost accounting per
// (unique_tag, condition, token) lane and returns the wallet series first,
// then each strategy group active in [start, end] sorted by name.
func Compute(events []Event, start, end time.Time) []*Series {
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	series := map[string]*Series{WalletKey: {Key: WalletKey}}
	lanes := map[[3]string]*lane{}
	cumulative := map[string]float64{}

	for _, event := range events {
		if event.At.After(end) {
			break
		}
		inWindow := !event.At.Before(start)
		key := [3]string{event.Tag, event.Condition, event.Token}
		l := lanes[key]
		group := Group(event.Tag)
		if l == nil {
			l = &lane{group: group}
			lanes[key] = l
		}
		if series[group] == nil {
			series[group] = &Series{Key: group}
		}
		targets := []*Series{series[WalletKey], series[group]}

		wasOpen := l.shares > dust
		realized := 0.0
		switch event.Side {
		case "BUY":
			l.shares += event.Shares
			l.cost += event.Shares*event.Price + event.Fee
		case "SELL":
			realized = event.Shares*event.Price - event.Fee - l.release(event.Shares)
		case "SETTLE":
			realized = event.Payout - l.release(event.Shares)
		}
		l.realized += realized

		for _, s := range targets {
			cumulative[s.Key] += realized
			if !inWindow {
				continue
			}
			s.active = true
			m := &s.Metrics
			if event.Side != "SETTLE" {
				m.Fills++
				m.Fees += event.Fee
				if event.Side == "BUY" {
					m.BuyVolume += event.Shares * event.Price
				} else {
					m.SellVolume += event.Shares * event.Price
				}
			}
			if wasOpen && l.shares <= dust && event.Side != "BUY" {
				m.Closed++
				if l.realized > 0 {
					m.Wins++
				}
			}
			s.Points = append(s.Points, Point{At: event.At, PnL: cumulative[s.Key]})
		}
		if l.shares <= dust {
			l.shares, l.cost, l.realized = 0, 0, 0
		}
		if !inWindow {
			for _, s := range targets {
				s.Base = cumulative[s.Key]
			}
		}
	}

	for _, l := range lanes {
		series[WalletKey].Metrics.OpenCost += l.cost
		series[l.group].Metrics.OpenCost += l.cost
	}
	result := []*Series{series[WalletKey]}
	var tags []string
	for key, s := range series {
		s.Metrics.AllTime = cumulative[key]
		s.Metrics.PnL = s.Metrics.AllTime - s.Base
		s.Metrics.MaxDrawdown = maxDrawdown(s)
		if key != WalletKey && s.active {
			tags = append(tags, key)
		}
	}
	sort.Strings(tags)
	for _, tag := range tags {
		result = append(result, series[tag])
	}
	return result
}

// release removes shares from the lane and returns their average cost.
// Selling more than is held costs only what was held.
func (l *lane) release(shares float64) float64 {
	if l.shares <= dust {
		return 0
	}
	sold := math.Min(shares, l.shares)
	cost := l.cost * sold / l.shares
	l.shares -= sold
	l.cost -= cost
	return cost
}

// maxDrawdown is the largest peak-to-trough fall of the window curve, which
// starts at Base.
func maxDrawdown(s *Series) float64 {
	peak, worst := s.Base, 0.0
	for _, p := range s.Points {
		peak = math.Max(peak, p.PnL)
		worst = math.Max(worst, peak-p.PnL)
	}
	return worst
}

// Sample returns the window PnL (rebased to zero at start) at n evenly
// spaced times from start to end, holding the last value between events.
func (s *Series) Sample(start, end time.Time, n int) ([]time.Time, []float64) {
	times := make([]time.Time, n)
	values := make([]float64, n)
	step := end.Sub(start) / time.Duration(n-1)
	next, current := 0, s.Base
	for i := range times {
		t := start.Add(step * time.Duration(i))
		if i == n-1 {
			t = end
		}
		for next < len(s.Points) && !s.Points[next].At.After(t) {
			current = s.Points[next].PnL
			next++
		}
		times[i], values[i] = t, current-s.Base
	}
	return times, values
}
