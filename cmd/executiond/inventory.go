package main

import (
	"context"
	"fmt"
	"log"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/nats"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/inventory"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store/postgres"
)

// startInventory enables split inventory: SPLIT_PRESERVE opens, and SPLIT
// and MERGE requests sent through the relayer. It fails fast when the
// relayer is not configured, rather than at the first split.
//
// It returns the manager and the request subscription, which shutdown waits
// on before the store closes.
func startInventory(ctx context.Context, clob *clobclient.Client, store *postgres.Store, execution *executor.Executor, wallet *equity.Tracker, sizer *equity.Sizer,
	events protocol.ExecutionEventPublisher, limits *equity.DrawdownLimits, cfg config, bus *natsbus.Bus, stats *metrics) (*inventory.Manager, nats.Drainer, error) {
	if err := clob.CheckRelayer(); err != nil {
		return nil, nil, fmt.Errorf("EXECUTION_INVENTORY_ENABLED needs the relayer: %w", err)
	}
	var gate inventory.Gate
	if limits != nil {
		gate = limits
	}
	manager, err := inventory.New(inventory.Config{
		Chain: clob, Store: store, Canceler: execution, Publish: events, Gate: gate,
		// A split spends free cash through the entry sizer, so entries and
		// splits never count the same dollars free.
		ClaimCash: func(ctx context.Context, want float64) (float64, func(), error) {
			granted, claim, err := sizer.Claim(ctx, want)
			return granted, claim.Release, err
		},
		// The split or merge moved cash: equity and sizing read it afresh.
		OnBooked: wallet.Invalidate,
		OnError:  stats.errorHandler("inventory", "split inventory error"),
	})
	if err != nil {
		return nil, nil, err
	}
	if err := manager.Recover(ctx); err != nil {
		return nil, nil, fmt.Errorf("recover split inventory operations: %w", err)
	}
	execution.SetInventory(manager)
	work, err := nats.SubscribeInventory(ctx, bus, manager, cfg.AllowedStrategies, stats.errorHandler("nats_handler", "NATS handler error"))
	if err != nil {
		return nil, nil, err
	}
	log.Printf("split inventory enabled: SPLIT_PRESERVE opens and %s requests", protocol.SubjectStrategyExecutionInventory)
	return manager, work, nil
}
