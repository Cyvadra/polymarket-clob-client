// Command executiond runs the NATS-driven Polymarket execution runtime.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/nats"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/positionfeatures"
	"github.com/Cyvadra/polymarket-clob-client/pkg/reconciler"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store/postgres"
)

type config struct {
	NATSURL               string
	PostgresURL           string
	MaxOpenBuyNotionalUSD string
	FeatureInterval       time.Duration
	ReconcileInterval     time.Duration
	SettlementInterval    time.Duration
	MissingOrderGrace     time.Duration
	MaxTradeAge           time.Duration
	ResultPriceWait       time.Duration
	BalanceCacheTTL       time.Duration
	EquityMaxQuoteAge     time.Duration
	MaxEquityFraction     float64
	ConnectTimeout        time.Duration
	ShutdownGracePeriod   time.Duration
}

type module interface {
	Init(context.Context) error
	Run(context.Context) error
	Close(context.Context) error
}

type namedModule struct {
	name   string
	module module
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := postgres.New(ctx, postgres.Config{URL: cfg.PostgresURL, ConnectTimeout: cfg.ConnectTimeout, MaxOpenBuyNotionalUSD: cfg.MaxOpenBuyNotionalUSD})
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate execution store: %w", err)
	}

	onHandlerError := func(err error) { log.Printf("NATS handler error: %v", err) }
	bus, err := natsbus.New(natsbus.Config{URL: cfg.NATSURL, Name: "polymarket-executiond", ConnectTimeout: cfg.ConnectTimeout, OnHandlerError: onHandlerError})
	if err != nil {
		return err
	}
	if err := bus.Init(ctx); err != nil {
		return err
	}
	defer bus.Close(context.Background())

	clobConfig, err := clobclient.ConfigFromEnv()
	if err != nil {
		return err
	}
	if strings.TrimSpace(clobConfig.PrivateKey) == "" {
		return fmt.Errorf("executiond requires a signing key: set POLYMARKET_PRIVATE_KEY_FILE (plus POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE), or POLYMARKET_PRIVATE_KEY for local development")
	}
	clob, err := clobclient.New(clobConfig)
	if err != nil {
		return fmt.Errorf("create CLOB client: %w", err)
	}
	// Log the address, never the key, so an operator can confirm which wallet
	// was unlocked without reading it back out of the config.
	log.Printf("signing as %s (maker %s)", clob.Address(), clob.MakerAddress())
	credentials, err := clob.EnsureCredentials(ctx)
	if err != nil {
		return err
	}

	execution, err := executor.New(store, clob, time.Now)
	if err != nil {
		return err
	}
	quotes := marketquotes.New()
	execution.SetQuoteProvider(quotes)
	quotes.OnNewMarket(metadataWarmer(ctx, clob))
	fills, err := accountfeed.NewFillConsumer(store, time.Now)
	if err != nil {
		return err
	}
	fills.SetFeeSchedules(clob)
	orders, err := accountfeed.NewOrderConsumer(store, time.Now)
	if err != nil {
		return err
	}
	accountStream, err := accountfeed.NewUserStream(accountfeed.UserStreamConfig{Credentials: *credentials, ProxyURL: clob.ProxyURL()}, orders, fills)
	if err != nil {
		return err
	}
	repair, err := reconciler.New(store, clob, fills, credentials.APIKey, time.Now, cfg.ReconcileInterval)
	if err != nil {
		return err
	}
	repair.SetMissingOrderGrace(cfg.MissingOrderGrace)
	repair.SetMaxTradeAge(cfg.MaxTradeAge)
	positions, err := positionfeatures.New(store, bus, time.Now, cfg.FeatureInterval)
	if err != nil {
		return err
	}
	positions.SetErrorHandler(func(err error) { log.Printf("position feature publish error: %v", err) })
	accountStream.SetErrorHandler(func(err error) { log.Printf("account stream error: %v", err) })
	fills.SetErrorHandler(func(err error) { log.Printf("account fill error: %v", err) })
	orders.SetErrorHandler(func(err error) { log.Printf("account order result error: %v", err) })
	orders.SetPriceWait(cfg.ResultPriceWait)
	execution.SetErrorHandler(func(err error) { log.Printf("execution lifecycle error: %v", err) })
	repair.SetErrorHandler(func(err error) { log.Printf("reconciliation error: %v", err) })
	execution.SetEventPublisher(bus)
	orders.SetEventPublisher(bus)
	repair.SetEventPublisher(bus)

	if err := nats.SubscribeOpen(bus, execution); err != nil {
		return err
	}
	if err := nats.SubscribeQuotes(bus, quotes); err != nil {
		return err
	}
	if err := nats.SubscribeClose(ctx, bus, execution, onHandlerError); err != nil {
		return err
	}
	if err := nats.SubscribePositionQuery(bus, store, time.Now); err != nil {
		return err
	}
	wallet, err := equity.New(clob, store, quotes, time.Now)
	if err != nil {
		return err
	}
	wallet.SetCashTTL(cfg.BalanceCacheTTL)
	wallet.SetMaxQuoteAge(cfg.EquityMaxQuoteAge)
	resolver, err := settlement.NewResolver(clob, clob, time.Now)
	if err != nil {
		return err
	}
	wallet.SetSettlements(resolver)
	// Lanes in resolved markets that hold nothing of value are emptied, so
	// they stop showing up as positions and stop being looked up.
	sweeper, err := settlement.NewSweeper(store, resolver, time.Now, cfg.SettlementInterval)
	if err != nil {
		return err
	}
	sweeper.SetErrorHandler(func(err error) { log.Printf("settlement sweep error: %v", err) })
	sweeper.SetSweepHandler(func(s settlement.Sweep) {
		log.Printf("settlement sweep: %d lanes checked, emptied %d lost and %d redeemed, shrank %d to the wallet, left %d to a working close and %d still settling",
			s.Checked, s.Lost, s.Redeemed, s.Shrunk, s.Reserved, s.Settling)
	})
	// A fill moves the exchange balance and shrinks the open-buy reservations
	// at once; a cached pre-fill balance would count that cash twice.
	fills.SetFillHook(wallet.Invalidate)
	if err := nats.SubscribeBalanceQuery(bus, wallet); err != nil {
		return err
	}
	sizer, err := equity.NewSizer(wallet, store)
	if err != nil {
		return err
	}
	sizer.SetMaxFraction(cfg.MaxEquityFraction)
	execution.SetEntrySizer(sizer)
	modules := []namedModule{
		{name: "nats", module: bus},
		{name: "executor", module: execution},
		{name: "account-stream", module: accountStream},
		{name: "reconciler", module: repair},
		{name: "position-features", module: positions},
		{name: "settlement-sweeper", module: sweeper},
	}
	if err := initModules(ctx, modules); err != nil {
		return err
	}

	runErr := runModules(ctx, modules)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()
	closeErr := closeModules(shutdownCtx, modules)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return errors.Join(runErr, closeErr)
	}
	return closeErr
}

// metadataWarmer loads a market's order metadata and fee schedule as soon as
// its first quote arrives, so the first order on it signs without a network
// round trip and its fills are priced without one. PMM
// quotes a market from registration and signals it a minute or more later.
// At startup every live market is announced at once, so warming is bounded.
func metadataWarmer(ctx context.Context, clob *clobclient.Client) func(marketquotes.Snapshot) {
	const concurrent, timeout = 4, 15 * time.Second
	slots := make(chan struct{}, concurrent)
	return func(snapshot marketquotes.Snapshot) {
		go func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			warmCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if _, err := clob.FeeSchedule(warmCtx, snapshot.ConditionID); err != nil && ctx.Err() == nil {
				log.Printf("warm fee schedule %s: %v", snapshot.ConditionID, err)
			}
			for _, tokenID := range []string{snapshot.Up.AssetID, snapshot.Down.AssetID} {
				if err := clob.WarmMarketMetadata(warmCtx, tokenID); err != nil && ctx.Err() == nil {
					log.Printf("warm market metadata %s/%s: %v", snapshot.ConditionID, tokenID, err)
				}
			}
		}()
	}
}

func initModules(ctx context.Context, modules []namedModule) error {
	initialized := make([]namedModule, 0, len(modules))
	for _, module := range modules {
		if module.name == "" || module.module == nil {
			return fmt.Errorf("execution module name and implementation are required")
		}
		if err := module.module.Init(ctx); err != nil {
			closeErr := closeModules(ctx, initialized)
			return errors.Join(fmt.Errorf("init %s: %w", module.name, err), closeErr)
		}
		initialized = append(initialized, module)
	}
	return nil
}

func runModules(ctx context.Context, modules []namedModule) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(modules))
	var wg sync.WaitGroup
	for _, module := range modules {
		module := module
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := module.module.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				errs <- fmt.Errorf("run %s: %w", module.name, err)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errs)
	}()

	var runErr error
	for err := range errs {
		cancel()
		runErr = errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	return ctx.Err()
}

func closeModules(ctx context.Context, modules []namedModule) error {
	var closeErr error
	for idx := len(modules) - 1; idx >= 0; idx-- {
		module := modules[idx]
		if err := module.module.Close(ctx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close %s: %w", module.name, err))
		}
	}
	return closeErr
}

func configFromEnv() (config, error) {
	cfg := config{
		NATSURL:               env("EXECUTION_NATS_URL", "nats://127.0.0.1:4222"),
		PostgresURL:           env("EXECUTION_POSTGRES_URL", "postgres://user:password@127.0.0.1:5432/execution?sslmode=disable"),
		MaxOpenBuyNotionalUSD: strings.TrimSpace(os.Getenv("EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD")),
		FeatureInterval:       durationEnv("EXECUTION_POSITION_FEATURE_INTERVAL", 500*time.Millisecond),
		ReconcileInterval:     durationEnv("EXECUTION_RECONCILE_INTERVAL", 30*time.Second),
		SettlementInterval:    durationEnv("EXECUTION_SETTLEMENT_SWEEP_INTERVAL", time.Minute),
		MissingOrderGrace:     durationEnv("EXECUTION_MISSING_ORDER_GRACE_PERIOD", 2*time.Minute),
		MaxTradeAge:           durationEnv("EXECUTION_RECONCILE_MAX_TRADE_AGE", 24*time.Hour),
		ResultPriceWait:       durationEnv("EXECUTION_RESULT_PRICE_WAIT", accountfeed.DefaultPriceWait),
		BalanceCacheTTL:       durationEnv("EXECUTION_BALANCE_CACHE_TTL", equity.DefaultCashTTL),
		EquityMaxQuoteAge:     durationEnv("EXECUTION_EQUITY_MAX_QUOTE_AGE", 30*time.Second),
		MaxEquityFraction:     equity.DefaultMaxFraction,
		ConnectTimeout:        durationEnv("EXECUTION_CONNECT_TIMEOUT", 10*time.Second),
		ShutdownGracePeriod:   durationEnv("EXECUTION_SHUTDOWN_GRACE_PERIOD", 10*time.Second),
	}
	if cfg.MaxOpenBuyNotionalUSD != "" && !decimal.Positive(cfg.MaxOpenBuyNotionalUSD) {
		return config{}, fmt.Errorf("EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD must be a positive decimal")
	}
	if value := strings.TrimSpace(os.Getenv("EXECUTION_MAX_EQUITY_FRACTION")); value != "" {
		fraction, err := strconv.ParseFloat(value, 64)
		if err != nil || fraction <= 0 || fraction > 1 {
			return config{}, fmt.Errorf("EXECUTION_MAX_EQUITY_FRACTION must be in (0, 1]")
		}
		cfg.MaxEquityFraction = fraction
	}
	if cfg.FeatureInterval <= 0 || cfg.ReconcileInterval <= 0 || cfg.SettlementInterval <= 0 || cfg.MissingOrderGrace <= 0 || cfg.MaxTradeAge <= 0 || cfg.ResultPriceWait < 0 || cfg.BalanceCacheTTL < 0 || cfg.EquityMaxQuoteAge < 0 || cfg.ConnectTimeout <= 0 || cfg.ShutdownGracePeriod <= 0 {
		return config{}, fmt.Errorf("execution durations must be positive")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	if milliseconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(milliseconds) * time.Millisecond
	}
	return -1
}
