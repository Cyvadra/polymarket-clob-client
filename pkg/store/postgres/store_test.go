package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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

// A bare COALESCE(NULLIF($n, ”), '0') is typed text, and PostgreSQL refuses to
// insert it into a NUMERIC column ("is of type numeric but expression is of
// type text"). Every such default must carry an explicit cast.
func TestNumericDefaultsAreCastInSQL(t *testing.T) {
	body, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	source := string(body)
	schema, err := os.ReadFile("migrations/000001_schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	for _, column := range []string{"matched_shares", "fee", "fee_rate_bps"} {
		if !strings.Contains(string(schema), column+" NUMERIC") {
			t.Fatalf("%s is no longer a NUMERIC column; revisit this test", column)
		}
	}
	for index, statement := range strings.Split(source, "COALESCE(NULLIF(")[1:] {
		expression := statement
		if end := strings.Index(expression, "\n"); end >= 0 {
			expression = expression[:end]
		}
		// Defaults that fall back to another column stay text-free and need no
		// cast; only the literal '0' defaults feed NUMERIC columns.
		if !strings.Contains(expression, "'0')") {
			continue
		}
		if !strings.Contains(expression, "'0')::numeric") {
			t.Fatalf("uncast numeric default in COALESCE occurrence %d: %s", index+1, expression)
		}
	}
}

// nullableRow mimics how pgx assigns a column to a scan destination: a NULL
// column can only be written into a pointer-to-pointer destination, which is
// exactly the constraint scanIntent has to satisfy for its nullable columns.
type nullableRow struct {
	values []any
}

func (r nullableRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan into %d destinations, row has %d columns", len(dest), len(r.values))
	}
	for index, value := range r.values {
		target := reflect.ValueOf(dest[index]).Elem()
		if value == nil {
			if target.Kind() != reflect.Ptr {
				return fmt.Errorf("can't scan into dest[%d]: cannot scan NULL into *%s", index, target.Type())
			}
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		source := reflect.ValueOf(value)
		if target.Kind() == reflect.Ptr && source.Kind() != reflect.Ptr {
			pointer := reflect.New(target.Type().Elem())
			pointer.Elem().Set(source.Convert(target.Type().Elem()))
			source = pointer
		}
		target.Set(source.Convert(target.Type()))
	}
	return nil
}

func TestScanIntentAcceptsNullableTimestamps(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	row := nullableRow{values: []any{
		"intent-1", "lane-a", "probe", "OPEN", "market", "slug", "condition",
		"token", "Up", "BUY", "1.20", "0.49",
		"GTC", false, int64(0), nil, nil,
		"INTENT_RECEIVED", []byte(`{}`), now, now,
	}}
	record, err := scanIntent(row)
	if err != nil {
		t.Fatalf("scan intent with NULL feature_completed_at and expires_at: %v", err)
	}
	if !record.FeatureCompletedAt.IsZero() || !record.ExpiresAt.IsZero() {
		t.Fatalf("NULL timestamps must read back as zero times: %+v", record)
	}
	if record.IntentID != "intent-1" || record.TargetUSD != "1.20" || record.UpdatedAt != now {
		t.Fatalf("unexpected record: %+v", record)
	}

	expires := now.Add(time.Hour)
	row.values[15] = now
	row.values[16] = expires
	record, err = scanIntent(row)
	if err != nil {
		t.Fatalf("scan intent with populated timestamps: %v", err)
	}
	if record.FeatureCompletedAt != now || record.ExpiresAt != expires {
		t.Fatalf("populated timestamps lost: %+v", record)
	}
}
