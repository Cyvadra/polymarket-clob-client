package main

import (
	"context"
	"fmt"
	"log"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
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
func startInventory(ctx context.Context, clob *clobclient.Client, store *postgres.Store, execution *executor.Executor, wallet *equity.Tracker,
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
		// A split spends cash neither the open buys nor the splits still in
		// flight have committed.
		FreeCash: func(ctx context.Context) (float64, error) {
			balance, err := clob.BalanceAllowance(ctx, "COLLATERAL", "")
			if err != nil {
				return 0, err
			}
			cash, err := decimal.FromBaseUnits(balance.Balance)
			if err != nil {
				return 0, err
			}
			committed, err := store.OpenBuyNotional(ctx)
			if err != nil {
				return 0, err
			}
			open, err := decimal.NonNegativeFloat(committed)
			if err != nil {
				return 0, err
			}
			splitting, err := store.InFlightSplitCash(ctx)
			if err != nil {
				return 0, err
			}
			pending, err := decimal.NonNegativeFloat(splitting)
			if err != nil {
				return 0, err
			}
			return cash - open - pending, nil
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
