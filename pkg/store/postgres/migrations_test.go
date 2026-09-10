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
	// The schema is a single merged migration; later files are data fixes.
	if len(migrations) != 2 {
		t.Fatalf("expected the merged schema plus the position rebuild, got %d", len(migrations))
	}
	if migrations[0].Name != "000001_schema.sql" || migrations[1].Name != "000002_positions_from_fills.sql" {
		t.Fatalf("unexpected migration names %q, %q", migrations[0].Name, migrations[1].Name)
	}
	if !strings.Contains(migrations[1].SQL, "UPDATE positions") || !strings.Contains(migrations[1].SQL, "trade_status <> 'FAILED'") {
		t.Fatalf("position rebuild must recompute positions from non-failed fills:\n%s", migrations[1].SQL)
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
