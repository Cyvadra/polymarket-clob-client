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
	"syscall"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/nats"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/positionfeatures"
	"github.com/Cyvadra/polymarket-clob-client/pkg/reconciler"
	"github.com/Cyvadra/polymarket-clob-client/pkg/service"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store/postgres"
)

type config struct {
	NATSURL             string
	PostgresURL         string
	FeatureInterval     time.Duration
	ReconcileInterval   time.Duration
	ConnectTimeout      time.Duration
	ShutdownGracePeriod time.Duration
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

	store, err := postgres.New(ctx, postgres.Config{URL: cfg.PostgresURL, ConnectTimeout: cfg.ConnectTimeout})
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate execution store: %w", err)
	}

	bus, err := natsbus.New(natsbus.Config{URL: cfg.NATSURL, Name: "polymarket-executiond", ConnectTimeout: cfg.ConnectTimeout, OnHandlerError: func(err error) { log.Printf("NATS handler error: %v", err) }})
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
	clob, err := clobclient.New(clobConfig)
	if err != nil {
		return fmt.Errorf("create CLOB client: %w", err)
	}
	if clobConfig.Credentials == nil {
		return fmt.Errorf("POLYMARKET_API_KEY, POLYMARKET_API_SECRET, and POLYMARKET_API_PASSPHRASE are required for executiond")
	}

	execution, err := executor.New(store, clob, time.Now)
	if err != nil {
		return err
	}
	quotes := marketquotes.New()
	execution.SetQuoteProvider(quotes)
	fills, err := accountfeed.NewFillConsumer(store, time.Now)
	if err != nil {
		return err
	}
	orders, err := accountfeed.NewOrderConsumer(store, time.Now)
	if err != nil {
		return err
	}
	accountStream, err := accountfeed.NewUserStream(accountfeed.UserStreamConfig{Credentials: *clobConfig.Credentials}, orders, fills)
	if err != nil {
		return err
	}
	repair, err := reconciler.New(store, clob, time.Now, cfg.ReconcileInterval)
	if err != nil {
		return err
	}
	positions, err := positionfeatures.New(store, bus, time.Now, cfg.FeatureInterval)
	if err != nil {
		return err
	}
	positions.SetErrorHandler(func(err error) { log.Printf("position feature publish error: %v", err) })
	accountStream.SetErrorHandler(func(err error) { log.Printf("account stream error: %v", err) })
	execution.SetErrorHandler(func(err error) { log.Printf("execution lifecycle error: %v", err) })
	repair.SetErrorHandler(func(err error) { log.Printf("reconciliation error: %v", err) })
	execution.SetEventPublisher(bus)
	orders.SetEventPublisher(bus)
	repair.SetEventPublisher(bus)

	if err := nats.SubscribeIntents(bus, execution); err != nil {
		return err
	}
	if err := nats.SubscribeQuotes(bus, quotes); err != nil {
		return err
	}
	runtime, err := service.New(
		service.NamedModule{Name: "executor", Module: execution},
		service.NamedModule{Name: "account-stream", Module: accountStream},
		service.NamedModule{Name: "reconciler", Module: repair},
		service.NamedModule{Name: "position-features", Module: positions},
	)
	if err != nil {
		return err
	}
	if err := runtime.Init(ctx); err != nil {
		return err
	}

	runErr := runtime.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()
	closeErr := runtime.Close(shutdownCtx)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return errors.Join(runErr, closeErr)
	}
	return closeErr
}

func configFromEnv() (config, error) {
	cfg := config{
		NATSURL:             os.Getenv("EXECUTION_NATS_URL"),
		PostgresURL:         os.Getenv("EXECUTION_POSTGRES_URL"),
		FeatureInterval:     durationEnv("EXECUTION_POSITION_FEATURE_INTERVAL", 500*time.Millisecond),
		ReconcileInterval:   durationEnv("EXECUTION_RECONCILE_INTERVAL", 30*time.Second),
		ConnectTimeout:      durationEnv("EXECUTION_CONNECT_TIMEOUT", 10*time.Second),
		ShutdownGracePeriod: durationEnv("EXECUTION_SHUTDOWN_GRACE_PERIOD", 10*time.Second),
	}
	if cfg.NATSURL == "" || cfg.PostgresURL == "" {
		return config{}, fmt.Errorf("EXECUTION_NATS_URL and EXECUTION_POSTGRES_URL are required")
	}
	if cfg.FeatureInterval <= 0 || cfg.ReconcileInterval <= 0 || cfg.ConnectTimeout <= 0 || cfg.ShutdownGracePeriod <= 0 {
		return config{}, fmt.Errorf("execution durations must be positive")
	}
	return cfg, nil
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
