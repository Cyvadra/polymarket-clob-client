//go:build postgres

package postgres

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestWarmupRehearsesWritesAndKeepsNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	conns, err := s.Warmup(ctx)
	if err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if conns < 1 {
		t.Fatalf("expected at least one warmed connection, got %d", conns)
	}
	for _, query := range []string{
		`SELECT count(*) FROM order_intents WHERE intent_id LIKE 'warmup-%'`,
		`SELECT count(*) FROM reservations WHERE intent_id LIKE 'warmup-%'`,
		`SELECT count(*) FROM orders WHERE intent_id LIKE 'warmup-%'`,
		`SELECT count(*) FROM order_events WHERE intent_id LIKE 'warmup-%'`,
	} {
		var n int
		if err := s.pool.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if n != 0 {
			t.Fatalf("warm-up kept %d rows: %s", n, query)
		}
	}
	// The connections went back usable, outside any transaction.
	if _, err := s.Warmup(ctx); err != nil {
		t.Fatalf("second warmup: %v", err)
	}
}

func TestWarmupFailsWithoutSchema(t *testing.T) {
	url := os.Getenv("EXECUTION_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("EXECUTION_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	s, err := New(ctx, Config{URL: url, ConnectTimeout: 10 * time.Second, MinConns: 1})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()
	// A schema with no tables earlier in the search path stands in for an
	// unmigrated database.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE order_intents (intent_id text)`); err != nil {
		t.Fatalf("shadow table: %v", err)
	}
	if err := rehearseOpen(ctx, conn.Conn()); err == nil {
		t.Fatal("expected the rehearsal to fail against a broken schema")
	}
	if status := conn.Conn().PgConn().TxStatus(); status != 'I' {
		t.Fatalf("expected the connection left outside a transaction, got status %c", status)
	}
	conn.Release()
}
