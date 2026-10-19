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
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/positionfeatures"
	"github.com/Cyvadra/polymarket-clob-client/pkg/reconciler"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
	storepkg "github.com/Cyvadra/polymarket-clob-client/pkg/store"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store/postgres"
)

type config struct {
	NATSURL                    string
	AllowedStrategies          *nats.Allowlist
	PostgresURL                string
	MaxOpenBuyNotionalUSD      string
	FeatureInterval            time.Duration
	ReconcileInterval          time.Duration
	SettlementInterval         time.Duration
	MissingOrderGrace          time.Duration
	MaxTradeAge                time.Duration
	ResultPriceWait            time.Duration
	BalanceCacheTTL            time.Duration
	EquityMaxQuoteAge          time.Duration
	MaxEquityFraction          float64
	MaxDrawdown                equity.DrawdownLimit
	StrategyMaxDrawdown        map[string]equity.DrawdownLimit // by strategy name prefix
	StrategyDefaultMaxDrawdown equity.DrawdownLimit
	DrawdownStart              time.Time
	EquityFlowThresholdUSD     float64
	SizeAfterLossOnly          bool
	ConnectTimeout             time.Duration
	KeepWarmInterval           time.Duration
	ShutdownGracePeriod        time.Duration
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
	// -check-config validates the environment and exits, so a deploy can
	// refuse a bad config before it replaces the running release.
	if len(os.Args) == 2 && os.Args[1] == "-check-config" {
		if _, err := configFromEnv(); err != nil {
			log.Fatal(err)
		}
		return
	}
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
	log.Printf("trading strategies: %s", strings.Join(cfg.AllowedStrategies.Names(), ", "))
	// Several executiond instances share the bus, so everything this one
	// publishes names its wallet.
	identity := protocol.NewIdentity(clob.Address(), clob.MakerAddress(), cfg.AllowedStrategies.Names())
	events := protocol.WithIdentity(bus, identity)
	credentials, err := clob.EnsureCredentials(ctx)
	if err != nil {
		return err
	}
	clob.SetRequestTraceHook(logOrderTiming)
	warmExchangeConnections(ctx, clob, cfg.KeepWarmInterval)

	execution, err := executor.New(store, clob, time.Now)
	if err != nil {
		return err
	}
	execution.SetSubmitLatencyHook(logPrePost)
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
	positions, err := positionfeatures.New(store, events, time.Now, cfg.FeatureInterval)
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
	execution.SetEventPublisher(events)
	orders.SetEventPublisher(events)
	repair.SetEventPublisher(events)

	openWork, err := nats.SubscribeOpen(ctx, bus, execution, cfg.AllowedStrategies, onHandlerError)
	if err != nil {
		return err
	}
	if err := nats.SubscribeQuotes(bus, quotes); err != nil {
		return err
	}
	closeWork, err := nats.SubscribeClose(ctx, bus, execution, cfg.AllowedStrategies, onHandlerError)
	if err != nil {
		return err
	}
	if err := nats.SubscribePositionQuery(bus, store, identity, time.Now); err != nil {
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
	recorder, err := equity.NewRecorder(wallet, store, cfg.EquityFlowThresholdUSD)
	if err != nil {
		return err
	}
	var limits *equity.DrawdownLimits
	if cfg.drawdownLimited() {
		limits, err = drawdownLimits(store, cfg)
		if err != nil {
			return err
		}
		if err := refreshDrawdown(ctx, limits); err != nil {
			return err
		}
		execution.SetOpenGate(limits)
	}
	// Loss-anchored sizing takes its base from the snapshot after the latest
	// settled loss. With none recorded yet, the first sweep records one.
	var anchor *equity.LossAnchor
	if cfg.SizeAfterLossOnly {
		anchor, err = equity.NewLossAnchor(store)
		if err != nil {
			return err
		}
		if err := refreshAnchor(ctx, anchor); err != nil {
			return err
		}
	}
	// Equity is recorded once lanes have settled and no winner is left
	// unsettled: one the wallet already redeemed has no payout recorded yet,
	// and its cash would read as a deposit and its lane as a loss.
	// A payout or loss swept before a restart and not yet counted by a
	// snapshot still needs one.
	pending, err := store.PendingTradeCash(ctx)
	if err != nil {
		return err
	}
	equityDue, anchorStale := len(pending.PayoutIDs) > 0, false
	sweeper.SetSweepHandler(func(s settlement.Sweep) {
		// A failed refresh would leave sizing on the pre-loss base until
		// another snapshot happened to be recorded; retry it every sweep.
		if anchorStale {
			if err := refreshAnchor(ctx, anchor); err != nil {
				log.Printf("refresh sizing base: %v", err)
			} else {
				anchorStale = false
			}
		}
		if s.Lost+s.Redeemed+s.Shrunk+s.Dust+s.Reserved+s.Settling > 0 {
			log.Printf("settlement sweep: %d lanes checked, emptied %d lost, %d redeemed and %d dust, shrank %d to the wallet, left %d to a working close and %d still settling",
				s.Checked, s.Lost, s.Redeemed, s.Dust, s.Shrunk, s.Reserved, s.Settling)
		}
		if s.Lost+s.Redeemed+s.Shrunk+s.Dust > 0 {
			equityDue = true
		}
		// With nothing held, no settlement will come to record a deposit or
		// withdrawal, so one is looked for directly. A failed pass may have
		// missed held lanes, and a redeemed winner's payout would read as a
		// deposit.
		if !equityDue && !s.Failed && s.Checked == 0 {
			moved, err := recorder.CashMoved(ctx)
			if err != nil {
				log.Printf("check for deposits and withdrawals: %v", err)
			}
			equityDue = moved
		}
		baseDue := anchor != nil && !anchor.Ready()
		if !equityDue && !baseDue {
			return
		}
		if held := s.WinnersHeld(); held > 0 {
			log.Printf("equity snapshot deferred: %d winning lanes not yet settled", held)
			return
		}
		// The recorder tags the snapshot as after a loss when it counts one.
		reason := equity.ReasonSettlement
		if baseDue {
			reason = equity.ReasonSizingBase
		}
		if !recordSettledEquity(ctx, recorder, reason, limits) {
			return
		}
		equityDue = false
		// Any snapshot may carry a deposit or withdrawal the base follows.
		if anchor != nil {
			if err := refreshAnchor(ctx, anchor); err != nil {
				log.Printf("refresh sizing base: %v", err)
				anchorStale = true
			}
		}
	})
	// A fill moves the exchange balance and shrinks the open-buy reservations
	// at once; a cached pre-fill balance would count that cash twice.
	fills.SetFillHook(wallet.Invalidate)
	if err := nats.SubscribeBalanceQuery(bus, wallet, identity); err != nil {
		return err
	}
	sizer, err := equity.NewSizer(wallet, store)
	if err != nil {
		return err
	}
	sizer.SetMaxFraction(cfg.MaxEquityFraction)
	if anchor != nil {
		sizer.SetLossAnchor(anchor)
	}
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
	// Opens and closes run off the NATS delivery goroutine, so draining the
	// bus does not wait for them. Wait here, before the store closes, so an
	// order being placed is not cut off between persisting and its POST.
	for name, work := range map[string]nats.Drainer{"opens": openWork, "closes": closeWork} {
		if err := work.Wait(shutdownCtx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("wait for in-flight %s: %w", name, err))
		}
	}
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
	var deprecated []string // bare-fraction limits, warned of once the config loads
	cfg := config{
		NATSURL:                env("EXECUTION_NATS_URL", "nats://127.0.0.1:4222"),
		PostgresURL:            env("EXECUTION_POSTGRES_URL", "postgres://user:password@127.0.0.1:5432/execution?sslmode=disable"),
		MaxOpenBuyNotionalUSD:  strings.TrimSpace(os.Getenv("EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD")),
		FeatureInterval:        durationEnv("EXECUTION_POSITION_FEATURE_INTERVAL", 500*time.Millisecond),
		ReconcileInterval:      durationEnv("EXECUTION_RECONCILE_INTERVAL", 30*time.Second),
		SettlementInterval:     durationEnv("EXECUTION_SETTLEMENT_SWEEP_INTERVAL", time.Minute),
		MissingOrderGrace:      durationEnv("EXECUTION_MISSING_ORDER_GRACE_PERIOD", 2*time.Minute),
		MaxTradeAge:            durationEnv("EXECUTION_RECONCILE_MAX_TRADE_AGE", 24*time.Hour),
		ResultPriceWait:        durationEnv("EXECUTION_RESULT_PRICE_WAIT", accountfeed.DefaultPriceWait),
		BalanceCacheTTL:        durationEnv("EXECUTION_BALANCE_CACHE_TTL", equity.DefaultCashTTL),
		EquityMaxQuoteAge:      durationEnv("EXECUTION_EQUITY_MAX_QUOTE_AGE", 30*time.Second),
		MaxEquityFraction:      equity.DefaultMaxFraction,
		EquityFlowThresholdUSD: equity.DefaultFlowThresholdUSD,
		ConnectTimeout:         durationEnv("EXECUTION_CONNECT_TIMEOUT", 10*time.Second),
		KeepWarmInterval:       durationEnv("EXECUTION_KEEPWARM_INTERVAL", 15*time.Second),
		ShutdownGracePeriod:    durationEnv("EXECUTION_SHUTDOWN_GRACE_PERIOD", 10*time.Second),
	}
	allowed, err := nats.ParseAllowlist(os.Getenv("EXECUTION_ALLOWED_STRATEGIES"))
	if err != nil {
		return config{}, fmt.Errorf("EXECUTION_ALLOWED_STRATEGIES must list the strategies this wallet trades, comma-separated: %w", err)
	}
	cfg.AllowedStrategies = allowed
	if allowed.Wildcard() {
		log.Printf("EXECUTION_ALLOWED_STRATEGIES is %q: this wallet acts on every strategy on the bus, so any other executiond on this NATS server repeats its trades", nats.Wildcard)
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
	if value := strings.TrimSpace(os.Getenv("EXECUTION_MAX_DRAWDOWN")); value != "" {
		limit, bare, err := parseDrawdownLimit(value)
		if err != nil {
			return config{}, fmt.Errorf("EXECUTION_MAX_DRAWDOWN: %w", err)
		}
		if bare {
			deprecated = append(deprecated, value)
		}
		cfg.MaxDrawdown = limit
	}
	if value := strings.TrimSpace(os.Getenv("EXECUTION_STRATEGY_MAX_DRAWDOWN")); value != "" {
		limits, bare, err := parseStrategyLimits(value, allowed)
		if err != nil {
			return config{}, fmt.Errorf("EXECUTION_STRATEGY_MAX_DRAWDOWN: %w", err)
		}
		cfg.StrategyMaxDrawdown = limits
		deprecated = append(deprecated, bare...)
	}
	if value := strings.TrimSpace(os.Getenv("EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN")); value != "" {
		limit, bare, err := parseDrawdownLimit(value)
		if err != nil {
			return config{}, fmt.Errorf("EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN: %w", err)
		}
		if bare {
			deprecated = append(deprecated, value)
		}
		cfg.StrategyDefaultMaxDrawdown = limit
	}
	if cfg.drawdownLimited() {
		start, err := parseStart(os.Getenv("EXECUTION_DRAWDOWN_START"))
		if err != nil {
			return config{}, fmt.Errorf("EXECUTION_DRAWDOWN_START: %w", err)
		}
		cfg.DrawdownStart = start
	}
	if value := strings.TrimSpace(os.Getenv("EXECUTION_EQUITY_FLOW_THRESHOLD_USD")); value != "" {
		threshold, err := strconv.ParseFloat(value, 64)
		if err != nil || threshold < 0 {
			return config{}, fmt.Errorf("EXECUTION_EQUITY_FLOW_THRESHOLD_USD must be a non-negative number")
		}
		cfg.EquityFlowThresholdUSD = threshold
	}
	if value := strings.TrimSpace(os.Getenv("EXECUTION_SIZE_AFTER_LOSS_ONLY")); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return config{}, fmt.Errorf("EXECUTION_SIZE_AFTER_LOSS_ONLY must be true or false")
		}
		cfg.SizeAfterLossOnly = enabled
	}
	if cfg.FeatureInterval <= 0 || cfg.ReconcileInterval <= 0 || cfg.SettlementInterval <= 0 || cfg.MissingOrderGrace <= 0 || cfg.MaxTradeAge <= 0 || cfg.ResultPriceWait < 0 || cfg.BalanceCacheTTL < 0 || cfg.EquityMaxQuoteAge < 0 || cfg.ConnectTimeout <= 0 || cfg.KeepWarmInterval < 0 || cfg.ShutdownGracePeriod <= 0 {
		return config{}, fmt.Errorf("execution durations must be positive")
	}
	for _, value := range deprecated {
		limit, _, _ := parseDrawdownLimit(value)
		log.Printf("drawdown limit %q is a bare fraction, read as %s; write it as %s, since bare numbers are deprecated", value, limit, limit)
	}
	return cfg, nil
}

// parseDrawdownLimit reads "$123" as a dollar limit or "20%" as a fraction of
// the peak. A bare number in (0, 1) is the fraction form older configs hold;
// it is still read as a fraction, reported by bare so the caller can warn.
// Any other bare number is refused, since 20 could mean 20% or $20.
func parseDrawdownLimit(value string) (limit equity.DrawdownLimit, bare bool, err error) {
	value = strings.TrimSpace(value)
	var amount float64
	switch {
	case strings.HasPrefix(value, "$"):
		amount, err = strconv.ParseFloat(strings.TrimSpace(value[1:]), 64)
		limit = equity.DollarLimit(amount)
	case strings.HasSuffix(value, "%"):
		amount, err = strconv.ParseFloat(strings.TrimSpace(value[:len(value)-1]), 64)
		limit = equity.FractionLimit(amount / 100)
	default:
		amount, err = strconv.ParseFloat(value, 64)
		limit = equity.FractionLimit(amount)
		if err != nil || !limit.Valid() {
			return equity.DrawdownLimit{}, false, fmt.Errorf("%q must be a dollar amount like $123 or a percentage like 20%%", value)
		}
		bare = true
	}
	if err != nil || !limit.Valid() {
		return equity.DrawdownLimit{}, false, fmt.Errorf("%q must be a positive dollar amount or a percentage in (0%%, 100%%)", value)
	}
	return limit, bare, nil
}

// parseStrategyLimits reads comma-separated prefix:limit pairs, each limit as
// parseDrawdownLimit reads it. Every strategy starting with a prefix counts
// toward that prefix's one limit, so prefixes may not overlap. A prefix is
// split from its limit at the last colon, so it may hold one itself, and
// must be covered by allowed: a misspelt one would otherwise leave its
// strategies without a limit. It also returns the limits written as bare fractions.
func parseStrategyLimits(value string, allowed *nats.Allowlist) (map[string]equity.DrawdownLimit, []string, error) {
	limits := map[string]equity.DrawdownLimit{}
	var bares []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		cut := strings.LastIndex(entry, ":")
		if cut < 0 {
			if strings.Trim(entry, "0123456789.") == "" {
				return nil, nil, fmt.Errorf("%q is not strategy:limit; write dollar limits without thousands separators", entry)
			}
			return nil, nil, fmt.Errorf("%q is not strategy:limit", entry)
		}
		name := strings.TrimSpace(entry[:cut])
		limit, bare, err := parseDrawdownLimit(entry[cut+1:])
		switch {
		case name == "":
			return nil, nil, fmt.Errorf("%q names no strategy", entry)
		case name == nats.Wildcard:
			return nil, nil, fmt.Errorf("%q: set a limit for every strategy with EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN", entry)
		case err != nil:
			return nil, nil, fmt.Errorf("strategy %q: %w", name, err)
		// Some entry is a prefix of name exactly when it allows the whole group.
		case !allowed.Allows(name):
			return nil, nil, fmt.Errorf("strategy prefix %q is not covered by EXECUTION_ALLOWED_STRATEGIES", name)
		}
		if _, dup := limits[name]; dup {
			return nil, nil, fmt.Errorf("strategy prefix %q is listed twice", name)
		}
		for other := range limits {
			if strings.HasPrefix(name, other) || strings.HasPrefix(other, name) {
				return nil, nil, fmt.Errorf("strategy prefixes %q and %q overlap; a strategy may count toward one limit only", other, name)
			}
		}
		limits[name] = limit
		if bare {
			bares = append(bares, strings.TrimSpace(entry[cut+1:]))
		}
	}
	if len(limits) == 0 {
		return nil, nil, fmt.Errorf("no strategy:limit pairs in %q", value)
	}
	return limits, bares, nil
}

// parseStart reads the drawdown start: an RFC 3339 time, or a UTC date.
// It is required with a drawdown limit, since the peak it is measured from
// belongs to whatever strategy is running, which only the operator knows.
func parseStart(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("required with EXECUTION_MAX_DRAWDOWN, EXECUTION_STRATEGY_MAX_DRAWDOWN or EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN")
	}
	if start, err := time.Parse(time.RFC3339, value); err == nil {
		return start, nil
	}
	if start, err := time.Parse(time.DateOnly, value); err == nil {
		return start, nil
	}
	return time.Time{}, fmt.Errorf("%q is neither an RFC 3339 time nor a YYYY-MM-DD date", value)
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

// recordSettledEquity saves the wallet's equity once positions have settled
// and refreshes the drawdown limit from it. It reports whether a snapshot
// was recorded; one that was not is retried after the next sweep.
func recordSettledEquity(ctx context.Context, recorder *equity.Recorder, reason string, limits *equity.DrawdownLimits) bool {
	snapshot, err := recorder.Record(ctx, reason)
	if err != nil {
		log.Printf("record equity after settlement: %v", err)
		return false
	}
	log.Printf("equity after settlement (%s): $%s (trade cash $%s, external flow $%s, trade index %s)",
		reason, snapshot.EquityUSD, snapshot.TradeCashUSD, snapshot.ExternalFlowUSD, snapshot.TradeIndex)
	if limits == nil {
		return true
	}
	if err := refreshDrawdown(ctx, limits); err != nil {
		log.Printf("refresh drawdown limit: %v", err)
	}
	return true
}

func refreshAnchor(ctx context.Context, anchor *equity.LossAnchor) error {
	base, ok, err := anchor.Refresh(ctx)
	if err != nil {
		return err
	}
	if !ok {
		log.Printf("equity-fraction opens size from live equity until a sizing base is recorded on a settlement sweep")
		return nil
	}
	log.Printf("sizing equity-fraction opens from $%.6f: $%s recorded %s (%s), external flow since $%.6f",
		base.EquityUSD, base.Snapshot.EquityUSD, base.Snapshot.TakenAt.Format(time.RFC3339), base.Snapshot.Reason, base.FlowUSD)
	return nil
}

// drawdownLimits builds the wallet's limit, if set, each listed prefix group's,
// and the default for the other strategies.
func drawdownLimits(s storepkg.EquityStore, cfg config) (*equity.DrawdownLimits, error) {
	var wallet *equity.DrawdownGuard
	if !cfg.MaxDrawdown.IsZero() {
		guard, err := equity.NewDrawdownGuard(s, cfg.DrawdownStart, cfg.MaxDrawdown)
		if err != nil {
			return nil, err
		}
		wallet = guard
	}
	var strategies []*equity.DrawdownGuard
	for name, limit := range cfg.StrategyMaxDrawdown {
		guard, err := equity.NewStrategyGroupDrawdownGuard(s, name, cfg.DrawdownStart, limit)
		if err != nil {
			return nil, err
		}
		strategies = append(strategies, guard)
	}
	limits := equity.NewDrawdownLimits(s, cfg.DrawdownStart, wallet, strategies...)
	if !cfg.StrategyDefaultMaxDrawdown.IsZero() {
		if err := limits.SetStrategyDefault(cfg.StrategyDefaultMaxDrawdown, cfg.AllowedStrategies.Allows); err != nil {
			return nil, err
		}
	}
	return limits, nil
}

// refreshDrawdown refreshes every guard, so one that fails does not leave
// the others stale, and returns the errors joined.
func refreshDrawdown(ctx context.Context, limits *equity.DrawdownLimits) error {
	refreshed, err := limits.Refresh(ctx)
	for _, guard := range refreshed {
		if suspended, reason := guard.Suspended(); suspended {
			log.Printf("opens suspended: %s", reason)
			continue
		}
		log.Printf("%s %s, opens allowed", guard.Label(), guard.Status())
	}
	return err
}

// drawdownLimited reports whether any drawdown limit, wallet or strategy, is set.
func (c config) drawdownLimited() bool {
	return !c.MaxDrawdown.IsZero() || len(c.StrategyMaxDrawdown) > 0 || !c.StrategyDefaultMaxDrawdown.IsZero()
}
