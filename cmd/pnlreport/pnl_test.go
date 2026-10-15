package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func at(hours float64) time.Time { return t0.Add(time.Duration(hours * float64(time.Hour))) }

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func byKey(series []*Series) map[string]*Series {
	out := map[string]*Series{}
	for _, s := range series {
		out[s.Key] = s
	}
	return out
}

func TestComputeAverageCostAndSettlement(t *testing.T) {
	events := []Event{
		{At: at(1), Tag: "a", Condition: "c1", Token: "t1", Side: "BUY", Shares: 10, Price: 0.40, Fee: 0.10},
		{At: at(2), Tag: "a", Condition: "c1", Token: "t1", Side: "BUY", Shares: 10, Price: 0.60},
		// avg cost 0.505; sell 5 @ 0.70 fee 0.05 -> 3.5 - 0.05 - 2.525
		{At: at(3), Tag: "a", Condition: "c1", Token: "t1", Side: "SELL", Shares: 5, Price: 0.70, Fee: 0.05},
		// remaining 15 settle to 15 USD -> 15 - 7.575
		{At: at(4), Tag: "a", Condition: "c1", Token: "t1", Side: "SETTLE", Shares: 15, Payout: 15},
		{At: at(5), Tag: "b", Condition: "c2", Token: "t2", Side: "BUY", Shares: 20, Price: 0.30},
		// lost settlement: -6
		{At: at(6), Tag: "b", Condition: "c2", Token: "t2", Side: "SETTLE", Shares: 20},
		{At: at(7), Tag: "b", Condition: "c3", Token: "t3", Side: "BUY", Shares: 4, Price: 0.50},
	}
	series := byKey(Compute(events, t0, at(10)))
	a, b, w := series["a"].Metrics, series["b"].Metrics, series[WalletKey].Metrics
	near(t, "a pnl", a.PnL, 0.925+7.425)
	near(t, "b pnl", b.PnL, -6)
	near(t, "wallet pnl", w.PnL, 0.925+7.425-6)
	near(t, "a fees", a.Fees, 0.15)
	near(t, "b open", b.OpenCost, 2)
	near(t, "wallet open", w.OpenCost, 2)
	near(t, "b volume", b.BuyVolume, 8)
	if a.Fills != 3 || a.Closed != 1 || a.Wins != 1 || b.Closed != 1 || b.Wins != 0 || w.Closed != 2 {
		t.Fatalf("counts a=%+v b=%+v w=%+v", a, b, w)
	}
	// wallet curve: +0.925, +8.35, then -6 -> 2.35; drawdown 6 from the 8.35 peak.
	near(t, "wallet dd", w.MaxDrawdown, 6)
}

func TestComputeWindowRebasesAndKeepsCostBasis(t *testing.T) {
	events := []Event{
		{At: at(1), Tag: "a", Condition: "c", Token: "t", Side: "BUY", Shares: 10, Price: 0.5},
		{At: at(2), Tag: "a", Condition: "c", Token: "t", Side: "SELL", Shares: 5, Price: 0.6},
		{At: at(3), Tag: "old", Condition: "x", Token: "y", Side: "BUY", Shares: 1, Price: 0.5},
		{At: at(20), Tag: "a", Condition: "c", Token: "t", Side: "SELL", Shares: 5, Price: 0.4},
		{At: at(30), Tag: "a", Condition: "c", Token: "t", Side: "BUY", Shares: 1, Price: 0.4}, // after end
	}
	series := Compute(events, at(10), at(25))
	if len(series) != 2 || series[1].Key != "a" {
		t.Fatalf("want wallet and a only, got %d series", len(series))
	}
	a := series[1]
	near(t, "base", a.Base, 0.5)
	near(t, "window pnl", a.Metrics.PnL, -0.5)
	near(t, "all-time", a.Metrics.AllTime, 0)
	near(t, "dd", a.Metrics.MaxDrawdown, 0.5)
	near(t, "wallet open", series[0].Metrics.OpenCost, 0.5)

	times, values := a.Sample(at(10), at(25), 4) // 10, 15, 20, 25
	if !times[3].Equal(at(25)) {
		t.Fatalf("last sample at %v", times[3])
	}
	want := []float64{0, 0, -0.5, -0.5}
	for i := range want {
		near(t, "sample", values[i], want[i])
	}
}

func TestComputeOversellCostsOnlyHeldShares(t *testing.T) {
	events := []Event{
		{At: at(1), Tag: "a", Condition: "c", Token: "t", Side: "BUY", Shares: 2, Price: 0.5},
		{At: at(2), Tag: "a", Condition: "c", Token: "t", Side: "SELL", Shares: 3, Price: 0.5},
	}
	a := Compute(events, t0, at(5))[1].Metrics
	near(t, "pnl", a.PnL, 0.5)
	near(t, "open", a.OpenCost, 0)
}

func TestReadEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executiond.env")
	content := "# comment\n\nEXECUTION_POSTGRES_URL=postgres://u:p@h/db?x=1\nexport PNL_TELEGRAM_TOKEN='123:abc'\nPNL_TELEGRAM_CHAT_ID=\"-100\"\nEMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if env["EXECUTION_POSTGRES_URL"] != "postgres://u:p@h/db?x=1" || env["PNL_TELEGRAM_TOKEN"] != "123:abc" ||
		env["PNL_TELEGRAM_CHAT_ID"] != "-100" || env["EMPTY"] != "" {
		t.Fatalf("env = %v", env)
	}
}

func TestChartAndTable(t *testing.T) {
	events := []Event{
		{At: at(1), Tag: "strat_1", Condition: "c", Token: "t", Side: "BUY", Shares: 10, Price: 0.5},
		{At: at(2), Tag: "strat_1", Condition: "c", Token: "t", Side: "SELL", Shares: 10, Price: 0.6},
	}
	series := Compute(events, t0, at(3))
	png, err := Chart(series[0], "test · wallet · 3h", t0, at(3))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatal("not a PNG")
	}
	tables := Table("executiond-b", series, 3, at(3))
	if len(tables) != 1 {
		t.Fatalf("want one message, got %d", len(tables))
	}
	table := tables[0]
	for _, want := range []string{"*PnL · executiond\\-b*", "```\n", "strat_1", "$1.00", "1/1"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table missing %q:\n%s", want, table)
		}
	}
}

func TestComputeGroupsTagsByStrategyPrefix(t *testing.T) {
	events := []Event{
		{At: at(1), Tag: "Up:btc:0x1", Condition: "0x1", Token: "t", Side: "BUY", Shares: 10, Price: 0.5},
		{At: at(2), Tag: "Up:btc:0x2", Condition: "0x2", Token: "t", Side: "BUY", Shares: 10, Price: 0.5},
		{At: at(3), Tag: "Up:btc:0x1", Condition: "0x1", Token: "t", Side: "SETTLE", Shares: 10, Payout: 10},
		{At: at(4), Tag: "plain", Condition: "0x3", Token: "t", Side: "BUY", Shares: 2, Price: 0.5},
	}
	series := Compute(events, t0, at(5))
	if len(series) != 3 || series[1].Key != "Up" || series[2].Key != "plain" {
		t.Fatalf("keys = %v", []string{series[1].Key, series[2].Key})
	}
	up := series[1].Metrics
	near(t, "up pnl", up.PnL, 5)
	near(t, "up open", up.OpenCost, 5)
	if up.Fills != 2 || up.Closed != 1 {
		t.Fatalf("up = %+v", up)
	}
}

func TestTableSplitsLongReports(t *testing.T) {
	var events []Event
	for i := 0; i < 80; i++ {
		tag := strings.Repeat("s", 40) + string(rune('A'+i%26)) + string(rune('a'+i/26))
		events = append(events, Event{At: at(1), Tag: tag, Condition: tag, Token: "t", Side: "BUY", Shares: 1, Price: 0.5})
	}
	tables := Table("x", Compute(events, t0, at(2)), 2, at(2))
	if len(tables) < 2 {
		t.Fatalf("want several messages, got %d", len(tables))
	}
	for _, table := range tables {
		if len(table) > 4096 || !strings.Contains(table, "strategy") || strings.Count(table, "```") != 2 {
			t.Fatalf("bad chunk (%d chars):\n%s", len(table), table)
		}
	}
}
