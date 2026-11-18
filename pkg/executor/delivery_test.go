package executor

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

func TestRecordDelivery(t *testing.T) {
	rx := time.Unix(100, 0)
	start := rx.Add(300 * time.Microsecond)
	for _, tc := range []struct {
		name        string
		created     time.Time
		received    time.Time
		wire, queue time.Duration
	}{
		{"both", rx.Add(-700 * time.Microsecond), rx, 700 * time.Microsecond, 300 * time.Microsecond},
		{"no created_at", time.Time{}, rx, 0, 300 * time.Microsecond},
		{"created after receipt", rx.Add(time.Millisecond), rx, 0, 300 * time.Microsecond},
		{"not delivered over NATS", rx.Add(-time.Millisecond), time.Time{}, 0, 0},
	} {
		ctx := withStages(context.WithValue(context.Background(), receivedAtKey{}, start))
		recordDelivery(ctx, protocol.ExecutionOpenRequest{CreatedAt: tc.created, ReceivedAt: tc.received})
		s := stagesOf(ctx)
		if s.Wire != tc.wire || s.Queue != tc.queue {
			t.Errorf("%s: wire=%v queue=%v, want %v %v", tc.name, s.Wire, s.Queue, tc.wire, tc.queue)
		}
	}
}
