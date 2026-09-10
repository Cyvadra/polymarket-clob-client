// Command executiontest runs a NATS-only black-box test against executiond.
// It is intentionally separate from the execution service and never loads
// Polymarket private credentials or calls execution business methods.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/executiontest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "executiontest: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var config executiontest.Config
	config.NATSURL = env("EXECUTION_NATS_URL", "nats://127.0.0.1:4222")
	flag.StringVar(&config.ConditionID, "condition-id", "", "Polymarket condition ID")
	flag.StringVar(&config.AssetID, "asset-id", "", "Polymarket outcome token/asset ID")
	flag.StringVar(&config.AssetID, "token-id", "", "alias for --asset-id")
	flag.StringVar(&config.Outcome, "outcome", "", "explicit outcome label, for example Up or Down")
	flag.StringVar(&config.TargetUSD, "target-usd", "", "USD notional for the BUY")
	flag.StringVar(&config.BuyLimit, "buy-limit", "", "BUY limit price in (0,1)")
	flag.StringVar(&config.Strategy, "strategy", "executiontest", "strategy/lane namespace")
	flag.StringVar(&config.ReportDir, "report-dir", "reports/executiontest", "directory for Markdown reports")
	flag.DurationVar(&config.CaseTimeout, "case-timeout", 2*time.Minute, "maximum time for the BUY case")
	flag.DurationVar(&config.CleanupTimeout, "cleanup-timeout", 30*time.Second, "maximum time for cleanup and position polling")
	flag.Parse()

	if err := config.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Only the first interrupt is graceful (cleanup still runs). Once it has
	// arrived, restore default signal handling so a second Ctrl-C exits.
	go func() {
		<-ctx.Done()
		stop()
	}()
	report, err := executiontest.NewReport(config.ReportDir, config)
	if err != nil {
		return err
	}
	defer report.Close()
	report.Note("Authorization", "This command submits real orders without a confirmation prompt. Do not run it against a wallet or NATS account with unrelated activity.")

	observer, err := executiontest.NewObserver(ctx, config)
	if err != nil {
		report.Scenario("nats-preflight", executiontest.StatusFail, err.Error())
		return err
	}
	defer observer.Close()

	runner := executiontest.NewRunner(config, observer, report)
	err = runner.Run(ctx)
	fmt.Printf("executiontest report: %s\n", report.Path)
	for _, message := range observer.Messages() {
		fmt.Printf("NATS %-32s %s\n", message.Subject, message.ReceivedAt.Format(time.RFC3339Nano))
	}
	return err
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
