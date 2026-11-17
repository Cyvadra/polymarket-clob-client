package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rollbackSQL ends a warm-up batch: nothing it wrote is kept.
const rollbackSQL = `ROLLBACK`

// Warmup rehearses an open's writes on every connection the pool keeps idle,
// in a transaction it rolls back. A fresh backend loads its tables' catalog
// entries on its first real write, so without this the first order after a
// restart pays for them. It is also the startup check that those writes
// work: an error means an order would fail the same way. It returns how many
// connections it warmed.
func (s *Store) Warmup(ctx context.Context) (int, error) {
	conns := int(max(s.pool.Config().MinConns, 1))
	acquired := make([]*pgxpool.Conn, 0, conns)
	defer func() {
		for _, conn := range acquired {
			conn.Release()
		}
	}()
	// Hold every connection at once so each rehearsal lands on its own backend.
	for range conns {
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			return 0, fmt.Errorf("acquire connection to warm up: %w", err)
		}
		acquired = append(acquired, conn)
	}
	errs := make(chan error, len(acquired))
	for i, conn := range acquired {
		go func() {
			if err := rehearseOpen(ctx, conn.Conn()); err != nil {
				errs <- fmt.Errorf("warm up order writes on connection %d: %w", i, err)
				return
			}
			errs <- nil
		}()
	}
	var all []error
	for range acquired {
		all = append(all, <-errs)
	}
	return len(acquired), errors.Join(all...)
}

// rehearseOpen sends the batches an open sends, intent, reservation and
// submitting order, as one transaction that it rolls back.
func rehearseOpen(ctx context.Context, conn *pgx.Conn) error {
	id, err := warmupID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	intent := store.OrderIntentRecord{
		IntentID: id, UniqueTag: id, Strategy: "warmup", Kind: store.IntentOpen, MarketID: "warmup", ConditionID: "warmup", TokenID: "1",
		Outcome: "Up", Side: store.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: store.TimeInForceGTC, CreatedAt: now,
	}
	reservation := store.ReservationRecord{
		ReservationID: id, IntentID: id, ChildSequence: store.StrategyChildSequence, UniqueTag: id, MarketID: "warmup", ConditionID: "warmup", TokenID: "1",
		Outcome: "Up", Side: store.SideBuy, Shares: "2", Notional: "1", State: "active", Reason: "warmup", CreatedAt: now, UpdatedAt: now,
	}
	signed, err := signedOrderRow(store.SignedOrderRecord{
		IntentID: id, ChildSequence: store.StrategyChildSequence, SignedPayload: []byte(`{}`), SignedOrderHash: id, Salt: "1",
		RequestedShares: "2", Price: "0.5", OrderType: store.TimeInForceGTC, CreatedAt: now,
	})
	if err != nil {
		return err
	}
	submitting := signed
	submitting.State, submitting.Revision = statemachine.StateSubmitting, signed.Revision+1

	batch := &pgx.Batch{}
	batch.Queue(beginSQL)
	batch.Queue(asyncCommitSQL)
	batch.Queue(insertIntentSQL, intentArgs(intent)...)
	batch.Queue(insertReservationSQL, reservation.ReservationID, reservation.IntentID, reservation.ChildSequence, reservation.UniqueTag, reservation.MarketID,
		reservation.ConditionID, reservation.TokenID, reservation.Outcome, reservation.Side, reservation.Shares, reservation.Notional,
		reservation.State, reservation.Reason, reservation.CreatedAt, reservation.UpdatedAt)
	var exchangeOrderID *string
	queueSubmittingOrder(batch, signed, &submitting, submitting.State, "warmup", &exchangeOrderID)
	batch.Queue(rollbackSQL)
	err = conn.SendBatch(ctx, batch).Close()
	if err != nil && conn.PgConn().TxStatus() != 'I' {
		// A failed statement leaves the transaction open and aborted, and the
		// rollback queued after it was skipped.
		_, _ = conn.Exec(ctx, rollbackSQL)
	}
	return err
}

func warmupID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("warm-up ID: %w", err)
	}
	return "warmup-" + hex.EncodeToString(b[:]), nil
}
