package postgres

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
	"github.com/jackc/pgx/v5"
)

// Split inventory. A split of n pairs books a BUY of n of each token on the
// inventory lane at 0.5, a merge a SELL of each at 0.5, so a pair costs and
// returns exactly 1 and trade cash matches the pUSD that moved. A
// SPLIT_PRESERVE fill sells one token off the inventory lane on the exchange;
// the matching share of the other token then moves to the strategy lane as a
// TRANSFER priced at 1 - sell price + fee per share, a SELL on the inventory
// lane and a BUY on the strategy lane that move no cash. The inventory lane
// so nets to zero, and the strategy lane holds what an ordinary BUY would.

const (
	inventorySuffix = protocol.InventoryTagSuffix
	ctfLegPrice     = "0.5"
)

var (
	inventoryTag   = protocol.InventoryTag
	isInventoryTag = protocol.IsInventoryTag
)

// lockInventory serializes every write that sizes from an inventory lane.
func lockInventory(ctx context.Context, tx pgx.Tx, invTag, conditionID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "inventory:"+invTag+":"+conditionID); err != nil {
		return fmt.Errorf("lock inventory: %w", err)
	}
	return nil
}

// remainingSQL is the pairs an inventory lane ($1 condition, $2 tag) still
// has free: the smaller side held less every share resting preserves have
// reserved, each of which will use one pair, and less every pair a merge in
// flight will burn when it lands.
const remainingSQL = `
	SELECT GREATEST(CASE WHEN count(*) = 2 THEN MIN(position_size) - SUM(reserved_size) ELSE 0 END
		- (SELECT COALESCE(SUM(c.shares), 0) FROM ctf_operations c
			WHERE c.condition_id = $1 AND c.unique_tag || '` + inventorySuffix + `' = $2
				AND c.action = 'MERGE' AND c.state IN ('PENDING', 'SUBMITTED', 'UNKNOWN')), 0)::text
	FROM positions WHERE condition_id = $1 AND unique_tag = $2
`

func inventoryLevel(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, uniqueTag, conditionID string) (store.InventoryLevel, error) {
	var level store.InventoryLevel
	if err := q.QueryRow(ctx, `
		SELECT (`+remainingSQL+`), COALESCE(SUM(reserved_size), 0)::text
		FROM positions WHERE condition_id = $1 AND unique_tag = $2
	`, conditionID, inventoryTag(uniqueTag)).Scan(&level.Remaining, &level.Reserved); err != nil {
		return level, fmt.Errorf("read remaining inventory: %w", err)
	}
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(shares) FILTER (WHERE action = 'SPLIT' AND state IN ('PENDING', 'SUBMITTED', 'UNKNOWN')), 0)::text,
			COALESCE(bool_or(action = 'MERGE'), false)
		FROM ctf_operations WHERE unique_tag = $1 AND condition_id = $2
	`, uniqueTag, conditionID).Scan(&level.InFlight, &level.Merged); err != nil {
		return level, fmt.Errorf("read inventory operations: %w", err)
	}
	return level, nil
}

func (s *Store) InventoryLevel(ctx context.Context, uniqueTag, conditionID string) (store.InventoryLevel, error) {
	return inventoryLevel(ctx, s.pool, uniqueTag, conditionID)
}

func (s *Store) BeginSplit(ctx context.Context, op store.CTFOperationRecord, maxShares string) (store.CTFOperationRecord, string, bool, error) {
	op.Action = store.CTFSplit
	if err := checkCTFOperation(op); err != nil {
		return op, "", false, err
	}
	if op.TargetShares == "" {
		return op, "", false, fmt.Errorf("a split needs target shares")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return op, "", false, fmt.Errorf("begin split: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockInventory(ctx, tx, inventoryTag(op.UniqueTag), op.ConditionID); err != nil {
		return op, "", false, err
	}
	level, err := inventoryLevel(ctx, tx, op.UniqueTag, op.ConditionID)
	if err != nil {
		return op, "", false, err
	}
	if level.Merged {
		return op, "", false, store.ErrInventoryClosed
	}
	// Whole 6-decimal units: the chain splits base units, so the gap is
	// truncated too and a split short of it means cash limited it.
	var gap, shares string
	if err := tx.QueryRow(ctx, `
		SELECT trunc(g, 6)::text, trunc(LEAST(g, COALESCE(NULLIF($4::text, '')::numeric, g)), 6)::text
		FROM (SELECT GREATEST($1::numeric - $2::numeric - $3::numeric, 0) AS g) gap
	`, op.TargetShares, level.Remaining, level.InFlight, maxShares).Scan(&gap, &shares); err != nil {
		return op, "", false, fmt.Errorf("size split: %w", err)
	}
	if !decimal.Positive(shares) {
		return op, gap, false, tx.Commit(ctx)
	}
	op.Shares = shares
	if err := insertCTFOperation(ctx, tx, &op); err != nil {
		return op, gap, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return op, gap, false, fmt.Errorf("commit split: %w", err)
	}
	return op, gap, true, nil
}

func (s *Store) BeginMerge(ctx context.Context, op store.CTFOperationRecord) (store.CTFOperationRecord, bool, error) {
	op.Action = store.CTFMerge
	if err := checkCTFOperation(op); err != nil {
		return op, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return op, false, fmt.Errorf("begin merge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockInventory(ctx, tx, inventoryTag(op.UniqueTag), op.ConditionID); err != nil {
		return op, false, err
	}
	level, err := inventoryLevel(ctx, tx, op.UniqueTag, op.ConditionID)
	if err != nil {
		return op, false, err
	}
	var shares string
	if err := tx.QueryRow(ctx, `
		SELECT trunc(LEAST($1::numeric, COALESCE(NULLIF($2::text, '')::numeric, $1::numeric)), 6)::text
	`, level.Remaining, op.Shares).Scan(&shares); err != nil {
		return op, false, fmt.Errorf("size merge: %w", err)
	}
	var closing bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ctf_operations WHERE op_id = $1 AND action = 'MERGE' AND state = 'PENDING')
	`, op.OpID).Scan(&closing); err != nil {
		return op, false, fmt.Errorf("read closing merge: %w", err)
	}
	if !decimal.Positive(shares) {
		// Nothing free to merge, but the lane is closed all the same: record
		// an empty merge so later splits are refused.
		op.Shares, op.State = "0", store.CTFConfirmed
		if !closing {
			if err := insertCTFOperation(ctx, tx, &op); err != nil {
				return op, false, err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ctf_operations SET shares = 0, state = 'CONFIRMED', confirmed_at = now(), updated_at = now() WHERE op_id = $1
		`, op.OpID); err != nil {
			return op, false, fmt.Errorf("close inventory lane: %w", err)
		}
		return op, false, tx.Commit(ctx)
	}
	op.Shares = shares
	if closing {
		if _, err := tx.Exec(ctx, `UPDATE ctf_operations SET shares = $2::numeric, updated_at = now() WHERE op_id = $1`, op.OpID, shares); err != nil {
			return op, false, fmt.Errorf("size merge: %w", err)
		}
		op.InventoryTag = inventoryTag(op.UniqueTag)
	} else if err := insertCTFOperation(ctx, tx, &op); err != nil {
		return op, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return op, false, fmt.Errorf("commit merge: %w", err)
	}
	return op, true, nil
}

// CloseInventory records a merge of no shares yet, so the lane counts as
// merged at once: splits are refused and preserves can no longer reserve its
// pairs while the merge cancels the resting ones. BeginMerge with the same
// op ID then sizes it.
func (s *Store) CloseInventory(ctx context.Context, op store.CTFOperationRecord) (store.CTFOperationRecord, error) {
	op.Action, op.Shares, op.State = store.CTFMerge, "0", store.CTFPending
	if err := checkCTFOperation(op); err != nil {
		return op, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return op, fmt.Errorf("begin close inventory: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockInventory(ctx, tx, inventoryTag(op.UniqueTag), op.ConditionID); err != nil {
		return op, err
	}
	if err := insertCTFOperation(ctx, tx, &op); err != nil {
		return op, err
	}
	if err := tx.Commit(ctx); err != nil {
		return op, fmt.Errorf("commit close inventory: %w", err)
	}
	return op, nil
}

func checkCTFOperation(op store.CTFOperationRecord) error {
	if op.OpID == "" || op.Strategy == "" || op.UniqueTag == "" || op.ConditionID == "" {
		return fmt.Errorf("operation ID, strategy, unique tag and condition ID are required")
	}
	if isInventoryTag(op.UniqueTag) {
		return fmt.Errorf("unique tag %q is an inventory lane; name the strategy lane", op.UniqueTag)
	}
	if len(op.Tokens) != 2 || len(op.Outcomes) != 2 {
		return fmt.Errorf("a split or merge needs both tokens and outcomes")
	}
	return nil
}

func insertCTFOperation(ctx context.Context, tx pgx.Tx, op *store.CTFOperationRecord) error {
	if op.State == "" {
		op.State = store.CTFPending
	}
	op.InventoryTag = inventoryTag(op.UniqueTag)
	if err := tx.QueryRow(ctx, `
		INSERT INTO ctf_operations (op_id, request_id, strategy, unique_tag, market_id, condition_id, action,
			target_shares, shares, tokens, outcomes, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::numeric, $9::numeric, $10, $11, $12)
		RETURNING created_at, updated_at
	`, op.OpID, op.RequestID, op.Strategy, op.UniqueTag, op.MarketID, op.ConditionID, string(op.Action),
		op.TargetShares, op.Shares, op.Tokens, op.Outcomes, string(op.State)).Scan(&op.CreatedAt, &op.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return store.ErrDuplicate
		}
		return fmt.Errorf("insert ctf operation: %w", err)
	}
	return nil
}

func (s *Store) ArmCTFOperation(ctx context.Context, opID string, baseline []string, armedAt, deadline time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE ctf_operations SET baseline = $2, deadline = $3, armed_at = $4, updated_at = now()
		WHERE op_id = $1 AND state = 'PENDING'
	`, opID, baseline, deadline, armedAt)
	if err != nil {
		return fmt.Errorf("arm ctf operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrConflict
	}
	return nil
}

func (s *Store) MarkCTFOperation(ctx context.Context, opID string, state store.CTFOpState, relayerTxID, txHash, errText string) error {
	if state == store.CTFConfirmed {
		return fmt.Errorf("confirm an operation with ConfirmCTFOperation, which books it")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE ctf_operations SET state = $2,
			relayer_tx_id = CASE WHEN $3 = '' THEN relayer_tx_id ELSE $3 END,
			tx_hash = CASE WHEN $4 = '' THEN tx_hash ELSE $4 END,
			error = $5, updated_at = now()
		WHERE op_id = $1 AND state <> 'CONFIRMED'
	`, opID, string(state), relayerTxID, txHash, errText)
	if err != nil {
		return fmt.Errorf("mark ctf operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrConflict
	}
	return nil
}

func (s *Store) ConfirmCTFOperation(ctx context.Context, opID, txHash string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin confirm ctf operation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	op, err := scanCTFOperation(tx.QueryRow(ctx, ctfOperationSelect+` WHERE op_id = $1 FOR UPDATE`, opID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	if op.State == store.CTFConfirmed {
		return tx.Commit(ctx)
	}
	if err := lockInventory(ctx, tx, op.InventoryTag, op.ConditionID); err != nil {
		return err
	}
	side := "BUY"
	if op.Action == store.CTFMerge {
		side = "SELL"
	}
	for i, token := range op.Tokens {
		fill := store.FillRecord{
			FillID: "ctf:" + op.OpID + ":" + token, UniqueTag: op.InventoryTag, MarketID: op.MarketID,
			ConditionID: op.ConditionID, TokenID: token, Outcome: op.Outcomes[i], Side: store.Side(side),
			Shares: op.Shares, Price: ctfLegPrice, TradeStatus: "CONFIRMED", ReceivedAt: at,
		}
		inserted, err := insertSourceFill(ctx, tx, fill, string(op.Action))
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if side == "BUY" {
			err = applyPositionDelta(ctx, tx, op.ConditionID, token, op.InventoryTag, op.MarketID, op.Outcomes[i], "BUY", op.Shares, ctfLegPrice, false, at)
		} else {
			err = applyUnreservedSell(ctx, tx, op.ConditionID, token, op.InventoryTag, op.Shares, at)
		}
		if err != nil {
			return err
		}
		if err := setPositionSettlementState(ctx, tx, op.ConditionID, token, op.InventoryTag, "CONFIRMED"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ctf_operations SET state = 'CONFIRMED', tx_hash = CASE WHEN $2 = '' THEN tx_hash ELSE $2 END,
			error = '', confirmed_at = $3, updated_at = now()
		WHERE op_id = $1
	`, op.OpID, txHash, at); err != nil {
		return fmt.Errorf("confirm ctf operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ctf operation: %w", err)
	}
	return nil
}

const ctfOperationSelect = `
	SELECT op_id, request_id, strategy, unique_tag, market_id, condition_id, action,
		COALESCE(target_shares::text, ''), shares::text, tokens, outcomes, state, relayer_tx_id, tx_hash, error,
		baseline, armed_at, deadline, created_at, updated_at
	FROM ctf_operations`

func scanCTFOperation(row pgx.Row) (store.CTFOperationRecord, error) {
	var op store.CTFOperationRecord
	var action, state string
	var armedAt, deadline *time.Time
	if err := row.Scan(&op.OpID, &op.RequestID, &op.Strategy, &op.UniqueTag, &op.MarketID, &op.ConditionID, &action,
		&op.TargetShares, &op.Shares, &op.Tokens, &op.Outcomes, &state, &op.RelayerTxID, &op.TxHash, &op.Error,
		&op.Baseline, &armedAt, &deadline, &op.CreatedAt, &op.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return op, err
		}
		return op, fmt.Errorf("scan ctf operation: %w", err)
	}
	op.Action, op.State, op.InventoryTag = store.CTFAction(action), store.CTFOpState(state), inventoryTag(op.UniqueTag)
	if deadline != nil {
		op.Deadline = *deadline
	}
	if armedAt != nil {
		op.ArmedAt = *armedAt
	}
	return op, nil
}

// ExchangeNetShares sums the wallet's exchange fills of each token matched at
// or after since, bought less sold, in base units. Fills booked from splits,
// merges and transfers are not trades and are left out.
func (s *Store) ExchangeNetShares(ctx context.Context, tokens []string, since time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.token_id, COALESCE(ROUND(SUM(CASE WHEN f.side = 'BUY' THEN f.shares ELSE -f.shares END) * 1000000), 0)::text
		FROM unnest($1::text[]) AS t(token_id)
		LEFT JOIN fills f ON f.token_id = t.token_id AND f.source = 'CLOB' AND f.trade_status <> 'FAILED'
			AND COALESCE(f.exchange_time, f.received_at) >= $2
		GROUP BY t.token_id
	`, tokens, since)
	if err != nil {
		return nil, fmt.Errorf("query exchange net shares: %w", err)
	}
	defer rows.Close()
	net := map[string]string{}
	for rows.Next() {
		var token, total string
		if err := rows.Scan(&token, &total); err != nil {
			return nil, fmt.Errorf("scan exchange net shares: %w", err)
		}
		net[token] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan exchange net shares: %w", err)
	}
	out := make([]string, len(tokens))
	for i, token := range tokens {
		out[i] = net[token]
	}
	return out, nil
}

func (s *Store) OpenCTFOperations(ctx context.Context) ([]store.CTFOperationRecord, error) {
	rows, err := s.pool.Query(ctx, ctfOperationSelect+` WHERE state IN ('PENDING', 'SUBMITTED', 'UNKNOWN') ORDER BY created_at, op_id`)
	if err != nil {
		return nil, fmt.Errorf("query open ctf operations: %w", err)
	}
	defer rows.Close()
	var ops []store.CTFOperationRecord
	for rows.Next() {
		op, err := scanCTFOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan open ctf operations: %w", err)
	}
	return ops, nil
}

// InFlightSplitCash is the pUSD that splits not yet resolved will spend, one
// dollar a pair. A split that already landed is counted until it resolves,
// which only leaves free cash short for that while.
func (s *Store) InFlightSplitCash(ctx context.Context) (string, error) {
	var total string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(shares), 0)::text FROM ctf_operations
		WHERE action = 'SPLIT' AND state IN ('PENDING', 'SUBMITTED', 'UNKNOWN')
	`).Scan(&total); err != nil {
		return "", fmt.Errorf("query in-flight split cash: %w", err)
	}
	return total, nil
}

func (s *Store) CTFOperationsSettling(ctx context.Context, since time.Time) (bool, error) {
	var settling bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ctf_operations
			WHERE state IN ('PENDING', 'SUBMITTED', 'UNKNOWN') OR (state = 'CONFIRMED' AND shares > 0 AND confirmed_at > $1))
	`, since).Scan(&settling); err != nil {
		return false, fmt.Errorf("query settling ctf operations: %w", err)
	}
	return settling, nil
}

// insertSourceFill inserts a fill that did not come from the exchange and
// reports whether it is new.
func insertSourceFill(ctx context.Context, tx pgx.Tx, fill store.FillRecord, source string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO fills (
			fill_id, intent_id, unique_tag, market_id, condition_id, token_id,
			outcome, side, shares, price, fee, trade_status, received_at, source
		) VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9::numeric, $10::numeric, 0, $11, $12, $13)
		ON CONFLICT (fill_id) DO NOTHING
	`, fill.FillID, fill.IntentID, fill.UniqueTag, fill.MarketID, fill.ConditionID, fill.TokenID,
		fill.Outcome, string(fill.Side), fill.Shares, fill.Price, fill.TradeStatus, fill.ReceivedAt, source)
	if err != nil {
		return false, fmt.Errorf("insert %s fill: %w", strings.ToLower(source), err)
	}
	return tag.RowsAffected() == 1, nil
}

// applyUnreservedSell takes shares off a lane's free size, leaving its
// reservations alone: a merge or a transfer out of inventory is not the
// resting sell those reservations back. applyPositionDelta's SELL, by
// contrast, is the fill of that resting sell and draws its reservation down.
// What it books already happened - on chain or on the exchange - so a lane
// short of free shares is taken to zero free and logged, never refused.
func applyUnreservedSell(ctx context.Context, tx pgx.Tx, conditionID, tokenID, uniqueTag, shares string, at time.Time) error {
	var short bool
	err := tx.QueryRow(ctx, `
		WITH lane AS (
			SELECT LEAST(available_size, $4::numeric) AS take, available_size < $4::numeric AS short
			FROM positions WHERE condition_id = $1 AND token_id = $2 AND unique_tag = $3 FOR UPDATE
		)
		UPDATE positions p SET
			position_size = p.position_size - lane.take,
			actual_shares = GREATEST(p.actual_shares - lane.take, 0),
			available_size = p.available_size - lane.take,
			entry_price = CASE WHEN p.position_size <= lane.take THEN NULL ELSE p.entry_price END,
			entry_time = CASE WHEN p.position_size <= lane.take THEN NULL ELSE p.entry_time END,
			state = CASE WHEN p.position_size <= lane.take THEN 'empty' ELSE p.state END,
			source_revision = p.source_revision + 1,
			updated_at = $5
		FROM lane
		WHERE p.condition_id = $1 AND p.token_id = $2 AND p.unique_tag = $3
		RETURNING lane.short
	`, conditionID, tokenID, uniqueTag, shares, at).Scan(&short)
	if errors.Is(err, pgx.ErrNoRows) {
		short = true
	} else if err != nil {
		return fmt.Errorf("take free inventory: %w", err)
	}
	if short {
		log.Printf("inventory lane %s token %s held fewer than %s free shares; took what it had", uniqueTag, tokenID, shares)
	}
	return nil
}

// preserveLeg reads the BUY a SPLIT_PRESERVE intent stands for. ok is false
// for any other intent.
func preserveLeg(ctx context.Context, tx pgx.Tx, intentID string) (tag, tokenID, outcome string, ok bool, err error) {
	if intentID == "" {
		return "", "", "", false, nil
	}
	var t, tok, out *string
	err = tx.QueryRow(ctx, `
		SELECT policy->'preserve'->>'unique_tag', policy->'preserve'->>'token_id', policy->'preserve'->>'outcome'
		FROM order_intents WHERE intent_id = $1
	`, intentID).Scan(&t, &tok, &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("read preserve leg: %w", err)
	}
	if t == nil || tok == nil || *t == "" || *tok == "" {
		return "", "", "", false, nil
	}
	if out != nil {
		outcome = *out
	}
	return *t, *tok, outcome, true, nil
}

func transferFillIDs(fillID string) (out, in string) {
	return "xfer:" + fillID + ":out", "xfer:" + fillID + ":in"
}

// applyPreserveTransfer moves the shares a preserve fill sold off one token
// of the inventory lane, taken from its other token, to the strategy lane.
func applyPreserveTransfer(ctx context.Context, tx pgx.Tx, record store.FillRecord, tradeStatus string, at time.Time) error {
	if !isInventoryTag(record.UniqueTag) || record.Side != "SELL" {
		return nil
	}
	tag, tokenID, outcome, ok, err := preserveLeg(ctx, tx, record.IntentID)
	if err != nil {
		return err
	}
	if !ok {
		// Only preserves sell off an inventory lane, so this sell has no
		// transfer to book and the lane is left one-sided.
		log.Printf("inventory lane %s fill %s (intent %q) is not a preserve leg; no transfer booked", record.UniqueTag, record.FillID, record.IntentID)
		return nil
	}
	var price string
	if err := tx.QueryRow(ctx, `
		SELECT LEAST(GREATEST(1 - $1::numeric + COALESCE(NULLIF($2, ''), '0')::numeric / $3::numeric, 0.000001), 0.999999)::text
	`, record.Price, record.Fee, record.Shares).Scan(&price); err != nil {
		return fmt.Errorf("price preserve transfer: %w", err)
	}
	outID, inID := transferFillIDs(record.FillID)
	out := store.FillRecord{FillID: outID, IntentID: record.IntentID, UniqueTag: record.UniqueTag, MarketID: record.MarketID,
		ConditionID: record.ConditionID, TokenID: tokenID, Outcome: outcome, Side: "SELL", Shares: record.Shares, Price: price,
		TradeStatus: tradeStatus, ReceivedAt: at}
	in := out
	in.FillID, in.UniqueTag, in.Side = inID, tag, "BUY"
	if inserted, err := insertSourceFill(ctx, tx, out, store.FillSourceTransfer); err != nil || !inserted {
		return err
	}
	if err := applyUnreservedSell(ctx, tx, record.ConditionID, tokenID, record.UniqueTag, record.Shares, at); err != nil {
		return err
	}
	if _, err := insertSourceFill(ctx, tx, in, store.FillSourceTransfer); err != nil {
		return err
	}
	if err := applyPositionDelta(ctx, tx, record.ConditionID, tokenID, tag, record.MarketID, outcome, "BUY", record.Shares, price, false, at); err != nil {
		return err
	}
	if err := setPositionSettlementState(ctx, tx, record.ConditionID, tokenID, record.UniqueTag, tradeStatus); err != nil {
		return err
	}
	return setPositionSettlementState(ctx, tx, record.ConditionID, tokenID, tag, tradeStatus)
}

// settlePreserveTransfer carries a preserve fill's new trade status to its
// transfer, reversing the transfer when the trade failed on chain.
func settlePreserveTransfer(ctx context.Context, tx pgx.Tx, record store.FillRecord, tradeStatus string, at time.Time) error {
	if !isInventoryTag(record.UniqueTag) {
		return nil
	}
	outID, inID := transferFillIDs(record.FillID)
	rows, err := tx.Query(ctx, `
		SELECT fill_id, unique_tag, token_id, outcome, market_id, side, shares::text, price::text, trade_status
		FROM fills WHERE fill_id IN ($1, $2) FOR UPDATE
	`, outID, inID)
	if err != nil {
		return fmt.Errorf("lock preserve transfer: %w", err)
	}
	type leg struct{ id, tag, token, outcome, market, side, shares, price, status string }
	var legs []leg
	for rows.Next() {
		var l leg
		if err := rows.Scan(&l.id, &l.tag, &l.token, &l.outcome, &l.market, &l.side, &l.shares, &l.price, &l.status); err != nil {
			rows.Close()
			return fmt.Errorf("scan preserve transfer: %w", err)
		}
		legs = append(legs, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan preserve transfer: %w", err)
	}
	for _, l := range legs {
		if l.status == "FAILED" {
			continue
		}
		if tradeStatus == "FAILED" {
			if l.side == "SELL" {
				// Back into inventory's free size, where it was taken from.
				err = applyPositionDelta(ctx, tx, record.ConditionID, l.token, l.tag, l.market, l.outcome, "BUY", l.shares, l.price, false, at)
			} else {
				err = applyPositionDelta(ctx, tx, record.ConditionID, l.token, l.tag, l.market, l.outcome, "BUY", l.shares, l.price, true, at)
			}
			if err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE fills SET trade_status = $2 WHERE fill_id = $1`, l.id, tradeStatus); err != nil {
			return fmt.Errorf("settle preserve transfer: %w", err)
		}
		if tradeStatus != "FAILED" {
			if err := setPositionSettlementState(ctx, tx, record.ConditionID, l.token, l.tag, tradeStatus); err != nil {
				return err
			}
		}
	}
	return nil
}

// reserveInventory checks, under the inventory lock, that the lane's
// remaining inventory backs a preserve reserving shares.
func reserveInventory(ctx context.Context, tx pgx.Tx, invTag, conditionID, shares string) error {
	if err := lockInventory(ctx, tx, invTag, conditionID); err != nil {
		return err
	}
	// A lane with a merge recorded is closing: its pairs go to the merge.
	var enough bool
	if err := tx.QueryRow(ctx, `
		SELECT (`+remainingSQL+`)::numeric >= $3::numeric AND NOT EXISTS (
			SELECT 1 FROM ctf_operations c
			WHERE c.condition_id = $1 AND c.unique_tag || '`+inventorySuffix+`' = $2
				AND c.action = 'MERGE' AND c.state <> 'FAILED')
	`, conditionID, invTag, shares).Scan(&enough); err != nil {
		return fmt.Errorf("check inventory: %w", err)
	}
	if !enough {
		return store.ErrInsufficientInventory
	}
	return nil
}

var _ store.InventoryStore = (*Store)(nil)
