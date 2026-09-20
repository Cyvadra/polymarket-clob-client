package postgres

import (
	"strings"
	"testing"
)

func TestMigrationsEmbedSchemaThenPositionRebuild(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	// The schema is a single merged migration; later files are data fixes and
	// indexes an already-migrated database cannot get from the schema file.
	want := []string{"000001_schema.sql", "000002_positions_from_fills.sql", "000003_fills_lane_index.sql"}
	if len(migrations) != len(want) {
		t.Fatalf("expected %d migrations, got %d", len(want), len(migrations))
	}
	for i, name := range want {
		if migrations[i].Name != name {
			t.Fatalf("migration %d is %q, want %q", i, migrations[i].Name, name)
		}
	}
	if !strings.Contains(migrations[1].SQL, "UPDATE positions") || !strings.Contains(migrations[1].SQL, "trade_status <> 'FAILED'") {
		t.Fatalf("position rebuild must recompute positions from non-failed fills:\n%s", migrations[1].SQL)
	}
	if !strings.Contains(migrations[2].SQL, "fills_lane_idx") {
		t.Fatalf("lane index migration must create fills_lane_idx:\n%s", migrations[2].SQL)
	}

	sql := migrations[0].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS order_intents",
		"CREATE TABLE IF NOT EXISTS orders",
		"signed_order_hash TEXT NOT NULL UNIQUE",
		"CREATE TABLE IF NOT EXISTS fills",
		"fill_id TEXT PRIMARY KEY",
		// Fill-settlement columns (formerly migration 000002).
		"fee_rate_bps NUMERIC(38, 18) NOT NULL DEFAULT 0",
		"trade_status TEXT NOT NULL DEFAULT 'CONFIRMED'",
		"trader_side TEXT NOT NULL DEFAULT ''",
		"CREATE TABLE IF NOT EXISTS positions",
		"PRIMARY KEY (condition_id, token_id, unique_tag)",
		"CREATE TABLE IF NOT EXISTS reservations",
		"reservations_one_active_sell_idx",
		"(condition_id, token_id, unique_tag)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
}
