package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	URL               string
	MaxConns          int32
	MinConns          int32
	ConnectTimeout    time.Duration
	HealthCheckPeriod time.Duration
	// MaxOpenBuyNotionalUSD caps the total notional of active BUY reservations.
	// Empty disables the cap. It is checked inside the reservation transaction
	// so concurrent intents cannot race past it.
	MaxOpenBuyNotionalUSD string
}

type Store struct {
	pool                  *pgxpool.Pool
	maxOpenBuyNotionalUSD string
}

func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("postgres URL is required")
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolConfig.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolConfig.MinConns = cfg.MinConns
	}
	if cfg.ConnectTimeout > 0 {
		poolConfig.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}
	if cfg.HealthCheckPeriod > 0 {
		poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool, maxOpenBuyNotionalUSD: cfg.MaxOpenBuyNotionalUSD}, nil
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("postgres store is not initialized")
	}
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migrations: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('executiond-migrations', 0))`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if err := runMigrations(ctx, tx, migrations); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

// WithIntentLock serializes work for one intent ID. The lock is transaction
// scoped so it is always released with the transaction, even if the connection
// is returned to the pool after an error.
func (s *Store) WithIntentLock(ctx context.Context, intentID string, fn func(context.Context) error) error {
	if intentID == "" || fn == nil {
		return fmt.Errorf("intent ID and lock function are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin intent lock: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, intentID); err != nil {
		return fmt.Errorf("acquire intent lock: %w", err)
	}
	if err := fn(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("release intent lock: %w", err)
	}
	return nil
}

func (s *Store) InsertIntent(ctx context.Context, record store.OrderIntentRecord) (bool, error) {
	if record.IntentID == "" {
		return false, fmt.Errorf("intent ID is required")
	}
	policy := record.Policy
	if len(policy) == 0 {
		policy = []byte("{}")
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	status := record.Status
	if status == "" {
		status = statemachine.StateIntentReceived
	}

	commandTag, err := s.pool.Exec(ctx, `
		INSERT INTO order_intents (
			intent_id, unique_tag, strategy, kind, market_id, event_slug, condition_id,
			token_id, outcome, side, target_usd, limit_price,
			time_in_force, post_only, feature_seq, feature_completed_at, expires_at,
			status, policy, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, NULLIF($11, '')::numeric, $12,
			$13, $14, $15, $16, $17,
			$18, $19, $20, $21
		)
		ON CONFLICT (intent_id) DO NOTHING
	`, record.IntentID, record.UniqueTag, record.Strategy, record.Kind, record.MarketID, record.EventSlug, record.ConditionID,
		record.TokenID, record.Outcome, record.Side, record.TargetUSD, record.LimitPrice,
		record.TimeInForce, record.PostOnly, record.FeatureSeq, zeroTimeToNil(record.FeatureCompletedAt), zeroTimeToNil(record.ExpiresAt),
		status, policy, createdAt, updatedAt)
	if err != nil {
		return false, fmt.Errorf("insert intent: %w", err)
	}
	return commandTag.RowsAffected() == 1, nil
}

func (s *Store) Intent(ctx context.Context, intentID string) (store.OrderIntentRecord, error) {
	if intentID == "" {
		return store.OrderIntentRecord{}, fmt.Errorf("intent ID is required")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT intent_id, unique_tag, strategy, kind, market_id, event_slug, condition_id,
			token_id, outcome, side, target_usd::text, limit_price::text,
			time_in_force, post_only, feature_seq, feature_completed_at, expires_at,
			status, policy, created_at, updated_at
		FROM order_intents
		WHERE intent_id = $1
	`, intentID)
	return scanIntent(row)
}

func (s *Store) PersistSignedOrder(ctx context.Context, record store.SignedOrderRecord) error {
	if record.IntentID == "" || record.ChildSequence <= 0 || len(record.SignedPayload) == 0 || record.SignedOrderHash == "" || record.Salt == "" {
		return fmt.Errorf("intent ID, child sequence, signed payload, signed order hash, and salt are required")
	}
	if record.RequestedShares == "" || record.Price == "" {
		return fmt.Errorf("requested shares and price are required")
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	state := record.State
	if state == "" {
		state = statemachine.StateSigned
	}
	if state != statemachine.StateSigned {
		return fmt.Errorf("signed order must start in %s", statemachine.StateSigned)
	}
	revision := record.Revision
	if revision == 0 {
		revision = 1
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin signed order persistence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO orders (
			intent_id, child_sequence, signed_payload, signed_order_hash, salt,
			exchange_order_id, requested_shares, matched_shares, price, order_type,
			post_only, state, revision, created_at, updated_at
		) VALUES (
			$1, $2, $3::jsonb, $4, $5,
			NULLIF($6, ''), $7, COALESCE(NULLIF($8, ''), '0')::numeric, $9, $10,
			$11, $12, $13, $14, $15
		)
	`, record.IntentID, record.ChildSequence, record.SignedPayload, record.SignedOrderHash, record.Salt,
		record.ExchangeOrderID, record.RequestedShares, record.MatchedShares, record.Price, record.OrderType,
		record.PostOnly, state, revision, createdAt, updatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrDuplicate
		}
		return fmt.Errorf("persist signed order: %w", err)
	}
	if err := insertOrderEvent(ctx, tx, store.OrderEventRecord{
		EventID:       eventID(record.IntentID, record.ChildSequence, revision, statemachine.EventSigned),
		IntentID:      record.IntentID,
		ChildSequence: record.ChildSequence,
		FromState:     statemachine.StateIntentReceived,
		ToState:       state,
		Event:         statemachine.EventSigned,
		ReceivedAt:    updatedAt,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit signed order persistence: %w", err)
	}
	return nil
}

func (s *Store) TransitionOrder(ctx context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, exchangeOrderID, reason string) (store.SignedOrderRecord, error) {
	if order.IntentID == "" || order.ChildSequence <= 0 || order.Revision <= 0 {
		return store.SignedOrderRecord{}, fmt.Errorf("intent ID, child sequence, and revision are required")
	}
	if matchedShares == "" {
		matchedShares = order.MatchedShares
	}
	if matchedShares == "" {
		matchedShares = "0"
	}
	transition, changed, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	if !changed && matchedShares == order.MatchedShares && (exchangeOrderID == "" || exchangeOrderID == order.ExchangeOrderID) {
		return order, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.SignedOrderRecord{}, fmt.Errorf("begin order transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	newRevision := order.Revision + 1
	var persistedExchangeOrderID *string
	var persistedMatchedShares string
	var persistedUpdatedAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE orders
		SET state = $1, revision = $2, matched_shares = GREATEST(matched_shares, $3::numeric),
			exchange_order_id = COALESCE(NULLIF($4, ''), exchange_order_id), updated_at = now()
		WHERE intent_id = $5 AND child_sequence = $6 AND revision = $7 AND state = $8
		RETURNING matched_shares::text, exchange_order_id, updated_at
	`, transition.To, newRevision, matchedShares, exchangeOrderID, order.IntentID, order.ChildSequence, order.Revision, order.State).Scan(&persistedMatchedShares, &persistedExchangeOrderID, &persistedUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.SignedOrderRecord{}, store.ErrConflict
	}
	if err != nil {
		return store.SignedOrderRecord{}, fmt.Errorf("update order transition: %w", err)
	}
	persistedExchangeID := ""
	if persistedExchangeOrderID != nil {
		persistedExchangeID = *persistedExchangeOrderID
	}
	if statemachine.IsTerminal(transition.To) {
		if err := releaseReservationTx(ctx, tx, fmt.Sprintf("%s:%d", order.IntentID, order.ChildSequence), "order reached "+string(transition.To)); err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.SignedOrderRecord{}, err
		}
	}
	if err := updateIntentStatus(ctx, tx, order.IntentID, transition.To); err != nil {
		return store.SignedOrderRecord{}, err
	}
	if err := insertOrderEvent(ctx, tx, store.OrderEventRecord{
		EventID: eventID(order.IntentID, order.ChildSequence, newRevision, event), IntentID: order.IntentID,
		ChildSequence: order.ChildSequence, ExchangeOrderID: persistedExchangeID, FromState: order.State,
		ToState: transition.To, Event: event, Reason: reason, ReceivedAt: time.Now().UTC(),
	}); err != nil {
		return store.SignedOrderRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.SignedOrderRecord{}, fmt.Errorf("commit order transition: %w", err)
	}
	order.State, order.Revision, order.MatchedShares = transition.To, newRevision, persistedMatchedShares
	order.ExchangeOrderID = persistedExchangeID
	order.UpdatedAt = persistedUpdatedAt
	return order, nil
}

func (s *Store) OpenOrders(ctx context.Context) ([]store.SignedOrderRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT intent_id, child_sequence, signed_payload, signed_order_hash, salt, exchange_order_id,
			requested_shares::text, matched_shares::text, price::text, order_type, post_only,
			state, revision, created_at, updated_at
		FROM orders
		WHERE state NOT IN ('CANCELED', 'FILLED', 'REJECTED', 'EXPIRED', 'FAILED')
		ORDER BY created_at, child_sequence
	`)
	if err != nil {
		return nil, fmt.Errorf("query open orders: %w", err)
	}
	defer rows.Close()
	var orders []store.SignedOrderRecord
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan open orders: %w", err)
	}
	return orders, nil
}

func (s *Store) OrderByExchangeID(ctx context.Context, exchangeOrderID string) (store.SignedOrderRecord, error) {
	if exchangeOrderID == "" {
		return store.SignedOrderRecord{}, fmt.Errorf("exchange order ID is required")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT intent_id, child_sequence, signed_payload, signed_order_hash, salt, exchange_order_id,
			requested_shares::text, matched_shares::text, price::text, order_type, post_only,
			state, revision, created_at, updated_at
		FROM orders
		WHERE exchange_order_id = $1
	`, exchangeOrderID)
	order, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return order, err
}

func (s *Store) OrderByIntent(ctx context.Context, intentID string, childSequence int) (store.SignedOrderRecord, error) {
	if intentID == "" || childSequence <= 0 {
		return store.SignedOrderRecord{}, fmt.Errorf("intent ID and child sequence are required")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT intent_id, child_sequence, signed_payload, signed_order_hash, salt, exchange_order_id,
			requested_shares::text, matched_shares::text, price::text, order_type, post_only,
			state, revision, created_at, updated_at
		FROM orders
		WHERE intent_id = $1 AND child_sequence = $2
	`, intentID, childSequence)
	order, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return order, err
}

// FilledShares sums the shares of every fill recorded against one exchange
// order, leaving out fills whose trade failed on chain.
func (s *Store) FilledShares(ctx context.Context, exchangeOrderID string) (string, error) {
	if exchangeOrderID == "" {
		return "", fmt.Errorf("exchange order ID is required")
	}
	var shares string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(shares), 0)::text
		FROM fills
		WHERE exchange_order_id = $1 AND trade_status <> 'FAILED'
	`, exchangeOrderID).Scan(&shares); err != nil {
		return "", fmt.Errorf("sum order fills: %w", err)
	}
	return shares, nil
}

// OrderAveragePrice returns the share-weighted average price of the fills
// recorded against one exchange order, leaving out fills whose trade failed on
// chain. It returns an empty string when the order has no fill yet.
func (s *Store) OrderAveragePrice(ctx context.Context, exchangeOrderID string) (string, error) {
	if exchangeOrderID == "" {
		return "", fmt.Errorf("exchange order ID is required")
	}
	var price string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((SUM(shares * price) / NULLIF(SUM(shares), 0))::text, '')
		FROM fills
		WHERE exchange_order_id = $1 AND trade_status <> 'FAILED'
	`, exchangeOrderID).Scan(&price); err != nil {
		return "", fmt.Errorf("average order fill price: %w", err)
	}
	return price, nil
}

func (s *Store) ApplyFill(ctx context.Context, record store.FillRecord) (bool, error) {
	if record.FillID == "" || record.ConditionID == "" || record.TokenID == "" || record.Outcome == "" {
		return false, fmt.Errorf("fill ID, condition ID, token ID, and outcome are required")
	}
	if record.Side != "BUY" && record.Side != "SELL" {
		return false, fmt.Errorf("invalid fill side %q", record.Side)
	}
	if record.Shares == "" || record.Price == "" {
		return false, fmt.Errorf("fill shares and price are required")
	}
	receivedAt := record.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin apply fill: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tradeStatus := record.TradeStatus
	if tradeStatus == "" {
		tradeStatus = "CONFIRMED"
	}
	var priorStatus string
	var priorSide string
	var priorShares, priorPrice string
	err = tx.QueryRow(ctx, `
		SELECT trade_status, side, shares::text, price::text
		FROM fills WHERE fill_id = $1 FOR UPDATE
	`, record.FillID).Scan(&priorStatus, &priorSide, &priorShares, &priorPrice)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock existing fill: %w", err)
	}
	if err == nil {
		if priorStatus == "FAILED" {
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("commit terminal failed fill: %w", err)
			}
			return false, nil
		}
		if tradeStatus != "FAILED" {
			if _, err := tx.Exec(ctx, `UPDATE fills SET trade_status = $2 WHERE fill_id = $1`, record.FillID, tradeStatus); err != nil {
				return false, fmt.Errorf("update fill settlement: %w", err)
			}
			if err := setPositionSettlementState(ctx, tx, record.ConditionID, record.TokenID, record.UniqueTag, tradeStatus); err != nil {
				return false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("commit fill settlement: %w", err)
			}
			return false, nil
		}
		if err := applyPositionDelta(ctx, tx, record.ConditionID, record.TokenID, record.UniqueTag, record.MarketID, record.Outcome, priorSide, priorShares, priorPrice, true, receivedAt); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE fills SET trade_status = 'FAILED' WHERE fill_id = $1`, record.FillID); err != nil {
			return false, fmt.Errorf("mark failed fill: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit failed fill rollback: %w", err)
		}
		return true, nil
	}
	commandTag, err := tx.Exec(ctx, `
		INSERT INTO fills (
			fill_id, exchange_order_id, intent_id, unique_tag, market_id, condition_id, token_id,
			outcome, side, shares, price, fee, fee_rate_bps, trade_status, trader_side, exchange_time, received_at
		) VALUES (
			$1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7,
			$8, $9, $10, $11, COALESCE(NULLIF($12, ''), '0')::numeric, COALESCE(NULLIF($13, ''), '0')::numeric, $14, $15, $16, $17
		)
		ON CONFLICT (fill_id) DO NOTHING
	`, record.FillID, record.ExchangeOrderID, record.IntentID, record.UniqueTag, record.MarketID, record.ConditionID, record.TokenID,
		record.Outcome, record.Side, record.Shares, record.Price, record.Fee, record.FeeRateBps, tradeStatus, record.TraderSide, zeroTimeToNil(record.ExchangeTime), receivedAt)
	if err != nil {
		return false, fmt.Errorf("insert fill: %w", err)
	}
	if commandTag.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit duplicate fill: %w", err)
		}
		return false, nil
	}
	if tradeStatus == "FAILED" {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit failed fill tombstone: %w", err)
		}
		return true, nil
	}
	if err := applyPositionDelta(ctx, tx, record.ConditionID, record.TokenID, record.UniqueTag, record.MarketID, record.Outcome, string(record.Side), record.Shares, record.Price, false, receivedAt); err != nil {
		return false, err
	}
	if err := setPositionSettlementState(ctx, tx, record.ConditionID, record.TokenID, record.UniqueTag, tradeStatus); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit fill: %w", err)
	}
	return true, nil
}

func oppositeSide(side string) string {
	if side == "BUY" {
		return "SELL"
	}
	return "BUY"
}

// updateIntentStatus mirrors the child order lifecycle onto the parent intent
// so order_intents.status reflects the latest observed state instead of the
// write-once INTENT_RECEIVED sentinel. Superseded close intents keep their
// marker even if the old order later reaches a terminal state.
func updateIntentStatus(ctx context.Context, tx pgx.Tx, intentID string, state statemachine.State) error {
	if _, err := tx.Exec(ctx, `
		UPDATE order_intents
		SET status = CASE WHEN status = $1 THEN status ELSE $2 END, updated_at = now()
		WHERE intent_id = $3
	`, store.IntentStatusSuperseded, string(state), intentID); err != nil {
		return fmt.Errorf("update intent status: %w", err)
	}
	return nil
}

func (s *Store) UpdateIntentStatus(ctx context.Context, intentID string, status string) error {
	if intentID == "" || status == "" {
		return fmt.Errorf("intent ID and status are required")
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE order_intents
		SET status = $1, updated_at = now()
		WHERE intent_id = $2
	`, status, intentID); err != nil {
		return fmt.Errorf("update intent status: %w", err)
	}
	return nil
}

func setPositionSettlementState(ctx context.Context, tx pgx.Tx, conditionID, tokenID, uniqueTag, tradeStatus string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE positions SET state = CASE WHEN $1 = 'CONFIRMED' THEN CASE WHEN position_size = 0 THEN 'empty' ELSE 'open' END ELSE 'unsettled' END
		WHERE condition_id = $2 AND token_id = $3 AND unique_tag = $4
	`, tradeStatus, conditionID, tokenID, uniqueTag); err != nil {
		return fmt.Errorf("set position settlement state: %w", err)
	}
	return nil
}

// applyPositionDelta moves a lane's position by one fill. Fills are the only
// source of position quantities, and each is applied at exactly the share
// count the exchange reported: no fee is estimated or deducted. What an open
// request asked for (target USD, limit price, planned shares) records intent
// only and never sizes a position.
func applyPositionDelta(ctx context.Context, tx pgx.Tx, conditionID, tokenID, uniqueTag, marketID, outcome, side, shares, price string, reverse bool, receivedAt time.Time) error {
	// Reversing a fill means applying the opposite side: a reversed BUY
	// reduces the position, a reversed SELL adds it back.
	if reverse {
		side = oppositeSide(side)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO positions (
			condition_id, token_id, unique_tag, market_id, outcome, position_size, actual_shares, available_size,
			reserved_size, entry_price, entry_time, state, source_revision, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			CASE WHEN $6 = 'BUY' THEN $7::numeric ELSE 0 END,
			CASE WHEN $6 = 'BUY' THEN $7::numeric ELSE 0 END,
			CASE WHEN $6 = 'BUY' THEN $7::numeric ELSE 0 END,
			0,
			CASE WHEN $6 = 'BUY' THEN $8::numeric ELSE NULL END,
			CASE WHEN $6 = 'BUY' THEN $9::timestamptz ELSE NULL END,
			CASE WHEN $6 = 'BUY' THEN 'open' ELSE 'empty' END,
			1, $9::timestamptz
		)
		ON CONFLICT (condition_id, token_id, unique_tag) DO UPDATE SET
			market_id = EXCLUDED.market_id,
			outcome = EXCLUDED.outcome,
			position_size = CASE
				WHEN $6 = 'BUY' THEN positions.position_size + $7::numeric
				ELSE GREATEST(positions.position_size - $7::numeric, 0)
			END,
			actual_shares = CASE
				WHEN $6 = 'BUY' THEN positions.actual_shares + $7::numeric
				ELSE GREATEST(positions.actual_shares - $7::numeric, 0)
			END,
			available_size = CASE
				WHEN $6 = 'BUY' THEN positions.available_size + $7::numeric
				ELSE GREATEST(positions.available_size - GREATEST($7::numeric - positions.reserved_size, 0), 0)
			END,
			reserved_size = CASE
				WHEN $6 = 'BUY' THEN positions.reserved_size
				ELSE GREATEST(positions.reserved_size - $7::numeric, 0)
			END,
			entry_price = CASE
				WHEN $6 = 'BUY' AND positions.position_size > 0 THEN
					((positions.entry_price * positions.position_size) + ($8::numeric * $7::numeric)) /
					(positions.position_size + $7::numeric)
				WHEN $6 = 'BUY' THEN $8::numeric
				WHEN positions.position_size <= $7::numeric THEN NULL
				ELSE positions.entry_price
			END,
			entry_time = CASE
				WHEN $6 = 'BUY' AND positions.position_size = 0 THEN $9::timestamptz
				WHEN $6 = 'SELL' AND positions.position_size <= $7::numeric THEN NULL
				ELSE positions.entry_time
			END,
			state = CASE
				WHEN $6 = 'BUY' THEN 'open'
				WHEN positions.position_size <= $7::numeric THEN 'empty'
				ELSE 'open'
			END,
			source_revision = positions.source_revision + 1,
			updated_at = $9::timestamptz
	`, conditionID, tokenID, uniqueTag, marketID, outcome, side,
		shares, price, receivedAt); err != nil {
		return fmt.Errorf("update position from fill: %w", err)
	}
	return nil
}

func (s *Store) Reserve(ctx context.Context, record store.ReservationRecord) error {
	if record.ReservationID == "" || record.IntentID == "" || record.ConditionID == "" || record.TokenID == "" || record.Outcome == "" {
		return fmt.Errorf("reservation ID, intent ID, condition ID, token ID, and outcome are required")
	}
	if record.Side != "BUY" && record.Side != "SELL" {
		return fmt.Errorf("invalid reservation side %q", record.Side)
	}
	if record.Shares == "" || record.Notional == "" {
		return fmt.Errorf("reservation shares and notional are required")
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	state := record.State
	if state == "" {
		state = "active"
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if record.Side == "SELL" {
		commandTag, err := tx.Exec(ctx, `
			UPDATE positions
			SET available_size = available_size - $1::numeric,
				reserved_size = reserved_size + $1::numeric,
				source_revision = source_revision + 1,
				updated_at = $2
			WHERE condition_id = $3 AND token_id = $4 AND unique_tag = $5 AND available_size >= $1::numeric
		`, record.Shares, updatedAt, record.ConditionID, record.TokenID, record.UniqueTag)
		if err != nil {
			return fmt.Errorf("reserve sell position: %w", err)
		}
		if commandTag.RowsAffected() != 1 {
			return store.ErrConflict
		}
	} else if err := s.checkOpenBuyExposure(ctx, tx, record.Notional); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO reservations (
			reservation_id, intent_id, child_sequence, unique_tag, market_id, condition_id, token_id,
			outcome, side, shares, notional, state, reason, created_at, updated_at
		) VALUES (
			$1, $2, NULLIF($3, 0), $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13, $14, $15
		)
	`, record.ReservationID, record.IntentID, record.ChildSequence, record.UniqueTag, record.MarketID, record.ConditionID, record.TokenID,
		record.Outcome, record.Side, record.Shares, record.Notional, state, record.Reason, createdAt, updatedAt)
	if err != nil {
		if isUniqueConstraint(err, "reservations_one_active_sell_idx") {
			return store.ErrActiveSellReservation
		}
		if isUniqueViolation(err) {
			return store.ErrDuplicate
		}
		return fmt.Errorf("insert reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reservation: %w", err)
	}
	return nil
}

func (s *Store) Reservation(ctx context.Context, reservationID string) (store.ReservationRecord, error) {
	if reservationID == "" {
		return store.ReservationRecord{}, fmt.Errorf("reservation ID is required")
	}
	var record store.ReservationRecord
	var childSequence *int
	err := s.pool.QueryRow(ctx, `
		SELECT reservation_id, intent_id, child_sequence, market_id, condition_id, token_id,
			unique_tag, outcome, side, shares::text, notional::text, state, reason, created_at, updated_at
		FROM reservations
		WHERE reservation_id = $1
	`, reservationID).Scan(&record.ReservationID, &record.IntentID, &childSequence, &record.MarketID,
		&record.ConditionID, &record.TokenID, &record.UniqueTag, &record.Outcome, &record.Side, &record.Shares,
		&record.Notional, &record.State, &record.Reason, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ReservationRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.ReservationRecord{}, fmt.Errorf("query reservation: %w", err)
	}
	if childSequence != nil {
		record.ChildSequence = *childSequence
	}
	return record, nil
}

// checkOpenBuyExposure rejects a BUY reservation that would push the total
// active BUY notional past the configured cap.
func (s *Store) checkOpenBuyExposure(ctx context.Context, tx pgx.Tx, notional string) error {
	if s.maxOpenBuyNotionalUSD == "" {
		return nil
	}
	var withinLimit bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(notional), 0) + $1::numeric <= $2::numeric
		FROM reservations WHERE state = 'active' AND side = 'BUY'
	`, notional, s.maxOpenBuyNotionalUSD).Scan(&withinLimit); err != nil {
		return fmt.Errorf("check open buy exposure: %w", err)
	}
	if !withinLimit {
		return store.ErrExposureLimit
	}
	return nil
}

func (s *Store) Release(ctx context.Context, reservationID, reason string) error {
	if reservationID == "" {
		return fmt.Errorf("reservation ID is required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin release reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	record, err := deleteActiveReservation(ctx, tx, reservationID)
	if err != nil {
		return err
	}
	if err := restoreReservationPosition(ctx, tx, record); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reservation release: %w", err)
	}
	return nil
}

func releaseReservationTx(ctx context.Context, tx pgx.Tx, reservationID, reason string) error {
	record, err := deleteActiveReservation(ctx, tx, reservationID)
	if err != nil {
		return err
	}
	return restoreReservationPosition(ctx, tx, record)
}

// deleteActiveReservation atomically removes the still-active reservation row
// and returns it, so a subsequent position restore uses the exact persisted
// values. It is shared by the public Release and the in-transaction release
// used when an order reaches a terminal state.
func deleteActiveReservation(ctx context.Context, tx pgx.Tx, reservationID string) (store.ReservationRecord, error) {
	var record store.ReservationRecord
	var childSequence *int
	err := tx.QueryRow(ctx, `
		DELETE FROM reservations
		WHERE reservation_id = $1 AND state = 'active'
		RETURNING intent_id, child_sequence, market_id, condition_id, token_id, unique_tag, outcome,
			side, shares::text, notional::text, state, reason, created_at, updated_at
	`, reservationID).Scan(&record.IntentID, &childSequence, &record.MarketID, &record.ConditionID,
		&record.TokenID, &record.UniqueTag, &record.Outcome, &record.Side, &record.Shares, &record.Notional, &record.State,
		&record.Reason, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ReservationRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.ReservationRecord{}, fmt.Errorf("release reservation: %w", err)
	}
	if childSequence != nil {
		record.ChildSequence = *childSequence
	}
	return record, nil
}

func restoreReservationPosition(ctx context.Context, tx pgx.Tx, record store.ReservationRecord) error {
	if record.Side != "SELL" {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE positions
		SET available_size = available_size + LEAST(reserved_size, $1::numeric),
			reserved_size = GREATEST(reserved_size - $1::numeric, 0),
			source_revision = source_revision + 1,
			updated_at = now()
		WHERE condition_id = $2 AND token_id = $3 AND unique_tag = $4
	`, record.Shares, record.ConditionID, record.TokenID, record.UniqueTag); err != nil {
		return fmt.Errorf("restore sell position: %w", err)
	}
	return nil
}

func (s *Store) PositionFeatures(ctx context.Context) ([]store.PositionRecord, error) {
	rows, err := s.pool.Query(ctx, positionSelectSQL()+" ORDER BY condition_id, token_id, unique_tag")
	if err != nil {
		return nil, fmt.Errorf("query position features: %w", err)
	}
	defer rows.Close()

	var positions []store.PositionRecord
	for rows.Next() {
		position, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		positions = append(positions, position)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan position features: %w", err)
	}
	return positions, nil
}

type migrationExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func runMigrations(ctx context.Context, execer migrationExecer, migrations []Migration) error {
	if _, err := execer.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	for _, migration := range migrations {
		var applied bool
		if err := execer.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, migration.Name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", migration.Name, err)
		}
		if applied {
			continue
		}
		if _, err := execer.Exec(ctx, migration.SQL); err != nil {
			return fmt.Errorf("apply migration %s: %w", migration.Name, err)
		}
		if _, err := execer.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, migration.Name); err != nil {
			return fmt.Errorf("record migration %s: %w", migration.Name, err)
		}
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanIntent(row rowScanner) (store.OrderIntentRecord, error) {
	var record store.OrderIntentRecord
	var policy []byte
	var targetUSD *string
	// feature_completed_at and expires_at are nullable, and insertIntent writes
	// NULL for a zero time. They must be read back through pointers or every
	// intent stored without them fails to scan.
	var featureCompletedAt, expiresAt *time.Time
	err := row.Scan(
		&record.IntentID, &record.UniqueTag, &record.Strategy, &record.Kind, &record.MarketID, &record.EventSlug, &record.ConditionID,
		&record.TokenID, &record.Outcome, &record.Side, &targetUSD, &record.LimitPrice,
		&record.TimeInForce, &record.PostOnly, &record.FeatureSeq, &featureCompletedAt, &expiresAt,
		&record.Status, &policy, &record.CreatedAt, &record.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OrderIntentRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.OrderIntentRecord{}, fmt.Errorf("scan intent: %w", err)
	}
	if targetUSD != nil {
		record.TargetUSD = *targetUSD
	}
	if featureCompletedAt != nil {
		record.FeatureCompletedAt = *featureCompletedAt
	}
	if expiresAt != nil {
		record.ExpiresAt = *expiresAt
	}
	if len(policy) > 0 {
		record.Policy = policy
	}
	return record, nil
}

func scanPosition(row rowScanner) (store.PositionRecord, error) {
	var record store.PositionRecord
	var entryPrice *string
	var entryTime *time.Time
	err := row.Scan(
		&record.MarketID, &record.ConditionID, &record.TokenID, &record.UniqueTag, &record.Outcome,
		&record.PositionSize, &record.ActualShares, &record.AvailableSize, &record.ReservedSize,
		&entryPrice, &entryTime, &record.State, &record.SourceRevision, &record.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.PositionRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.PositionRecord{}, fmt.Errorf("scan position: %w", err)
	}
	if entryPrice != nil {
		record.EntryPrice = *entryPrice
	}
	if entryTime != nil {
		record.EntryTime = *entryTime
	}
	return record, nil
}

func positionSelectSQL() string {
	return `
		SELECT market_id, condition_id, token_id, unique_tag, outcome,
			position_size::text, actual_shares::text, available_size::text, reserved_size::text,
			entry_price::text, entry_time, state, source_revision, updated_at
		FROM positions
	`
}

func insertOrderEvent(ctx context.Context, execer migrationExecer, record store.OrderEventRecord) error {
	receivedAt := record.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	observation := record.Observation
	if len(observation) == 0 {
		observation = []byte(`{}`)
	}
	_, err := execer.Exec(ctx, `
		INSERT INTO order_events (
			event_id, intent_id, child_sequence, exchange_order_id, from_state, to_state,
			event, observation, reason, exchange_time, received_at
		) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8::jsonb, $9, $10, $11)
		ON CONFLICT (event_id) DO NOTHING
	`, record.EventID, record.IntentID, record.ChildSequence, record.ExchangeOrderID, record.FromState,
		record.ToState, record.Event, observation, record.Reason, zeroTimeToNil(record.ExchangeTime), receivedAt)
	if err != nil {
		return fmt.Errorf("insert order event: %w", err)
	}
	return nil
}

func scanOrder(row rowScanner) (store.SignedOrderRecord, error) {
	var record store.SignedOrderRecord
	var exchangeOrderID *string
	err := row.Scan(&record.IntentID, &record.ChildSequence, &record.SignedPayload, &record.SignedOrderHash,
		&record.Salt, &exchangeOrderID, &record.RequestedShares, &record.MatchedShares, &record.Price,
		&record.OrderType, &record.PostOnly, &record.State, &record.Revision, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.SignedOrderRecord{}, fmt.Errorf("scan order: %w", err)
	}
	if exchangeOrderID != nil {
		record.ExchangeOrderID = *exchangeOrderID
	}
	return record, nil
}

func scanReservation(row rowScanner) (store.ReservationRecord, error) {
	var record store.ReservationRecord
	var childSequence *int
	err := row.Scan(&record.ReservationID, &record.IntentID, &childSequence, &record.MarketID, &record.ConditionID,
		&record.TokenID, &record.UniqueTag, &record.Outcome, &record.Side, &record.Shares, &record.Notional, &record.State,
		&record.Reason, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return store.ReservationRecord{}, fmt.Errorf("scan reservation: %w", err)
	}
	if childSequence != nil {
		record.ChildSequence = *childSequence
	}
	return record, nil
}

func eventID(intentID string, childSequence int, revision int64, event statemachine.Event) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%s", intentID, childSequence, revision, event)))
	return fmt.Sprintf("%x", sum[:])
}

func isUniqueViolation(err error) bool {
	_, ok := uniqueViolation(err)
	return ok
}

func isUniqueConstraint(err error, constraint string) bool {
	pgError, ok := uniqueViolation(err)
	return ok && pgError.ConstraintName == constraint
}

func uniqueViolation(err error) (*pgconn.PgError, bool) {
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23505" {
		return nil, false
	}
	return pgError, true
}

func zeroTimeToNil(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

var _ store.Store = (*Store)(nil)
