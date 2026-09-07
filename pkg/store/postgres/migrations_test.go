package postgres

import (
	"strings"
	"testing"
)

func TestMigrationsEmbedInitialExecutionState(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if len(migrations) != 4 {
		t.Fatalf("expected four migrations, got %d", len(migrations))
	}
	if migrations[0].Name != "000001_execution_state.sql" {
		t.Fatalf("unexpected migration name %q", migrations[0].Name)
	}

	sql := migrations[0].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS order_intents",
		"idempotency_key TEXT NOT NULL UNIQUE",
		"CREATE TABLE IF NOT EXISTS orders",
		"signed_order_hash TEXT NOT NULL UNIQUE",
		"CREATE TABLE IF NOT EXISTS fills",
		"fill_id TEXT PRIMARY KEY",
		"CREATE TABLE IF NOT EXISTS positions",
		"PRIMARY KEY (condition_id, token_id)",
		"CREATE TABLE IF NOT EXISTS reservations",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
	if migrations[1].Name != "000002_fill_settlement.sql" || !strings.Contains(migrations[1].SQL, "trade_status") {
		t.Fatalf("unexpected settlement migration: %+v", migrations[1])
	}
	if migrations[2].Name != "000003_active_sell_reservation.sql" || !strings.Contains(migrations[2].SQL, "reservations_one_active_sell_idx") {
		t.Fatalf("unexpected active sell reservation migration: %+v", migrations[2])
	}
	if migrations[3].Name != "000004_target_usd.sql" || !strings.Contains(migrations[3].SQL, "RENAME COLUMN") {
		t.Fatalf("unexpected target usd migration: %+v", migrations[3])
	}
}
