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
	if len(migrations) != 2 {
		t.Fatalf("expected two migrations, got %d", len(migrations))
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
}
