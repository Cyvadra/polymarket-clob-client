package main

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

// logOrderTiming prints the network phases of one order submission. A
// reused=false line is an order that paid for a dial and TLS handshake.
func logOrderTiming(t transport.Timing) {
	status := "ok"
	if t.Err != nil {
		status = t.Err.Error()
	}
	log.Printf("order http %s %s reused=%t idle=%s getconn=%s connect=%s tls=%s ttfb=%s total=%s status=%s",
		t.Method, t.Path, t.Reused, ms(t.IdleTime), ms(t.GetConn), ms(t.Connect), ms(t.TLS), ms(t.FirstByte), ms(t.Total), status)
}

// logPrePost prints the local cost of an order before it leaves: sizing,
// locks, persistence and signing. Paired with the "order http" line it splits
// an order's latency into ours and the network's.
func logPrePost(intent protocol.ExecutionIntent, prePost time.Duration) {
	log.Printf("order pre-post %s %s %s intent=%s took=%s", intent.Kind, intent.Side, intent.Strategy, intent.IntentID, ms(prePost))
}

// warmExchangeConnections opens the order and read pools before any intent
// is subscribed, so the first signal already rides a warm connection, then
// keeps them warm in the background. interval 0 disables the loop but still
// does the initial warm-up.
func warmExchangeConnections(ctx context.Context, clob *clobclient.Client, interval time.Duration) {
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	orders, reads, err := clob.Warm(warmCtx)
	cancel()
	if err != nil {
		log.Printf("initial connection warm-up failed (will retry): %v", err)
	} else {
		log.Printf("exchange connections warm: order pool %s, read pool %s", ms(orders), ms(reads))
	}
	if interval <= 0 {
		return
	}
	summary := &pingSummary{since: time.Now()}
	go clob.KeepWarm(ctx, interval, summary.observe)
}

// keepWarmSummaryEvery is how often pingSummary logs its rollup.
const keepWarmSummaryEvery = 10 * time.Minute

// pingSummary logs keep-warm round trips as a periodic p50/max line and
// rate-limits failure logs, so the loop's output stays readable.
type pingSummary struct {
	mu        sync.Mutex
	since     time.Time
	samples   []time.Duration
	failures  int
	maxReads  time.Duration
	lastError time.Time
}

func (s *pingSummary) observe(orders, reads time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failures++
		if time.Since(s.lastError) > time.Minute {
			s.lastError = time.Now()
			log.Printf("keep-warm ping failed: %v", err)
		}
	} else {
		s.samples = append(s.samples, orders)
		if reads > s.maxReads {
			s.maxReads = reads
		}
	}
	if time.Since(s.since) < keepWarmSummaryEvery {
		return
	}
	if len(s.samples) > 0 {
		sort.Slice(s.samples, func(i, j int) bool { return s.samples[i] < s.samples[j] })
		log.Printf("keep-warm order pool rtt: n=%d p50=%s max=%s read pool max=%s failures=%d",
			len(s.samples), ms(s.samples[len(s.samples)/2]), ms(s.samples[len(s.samples)-1]), ms(s.maxReads), s.failures)
	} else if s.failures > 0 {
		log.Printf("keep-warm: no successful ping in the last %s, failures=%d", keepWarmSummaryEvery, s.failures)
	}
	s.since, s.samples, s.failures, s.maxReads = time.Now(), s.samples[:0], 0, 0
}

func ms(d time.Duration) string { return d.Round(100 * time.Microsecond).String() }
