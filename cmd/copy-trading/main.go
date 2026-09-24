// Command copy-trading copies the listened wallets' trades that pmm forwards
// over NATS onto the configured signer, sizing each copy with a fixed USD
// amount. Which wallets are listened to is decided by pmm, not here.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/copytrading"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type config struct {
	NATSURL        string
	Subject        string
	USD            float64
	InitialDiff    float64
	MaxTradeAge    time.Duration
	ConnectTimeout time.Duration
	DryRun         bool
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

	clobConfig, err := clobclient.ConfigFromEnv()
	if err != nil {
		return err
	}
	if strings.TrimSpace(clobConfig.PrivateKey) == "" {
		return fmt.Errorf("copy-trading requires a signing key: set POLYMARKET_PRIVATE_KEY_FILE (plus POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE), or POLYMARKET_PRIVATE_KEY for local development")
	}
	clob, err := clobclient.New(clobConfig)
	if err != nil {
		return fmt.Errorf("create CLOB client: %w", err)
	}
	log.Printf("signing as %s (maker %s)", clob.Address(), clob.MakerAddress())
	if _, err := clob.EnsureCredentials(ctx); err != nil {
		return err
	}

	follower, err := copytrading.New(copytrading.Config{
		USD:         cfg.USD,
		InitialDiff: cfg.InitialDiff,
		MaxTradeAge: cfg.MaxTradeAge,
		DryRun:      cfg.DryRun,
		Logf:        log.Printf,
	}, clob)
	if err != nil {
		return err
	}

	bus, err := natsbus.New(natsbus.Config{URL: cfg.NATSURL, Name: "polymarket-copy-trading", ConnectTimeout: cfg.ConnectTimeout, OnHandlerError: func(err error) { log.Printf("NATS handler error: %v", err) }})
	if err != nil {
		return err
	}
	// Deferred calls run in reverse: stop NATS delivery first, then let the
	// copies in flight finish.
	defer follower.Close()
	if err := bus.Init(ctx); err != nil {
		return err
	}
	defer bus.Close(context.Background())

	// The NATS callback is serial, so copies run in the follower's background
	// queues: a slow order on one market must not delay the next trade.
	err = bus.Subscribe(cfg.Subject, func(_ context.Context, payload []byte) error {
		activity, err := natsbus.DecodeJSON[copytrading.Activity](payload)
		if err != nil {
			return err
		}
		if !follower.Enqueue(activity) {
			log.Printf("drop %s %s %s: shutting down", activity.Address, activity.Side, activity.AssetID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("copying trades from %s with $%.2f per trade, initial diff %.4f", cfg.Subject, cfg.USD, cfg.InitialDiff)
	if cfg.DryRun {
		log.Printf("dry run: no orders will be submitted")
	}
	return bus.Run(ctx)
}

func configFromEnv() (config, error) {
	cfg := config{
		NATSURL:        env("COPY_TRADING_NATS_URL", "nats://127.0.0.1:4222"),
		Subject:        env("COPY_TRADING_NATS_SUBJECT", copytrading.DefaultSubject),
		MaxTradeAge:    durationEnv("COPY_TRADING_MAX_TRADE_AGE", 10*time.Second),
		ConnectTimeout: durationEnv("COPY_TRADING_CONNECT_TIMEOUT", 10*time.Second),
	}
	if cfg.MaxTradeAge <= 0 || cfg.ConnectTimeout <= 0 {
		return config{}, fmt.Errorf("COPY_TRADING_MAX_TRADE_AGE and COPY_TRADING_CONNECT_TIMEOUT must be positive durations")
	}
	var err error
	if cfg.USD, err = floatEnv("COPY_TRADING_USD", 0); err != nil || !(cfg.USD > 0) {
		return config{}, fmt.Errorf("COPY_TRADING_USD must be a positive number")
	}
	if cfg.InitialDiff, err = floatEnv("COPY_TRADING_INITIAL_DIFF", 0.02); err != nil || cfg.InitialDiff < 0 || cfg.InitialDiff >= 1 {
		return config{}, fmt.Errorf("COPY_TRADING_INITIAL_DIFF must be a price in [0, 1)")
	}
	if cfg.DryRun, err = strconv.ParseBool(env("COPY_TRADING_DRY_RUN", "false")); err != nil {
		return config{}, fmt.Errorf("COPY_TRADING_DRY_RUN must be a boolean")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func floatEnv(name string, fallback float64) (float64, error) {
	value := env(name, "")
	if value == "" {
		return fallback, nil
	}
	return strconv.ParseFloat(value, 64)
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := env(name, "")
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	return -1
}
