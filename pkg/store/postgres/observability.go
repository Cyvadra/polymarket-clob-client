package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// StartDaemonRun records a starting executiond process and returns its run ID.
func (s *Store) StartDaemonRun(ctx context.Context, run store.DaemonRun) (int64, error) {
	config, err := json.Marshal(run.Config)
	if err != nil {
		return 0, fmt.Errorf("encode daemon run config: %w", err)
	}
	strategies := run.Strategies
	if strategies == nil {
		strategies = []string{}
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO daemon_runs (version, signer, maker, strategies, config)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		RETURNING run_id
	`, run.Version, run.Signer, run.Maker, strategies, string(config)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start daemon run: %w", err)
	}
	return id, nil
}

// StopDaemonRun stamps a run's stop time.
func (s *Store) StopDaemonRun(ctx context.Context, runID int64, at time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE daemon_runs SET stopped_at = $2 WHERE run_id = $1`, runID, at); err != nil {
		return fmt.Errorf("stop daemon run: %w", err)
	}
	return nil
}

// RecordSweep saves one settlement sweep pass.
func (s *Store) RecordSweep(ctx context.Context, sweep store.SweepRecord) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settlement_sweeps (run_id, swept_at, checked, lost, redeemed, dust, shrunk, closing, settling, failed)
		VALUES (NULLIF($1::bigint, 0), $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, sweep.RunID, sweep.At, sweep.Checked, sweep.Lost, sweep.Redeemed, sweep.Dust, sweep.Shrunk, sweep.Closing, sweep.Settling, sweep.Failed); err != nil {
		return fmt.Errorf("record settlement sweep: %w", err)
	}
	return nil
}

// RecordRiskEvent saves a risk or sizing decision.
func (s *Store) RecordRiskEvent(ctx context.Context, event store.RiskEvent) error {
	if event.Kind == "" {
		return fmt.Errorf("risk event kind is required")
	}
	details := event.Details
	if details == nil {
		details = map[string]any{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode risk event details: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO risk_events (run_id, at, kind, subject, reason, details)
		VALUES (NULLIF($1::bigint, 0), $2, $3, $4, $5, $6::jsonb)
	`, event.RunID, event.At, event.Kind, event.Subject, event.Reason, string(encoded)); err != nil {
		return fmt.Errorf("record risk event: %w", err)
	}
	return nil
}
