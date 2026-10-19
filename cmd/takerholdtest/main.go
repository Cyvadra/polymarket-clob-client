// Command takerholdtest probes whether an order can be canceled while the
// exchange holds it in the taker-delay window. Each loop signs a BUY at the
// entry price, posts it, and cancels it by its locally computed order ID
// before the POST returns, on a fixed cadence for a fixed number of tries.
// A BUY that crosses the best ask runs as a taker (held); one below it rests
// as a maker, the no-hold baseline. The run stops at the
// first fill (or the timeout) and sells any position it bought back with a
// FAK at the floor price.
//
// It loads the signing key from the environment (POLYMARKET_PRIVATE_KEY or
// POLYMARKET_PRIVATE_KEY_FILE, POLYMARKET_MAKER_ADDRESS,
// POLYMARKET_SIGNATURE_TYPE) and submits real orders without a prompt.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

const userStreamURL = "wss://ws-subscriptions-clob.polymarket.com/ws/user"

type config struct {
	ConditionID string
	TokenID     string
	Shares      float64
	Taker       bool // set by preflight: the entry price crosses the best ask
	Price       float64
	SellPrice   float64
	FirstCancel time.Duration
	RetryEvery  time.Duration
	Retries     int
	Settle      time.Duration
	Timeout     time.Duration
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "takerholdtest: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.ConditionID, "condition-id", "", "target market condition ID")
	flag.StringVar(&cfg.TokenID, "token-id", "", "outcome token to BUY")
	flag.Float64Var(&cfg.Shares, "shares", 0, "shares per order; 0 sizes it to max(minimum shares, $1.50 notional)")
	flag.Float64Var(&cfg.Price, "price", 0.99, "BUY entry price: at or above the best ask runs as a held taker, below it as a resting maker")
	flag.Float64Var(&cfg.SellPrice, "sell-price", 0.01, "FAK SELL price for the sell-back")
	flag.DurationVar(&cfg.FirstCancel, "first-cancel", 60*time.Millisecond, "delay from POST start to the first cancel")
	flag.DurationVar(&cfg.RetryEvery, "retry-every", 10*time.Millisecond, "cadence of cancel retries after a refusal")
	flag.IntVar(&cfg.Retries, "retries", 22, "cancel retries after the first refusal")
	flag.DurationVar(&cfg.Settle, "settle", 1500*time.Millisecond, "wait after each loop for a fill to show")
	flag.DurationVar(&cfg.Timeout, "timeout", time.Minute, "total loop duration")
	flag.Parse()
	if cfg.ConditionID == "" || cfg.TokenID == "" {
		return errors.New("--condition-id and --token-id are required")
	}
	if cfg.Shares < 0 || cfg.Price <= 0 || cfg.Price >= 1 || cfg.SellPrice <= 0 || cfg.SellPrice >= 1 || cfg.Retries < 0 {
		return errors.New("invalid --shares, --price, --sell-price or --retries")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clobConfig, err := clobclient.ConfigFromEnv()
	if err != nil {
		return err
	}
	if strings.TrimSpace(clobConfig.PrivateKey) == "" {
		return errors.New("a signing key is required: set POLYMARKET_PRIVATE_KEY_FILE (plus POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE) or POLYMARKET_PRIVATE_KEY")
	}
	clob, err := clobclient.New(clobConfig)
	if err != nil {
		return fmt.Errorf("create CLOB client: %w", err)
	}
	log.Printf("signing as %s (maker %s, signature type %d)", clob.Address(), clob.MakerAddress(), clobConfig.SignatureType)
	if proxy := clob.ProxyURL(); proxy != nil {
		log.Printf("proxy: %s (POLYMARKET_PROXY_URL)", proxy.Redacted())
	} else {
		log.Printf("proxy: from environment (HTTPS_PROXY/HTTP_PROXY/NO_PROXY)")
	}
	creds, err := clob.EnsureCredentials(ctx)
	if err != nil {
		return err
	}

	minSize, startBalance, err := preflight(ctx, clob, &cfg)
	if err != nil {
		return err
	}

	// The POST and every cancel share the order pool. Warm it before the
	// first round and keep it warm, so they ride one open HTTP/2 connection
	// instead of paying a dial and TLS handshake inside the timed window.
	// Any request that still opens a connection is logged.
	clob.SetRequestTraceHook(func(t transport.Timing) {
		if !t.Reused {
			log.Printf("cold connection: %s %s connect=%s tls=%s ttfb=%s", t.Method, t.Path, t.Connect, t.TLS, t.FirstByte)
		}
	})
	ordersWarm, readsWarm, err := clob.Warm(ctx)
	if err != nil {
		return fmt.Errorf("warm connections: %w", err)
	}
	log.Printf("connections warm: orders %s, reads %s", ordersWarm.Round(100*time.Microsecond), readsWarm.Round(100*time.Microsecond))

	fills := &fillWatch{conditionID: cfg.ConditionID, orders: map[string]bool{}}
	dialer := *websocket.DefaultDialer // Proxy: http.ProxyFromEnvironment
	if proxy := clob.ProxyURL(); proxy != nil {
		dialer.Proxy = http.ProxyURL(proxy)
	}
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	if err := fills.start(streamCtx, &dialer, creds); err != nil {
		return err
	}
	go clob.KeepWarm(streamCtx, 5*time.Second, func(_, _ time.Duration, err error) {
		if err != nil {
			log.Printf("keep-warm ping failed: %v", err)
		}
	})

	result := &report{}
	defer func() {
		cleanup(clob, cfg, fills, startBalance, minSize, result)
		result.print()
	}()
	return loop(ctx, clob, cfg, fills, result)
}

// preflight fills in cfg.Shares (when 0) and cfg.Taker, and refuses a target
// the probe cannot learn from: no taker hold, a closed market, a price off
// the tick, or too few shares.
func preflight(ctx context.Context, clob *clobclient.Client, cfg *config) (minSize, balance float64, err error) {
	held, err := clob.TakerDelayEnabled(ctx, cfg.ConditionID)
	if err != nil {
		return 0, 0, fmt.Errorf("read taker delay: %w", err)
	}
	if !held {
		return 0, 0, errors.New("abort: no taker hold on this market (itode is not true)")
	}
	market, err := clob.Market(ctx, cfg.ConditionID)
	if err != nil {
		return 0, 0, fmt.Errorf("read market: %w", err)
	}
	if market.Closed {
		return 0, 0, errors.New("abort: market is closed")
	}
	found := false
	for _, token := range market.Tokens {
		found = found || token.TokenID == cfg.TokenID
	}
	if !found {
		return 0, 0, fmt.Errorf("abort: token %s is not in market %s", cfg.TokenID, cfg.ConditionID)
	}
	tick, err := clob.TickSize(ctx, cfg.TokenID)
	if err != nil {
		return 0, 0, fmt.Errorf("read tick size: %w", err)
	}
	for _, price := range []float64{cfg.Price, cfg.SellPrice} {
		if math.Abs(math.Round(price/tick)*tick-price) > 1e-9 {
			return 0, 0, fmt.Errorf("abort: price %.4f is not on tick %.4f", price, tick)
		}
	}
	if minSize, err = clob.MinOrderSize(ctx, cfg.TokenID); err != nil {
		return 0, 0, fmt.Errorf("read minimum order size: %w", err)
	}
	if cfg.Shares == 0 {
		cfg.Shares = entryShares(cfg.Price, minSize)
	}
	if cfg.Shares < minSize {
		return 0, 0, fmt.Errorf("abort: %.4f shares is below the minimum %.4f", cfg.Shares, minSize)
	}
	if cfg.Taker, err = crossing(ctx, clob, *cfg); err != nil {
		return 0, 0, err
	}
	if balance, err = tokenBalance(ctx, clob, cfg.TokenID); err != nil {
		return 0, 0, err
	}
	log.Printf("preflight ok: itode=true tick=%g min_size=%g start_balance=%.6f mode=%s order=BUY %g @ %g ($%.2f)",
		tick, minSize, balance, mode(cfg.Taker), cfg.Shares, cfg.Price, cfg.Shares*cfg.Price)
	return minSize, balance, nil
}

// entryShares sizes an order to at least the market's minimum shares and at
// least $1.50 notional, safely above the exchange's $1 minimum, rounded up
// to whole cents of a share.
func entryShares(price, minSize float64) float64 {
	return math.Max(minSize, math.Ceil(1.5/price*100)/100)
}

func mode(taker bool) string {
	if taker {
		return "taker"
	}
	return "maker"
}

// crossing reports whether the BUY would take (the best ask is at or below
// the entry price) rather than rest on the book.
func crossing(ctx context.Context, clob *clobclient.Client, cfg config) (bool, error) {
	book, err := clob.OrderBook(ctx, cfg.TokenID)
	if err != nil {
		return false, fmt.Errorf("read book: %w", err)
	}
	best := math.Inf(1)
	for _, level := range book.Asks {
		if price, err := strconv.ParseFloat(level.Price, 64); err == nil && price < best {
			best = price
		}
	}
	return best <= cfg.Price+1e-9, nil
}

func tokenBalance(ctx context.Context, clob *clobclient.Client, tokenID string) (float64, error) {
	balance, err := clob.BalanceAllowance(ctx, "CONDITIONAL", tokenID)
	if err != nil {
		return 0, fmt.Errorf("read token balance: %w", err)
	}
	units, err := strconv.ParseFloat(balance.Balance, 64)
	if err != nil {
		return 0, fmt.Errorf("parse token balance %q: %w", balance.Balance, err)
	}
	return units / 1e6, nil
}

type attempt struct {
	offset time.Duration
	took   time.Duration
	ok     bool
	reason string
}

type round struct {
	n        int
	orderID  string
	postTook time.Duration
	status   string
	postErr  error
	attempts []attempt
}

func (r round) canceled() bool {
	for _, a := range r.attempts {
		if a.ok {
			return true
		}
	}
	return false
}

type report struct {
	rounds     []round
	successes  int
	stopReason string
	sold       string
}

func loop(ctx context.Context, clob *clobclient.Client, cfg config, fills *fillWatch, result *report) error {
	deadline := time.Now().Add(cfg.Timeout)
	for n := 1; ; n++ {
		if time.Now().After(deadline) {
			result.stopReason = "timeout"
			return nil
		}
		if ctx.Err() != nil {
			result.stopReason = "interrupted"
			return nil
		}
		// Every round must run in the mode preflight chose, or the rounds
		// would mix held and unheld orders.
		taker, err := crossing(ctx, clob, cfg)
		if err != nil {
			result.stopReason = err.Error()
			return err
		}
		if taker != cfg.Taker {
			result.stopReason = fmt.Sprintf("book moved: the BUY @ %g would now run as a %s, not a %s", cfg.Price, mode(taker), mode(cfg.Taker))
			return nil
		}
		r, err := probe(ctx, clob, cfg, fills, n)
		result.rounds = append(result.rounds, r)
		if err != nil {
			result.stopReason = err.Error()
			return err
		}
		wait(ctx, cfg.Settle)
		if fills.filled() || r.status == "matched" {
			result.stopReason = fmt.Sprintf("fill on round %d", n)
			return nil
		}
		if !r.canceled() {
			// Unfilled yet never canceled: the order may still rest on the
			// book, and another round would stack a second one.
			result.stopReason = fmt.Sprintf("round %d: order neither filled nor canceled", n)
			return nil
		}
		result.successes++
	}
}

// probe runs one submit-and-cancel round. The POST runs in its own goroutine
// because the exchange answers it only after the hold, while the cancels go
// out by the order ID the client computed when signing.
func probe(ctx context.Context, clob *clobclient.Client, cfg config, fills *fillWatch, n int) (round, error) {
	signed, err := clob.CreateOrder(ctx, clobclient.UserOrder{TokenID: cfg.TokenID, Side: clobclient.SideBuy, Price: cfg.Price, Shares: cfg.Shares, OrderType: clobclient.OrderTypeGTC})
	if err != nil {
		return round{n: n}, fmt.Errorf("sign order: %w", err)
	}
	r := round{n: n, orderID: signed.OrderID}
	fills.track(signed.OrderID)

	type posted struct {
		resp *clobclient.OrderResponse
		err  error
		took time.Duration
	}
	done := make(chan posted, 1)
	// The POST and cancels outlive Ctrl-C so an order in flight is still
	// accounted for; cleanup sweeps anything left.
	work, cancelWork := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWork()
	t0 := time.Now()
	go func() {
		resp, err := clob.SubmitSignedOrder(work, signed, clobclient.OrderTypeGTC, false)
		done <- posted{resp, err, time.Since(t0)}
	}()

	// Cancels fire on a fixed schedule, each in its own goroutine, so one
	// cancel's round trip (~100ms) never delays the next. All of them go out
	// even after one lands or the POST answers, so every round samples the
	// same window.
	var inflight sync.WaitGroup
	results := make(chan attempt, cfg.Retries+1)
	send := func(sent time.Time) {
		defer inflight.Done()
		res, err := clob.CancelOrderResult(work, signed.OrderID)
		a := attempt{offset: sent.Sub(t0), took: time.Since(sent)}
		switch {
		case err != nil:
			a.reason = err.Error()
		case res.Has(signed.OrderID):
			a.ok = true
		default:
			a.reason = res.NotCanceled[signed.OrderID]
			for id, reason := range res.NotCanceled {
				if a.reason == "" {
					a.reason = id + ": " + reason
				}
			}
		}
		results <- a
	}
	for i := 0; i <= cfg.Retries; i++ {
		time.Sleep(time.Until(t0.Add(cfg.FirstCancel + time.Duration(i)*cfg.RetryEvery)))
		inflight.Add(1)
		go send(time.Now())
	}
	inflight.Wait()
	close(results)
	for a := range results {
		r.attempts = append(r.attempts, a)
	}
	sort.Slice(r.attempts, func(i, j int) bool { return r.attempts[i].offset < r.attempts[j].offset })
	post := <-done
	r.postTook, r.postErr = post.took, post.err
	if post.resp != nil {
		r.status = post.resp.Status
	}
	logRound(r)
	return r, nil
}

func logRound(r round) {
	var b strings.Builder
	fmt.Fprintf(&b, "round %d order=%s post=%s status=%q", r.n, r.orderID, r.postTook.Round(100*time.Microsecond), r.status)
	if r.postErr != nil {
		fmt.Fprintf(&b, " post_err=%q", r.postErr.Error())
	}
	for i, a := range r.attempts {
		result := "refused"
		if a.ok {
			result = "CANCELED"
		}
		fmt.Fprintf(&b, "\n  cancel #%d at +%s (%s): %s", i+1, a.offset.Round(100*time.Microsecond), a.took.Round(100*time.Microsecond), result)
		if a.reason != "" {
			fmt.Fprintf(&b, " %q", a.reason)
		}
	}
	log.Print(b.String())
}

// cleanup cancels anything left on the market, waits for any fill to land
// in the balance, and sells the increase back with a FAK at the floor price.
func cleanup(clob *clobclient.Client, cfg config, fills *fillWatch, startBalance, minSize float64, result *report) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := clob.CancelMarketOrders(ctx, cfg.ConditionID, cfg.TokenID); err != nil {
		log.Printf("cleanup: cancel market orders: %v", err)
	}
	if !fills.filled() && !anyMatched(result.rounds) {
		result.sold = "nothing to sell"
		return
	}
	// A fill reaches the balance only once it settles; poll until it grows
	// and holds steady.
	var held, last float64
	for i := 0; i < 30; i++ {
		balance, err := tokenBalance(ctx, clob, cfg.TokenID)
		if err == nil {
			held = balance - startBalance
			if held > 1e-9 && math.Abs(balance-last) < 1e-9 {
				break
			}
			last = balance
		}
		if !wait(ctx, 500*time.Millisecond) {
			break
		}
	}
	digits := clobclient.SharePrecisionDigits(clobclient.SideSell, clobclient.OrderTypeFAK)
	scale := math.Pow10(digits)
	shares := math.Floor(held*scale) / scale
	switch {
	case shares <= 0:
		result.sold = fmt.Sprintf("no position increase seen (fills: %s); check the wallet by hand", fills.summary())
		return
	case shares < minSize:
		result.sold = fmt.Sprintf("MANUAL: %.4f shares is below the %.4f minimum, left unsold", shares, minSize)
		return
	}
	resp, err := clob.SubmitOrder(ctx, clobclient.UserOrder{TokenID: cfg.TokenID, Side: clobclient.SideSell, Price: cfg.SellPrice, Shares: shares, OrderType: clobclient.OrderTypeFAK})
	if err != nil {
		result.sold = fmt.Sprintf("MANUAL: FAK SELL %.4f @ %.2f failed: %v", shares, cfg.SellPrice, err)
		return
	}
	result.sold = fmt.Sprintf("FAK SELL %.4f @ %.2f: status=%s making=%s taking=%s", shares, cfg.SellPrice, resp.Status, resp.MakingAmount, resp.TakingAmount)
}

func anyMatched(rounds []round) bool {
	for _, r := range rounds {
		if r.status == "matched" {
			return true
		}
	}
	return false
}

func (r *report) print() {
	firstOK := map[int]int{}
	for _, round := range r.rounds {
		for i, a := range round.attempts {
			if a.ok {
				firstOK[i+1]++
				break
			}
		}
	}
	fmt.Printf("\n== takerholdtest\nrounds: %d\nsuccessful loops (no fill): %d\nstop: %s\nsell-back: %s\n", len(r.rounds), r.successes, r.stopReason, r.sold)
	if len(firstOK) > 0 {
		keys := make([]int, 0, len(firstOK))
		for k := range firstOK {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		fmt.Println("first successful cancel attempt:")
		for _, k := range keys {
			fmt.Printf("  #%d: %d\n", k, firstOK[k])
		}
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// fillWatch listens on the account's user stream for trades in the target
// market. Any trade there counts: the wallet must have no other activity in
// it during the run.
type fillWatch struct {
	conditionID string
	mu          sync.Mutex
	orders      map[string]bool
	trades      []clobclient.Trade
}

func (w *fillWatch) track(orderID string) {
	w.mu.Lock()
	w.orders[strings.ToLower(orderID)] = true
	w.mu.Unlock()
}

func (w *fillWatch) filled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.trades) > 0
}

func (w *fillWatch) summary() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	parts := make([]string, 0, len(w.trades))
	for _, t := range w.trades {
		parts = append(parts, fmt.Sprintf("%s %s %s@%s %s", t.TraderSide, t.Side, t.Size, t.Price, t.Status))
	}
	return strings.Join(parts, "; ")
}

func (w *fillWatch) start(ctx context.Context, dialer *websocket.Dialer, creds *clobclient.Credentials) error {
	conn, _, err := dialer.DialContext(ctx, userStreamURL, nil)
	if err != nil {
		return fmt.Errorf("dial user stream: %w", err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "user", "markets": []string{w.conditionID}, "auth": map[string]string{
		"apiKey": creds.APIKey, "secret": creds.Secret, "passphrase": creds.Passphrase,
	}}); err != nil {
		conn.Close()
		return fmt.Errorf("subscribe user stream: %w", err)
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	go func() {
		pings := time.NewTicker(10 * time.Second)
		defer pings.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pings.C:
				_ = conn.WriteMessage(websocket.TextMessage, []byte("PING"))
			}
		}
	}()
	go func() {
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("user stream closed: %v (fills now rely on POST status and balance)", err)
				}
				return
			}
			w.consume(payload)
		}
	}()
	return nil
}

func (w *fillWatch) consume(payload []byte) {
	trimmed := strings.TrimSpace(string(payload))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var event struct {
		EventType string `json:"event_type"`
		ID        string `json:"id"`
		Market    string `json:"market"`
		Status    string `json:"status"`
		Type      string `json:"type"`
		Matched   string `json:"size_matched"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.EventType {
	case "order":
		w.mu.Lock()
		ours := w.orders[strings.ToLower(event.ID)]
		w.mu.Unlock()
		if ours {
			log.Printf("stream order %s type=%s status=%s matched=%s", event.ID, event.Type, event.Status, event.Matched)
		}
	case "trade":
		var trade clobclient.Trade
		if json.Unmarshal(payload, &trade) != nil || !strings.EqualFold(trade.Market, w.conditionID) {
			return
		}
		w.mu.Lock()
		w.trades = append(w.trades, trade)
		w.mu.Unlock()
		log.Printf("stream FILL %s %s %s@%s status=%s taker_order=%s", trade.TraderSide, trade.Side, trade.Size, trade.Price, trade.Status, trade.TakerOrderID)
	}
}
