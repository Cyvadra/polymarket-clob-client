// Command pnlreport rebuilds realized PnL from one executiond instance's
// fills and settlement payouts, and sends a metrics table, the wallet curve
// and one curve per strategy (the unique_tag prefix) to Telegram. Run it from
// cron once per instance, pointing --env at that instance's executiond.env.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cyvadra/utils/telegram"
	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pnlreport:", err)
		os.Exit(1)
	}
}

func run() error {
	envPath := flag.String("env", "", "executiond.env of the instance to report (required)")
	hours := flag.Int("hours", 72, "report window in hours")
	label := flag.String("label", "", "report label (default: the env file's directory name)")
	strategies := flag.String("strategies", "", "comma-separated pmm strategies.yaml paths for exclusive_group (default: PNL_STRATEGIES)")
	dryRun := flag.Bool("dry-run", false, "write the charts and table to --out instead of sending")
	outDir := flag.String("out", ".", "output directory for --dry-run")
	flag.Parse()
	if *envPath == "" {
		return fmt.Errorf("--env is required")
	}
	if *hours < 1 {
		return fmt.Errorf("--hours must be positive")
	}
	env, err := readEnv(*envPath)
	if err != nil {
		return err
	}
	if *label == "" {
		*label = filepath.Base(filepath.Dir(*envPath))
	}
	var bot *telegram.Service
	if !*dryRun {
		token, chatID := env["PNL_TELEGRAM_TOKEN"], env["PNL_TELEGRAM_CHAT_ID"]
		if token == "" || chatID == "" {
			return fmt.Errorf("%s must set PNL_TELEGRAM_TOKEN and PNL_TELEGRAM_CHAT_ID", *envPath)
		}
		if bot, err = telegram.Register("pnlreport", token, chatID); err != nil {
			return err
		}
	}
	if env["EXECUTION_POSTGRES_URL"] == "" {
		return fmt.Errorf("%s must set EXECUTION_POSTGRES_URL", *envPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	events, err := loadEvents(ctx, env["EXECUTION_POSTGRES_URL"])
	if err != nil {
		return err
	}
	end := time.Now().UTC()
	start := end.Add(-time.Duration(*hours) * time.Hour)
	series := Compute(events, start, end)
	var groupMap map[string]string
	if paths := ParsePaths(firstNonEmpty(*strategies, env["PNL_STRATEGIES"])); len(paths) > 0 {
		if groupMap, err = LoadGroups(paths); err != nil {
			return err
		}
	}
	groups := GroupSeries(series[1:], groupMap)
	tables := Table(*label, series[0], groups, *hours, end)

	// Chart 1: the wallet on the left axis, every group on a shared right axis.
	overview := []Line{{Name: WalletKey, S: series[0]}}
	for _, g := range groups {
		overview = append(overview, Line{Name: g.Series.Key, S: g.Series, RightAxis: true})
	}
	var photos []telegram.File
	add := func(name, subtitle string, lines []Line) error {
		title := fmt.Sprintf("%s · %s · %dh", *label, name, *hours)
		png, err := MultiChart(title, subtitle, lines, start, end)
		if err != nil {
			return err
		}
		photos = append(photos, telegram.File{Name: fileName(name) + ".png", Data: png, Caption: name})
		return nil
	}
	if err := add("overview", summary(series[0]), overview); err != nil {
		return err
	}
	// One chart per multi-strategy group: the group total and each member.
	for _, g := range groups {
		if len(g.Members) < 2 {
			continue
		}
		lines := []Line{{Name: g.Series.Key, S: g.Series}}
		for _, m := range g.Members {
			lines = append(lines, Line{Name: m.Key, S: m})
		}
		if err := add(g.Series.Key, summary(g.Series), lines); err != nil {
			return err
		}
	}

	if *dryRun {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			return err
		}
		for _, photo := range photos {
			if err := os.WriteFile(filepath.Join(*outDir, photo.Name), photo.Data, 0o644); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(*outDir, "report.md"), []byte(strings.Join(tables, "\n\n")), 0o644)
	}
	for _, table := range tables {
		if _, err := bot.SendMarkdown(ctx, table); err != nil {
			return fmt.Errorf("send table: %w", err)
		}
	}
	if _, err := bot.SendPhotos(ctx, photos...); err != nil {
		return fmt.Errorf("send charts: %w", err)
	}
	return nil
}

func loadEvents(ctx context.Context, url string) ([]Event, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	defer conn.Close(context.Background())

	var events []Event
	rows, err := conn.Query(ctx, `
		SELECT unique_tag, condition_id, token_id, side, shares::float8, price::float8, fee::float8,
			COALESCE(exchange_time, received_at)
		FROM fills WHERE trade_status <> 'FAILED'`)
	if err != nil {
		return nil, fmt.Errorf("query fills: %w", err)
	}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Tag, &e.Condition, &e.Token, &e.Side, &e.Shares, &e.Price, &e.Fee, &e.At); err != nil {
			return nil, fmt.Errorf("scan fill: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read fills: %w", err)
	}

	// Payouts follow fills, so a stable sort keeps a settlement after a fill
	// with the same timestamp.
	rows, err = conn.Query(ctx, `
		SELECT unique_tag, condition_id, token_id, shares::float8, payout_usd::float8, settled_at
		FROM settlement_payouts`)
	if err != nil {
		return nil, fmt.Errorf("query settlement_payouts: %w", err)
	}
	for rows.Next() {
		e := Event{Side: "SETTLE"}
		if err := rows.Scan(&e.Tag, &e.Condition, &e.Token, &e.Shares, &e.Payout, &e.At); err != nil {
			return nil, fmt.Errorf("scan payout: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read settlement_payouts: %w", err)
	}
	return events, nil
}

// readEnv parses KEY=VALUE lines, skipping blanks and comments and
// stripping one pair of matching quotes.
func readEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "export ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		values[strings.TrimSpace(key)] = value
	}
	return values, scanner.Err()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func fileName(key string) string {
	return unsafeName.ReplaceAllString(key, "_")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
