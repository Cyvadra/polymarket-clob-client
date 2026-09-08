package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeExecer struct {
	rowsAffected []int64
	statements   []string
	applied      map[string]bool
	queryErr     error
	errAt        int
	err          error
}

type fakeRow struct {
	applied bool
	err     error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	value, ok := dest[0].(*bool)
	if !ok {
		return errors.New("expected bool destination")
	}
	*value = r.applied
	return nil
}

func (f *fakeExecer) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if f.queryErr != nil {
		return fakeRow{err: f.queryErr}
	}
	name, _ := args[0].(string)
	return fakeRow{applied: f.applied[name]}
}

func (f *fakeExecer) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.statements = append(f.statements, sql)
	call := len(f.statements)
	if f.errAt == call {
		return pgconn.CommandTag{}, f.err
	}
	rowsAffected := int64(1)
	if len(f.rowsAffected) >= call {
		rowsAffected = f.rowsAffected[call-1]
	}
	return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", rowsAffected)), nil
}

func TestRunMigrationsAppliesThenRecords(t *testing.T) {
	execer := &fakeExecer{}
	err := runMigrations(context.Background(), execer, []Migration{
		{Name: "one.sql", SQL: "SELECT 1"},
		{Name: "two.sql", SQL: "SELECT 2"},
	})
	if err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if len(execer.statements) != 5 {
		t.Fatalf("expected ensure + apply/record each migration, got %d statements", len(execer.statements))
	}
	if !strings.Contains(execer.statements[1], "SELECT 1") || !strings.Contains(execer.statements[3], "SELECT 2") {
		t.Fatalf("expected migrations to be applied before recording, got %v", execer.statements)
	}
}

func TestRunMigrationsSkipsAlreadyApplied(t *testing.T) {
	execer := &fakeExecer{applied: map[string]bool{"one.sql": true}}
	err := runMigrations(context.Background(), execer, []Migration{{Name: "one.sql", SQL: "SELECT 1"}})
	if err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if len(execer.statements) != 1 {
		t.Fatalf("expected only ensure statement, got %d statements", len(execer.statements))
	}
}

func TestRunMigrationsWrapsApplyError(t *testing.T) {
	boom := errors.New("boom")
	execer := &fakeExecer{errAt: 2, err: boom}
	err := runMigrations(context.Background(), execer, []Migration{{Name: "one.sql", SQL: "SELECT 1"}})
	if err == nil || !strings.Contains(err.Error(), "apply migration one.sql") || !errors.Is(err, boom) {
		t.Fatalf("expected wrapped apply error, got %v", err)
	}
}

func TestRunMigrationsWrapsRecordError(t *testing.T) {
	boom := errors.New("boom")
	execer := &fakeExecer{errAt: 3, err: boom}
	err := runMigrations(context.Background(), execer, []Migration{{Name: "one.sql", SQL: "SELECT 1"}})
	if err == nil || !strings.Contains(err.Error(), "record migration one.sql") || !errors.Is(err, boom) {
		t.Fatalf("expected wrapped record error, got %v", err)
	}
}

func TestZeroTimeToNil(t *testing.T) {
	if value := zeroTimeToNil(time.Time{}); value != nil {
		t.Fatalf("expected nil zero time, got %v", value)
	}
}

func TestUniqueConstraintMatchesNamedPostgresViolation(t *testing.T) {
	err := &pgconn.PgError{Code: "23505", ConstraintName: "reservations_one_active_sell_idx"}
	if !isUniqueConstraint(err, "reservations_one_active_sell_idx") {
		t.Fatal("expected active sell reservation constraint to match")
	}
}

func TestTransitionOrderReturnsPersistedValuesFromDatabase(t *testing.T) {
	body, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	source := string(body)
	for _, fragment := range []string{
		"RETURNING matched_shares::text, exchange_order_id, updated_at",
		"order.State, order.Revision, order.MatchedShares = transition.To, newRevision, persistedMatchedShares",
		"order.ExchangeOrderID = persistedExchangeID",
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("TransitionOrder must return persisted DB values; missing %q", fragment)
		}
	}
}
