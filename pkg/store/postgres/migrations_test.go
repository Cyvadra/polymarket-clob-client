package postgres

import (
	"strings"
	"testing"
)

func TestMigrationsEmbedSingleSchema(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("expected a single merged migration, got %d", len(migrations))
	}
	if migrations[0].Name != "000001_schema.sql" {
		t.Fatalf("unexpected migration name %q", migrations[0].Name)
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
