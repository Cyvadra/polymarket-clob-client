package natsbus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestDecodeJSON(t *testing.T) {
	value, err := DecodeJSON[struct {
		ID string `json:"id"`
	}]([]byte(`{"id":"event-1"}`))
	if err != nil || value.ID != "event-1" {
		t.Fatalf("decode JSON: value=%+v err=%v", value, err)
	}
}

func TestNewRequiresURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected missing URL to be rejected")
	}
}

// Close used to hold the bus mutex while draining, so a handler publishing an
// acknowledgement during shutdown deadlocked against its own drain.
func TestCloseDoesNotBlockAConcurrentPublish(t *testing.T) {
	bus, err := New(Config{URL: "nats://127.0.0.1:4222"})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	publishing := make(chan error, 1)
	go func() { publishing <- bus.PublishJSON("subject", map[string]string{"a": "b"}) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("close bus: %v", err)
	}
	select {
	case <-publishing:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on shutdown")
	}
}

func TestPublishAfterCloseReportsUninitialized(t *testing.T) {
	bus, err := New(Config{URL: "nats://127.0.0.1:4222"})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close bus: %v", err)
	}
	if err := bus.PublishJSON("subject", map[string]string{}); err == nil {
		t.Fatal("expected publishing on a closed bus to fail")
	}
}

// applied builds the client options and returns them resolved, so the
// lifecycle handlers can be exercised without a live server.
func applied(t *testing.T, cfg Config) *nats.Options {
	t.Helper()
	bus, err := New(cfg)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	var options nats.Options
	for _, option := range bus.connectOptions() {
		if err := option(&options); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	return &options
}

// A disconnect used to be reported while the matching reconnect was silent,
// which left a log in which a daemon that had resubscribed and one that had
// silently stopped receiving looked identical.
func TestConnectionLifecycleReportsBothDisconnectAndReconnect(t *testing.T) {
	var reported []string
	options := applied(t, Config{
		URL:            "nats://127.0.0.1:4222",
		OnHandlerError: func(err error) { reported = append(reported, err.Error()) },
	})
	if options.DisconnectedErrCB == nil {
		t.Fatal("no disconnect handler registered")
	}
	if options.ReconnectedCB == nil {
		t.Fatal("no reconnect handler registered")
	}
	options.DisconnectedErrCB(nil, errors.New("EOF"))
	options.DisconnectedErrCB(nil, nil)
	options.ReconnectedCB(nil)

	if len(reported) != 3 {
		t.Fatalf("expected a report for each event, got %d: %v", len(reported), reported)
	}
	if !strings.Contains(reported[0], "NATS disconnected: EOF") {
		t.Fatalf("unexpected disconnect report: %q", reported[0])
	}
	// A clean disconnect carries no error, and used to go unreported.
	if reported[1] != "NATS disconnected" {
		t.Fatalf("unexpected clean disconnect report: %q", reported[1])
	}
	if !strings.Contains(reported[2], "NATS reconnected") || !strings.Contains(reported[2], "subscriptions re-established") {
		t.Fatalf("unexpected reconnect report: %q", reported[2])
	}
}

// A bus with no error handler must not panic when the connection drops.
func TestConnectionLifecycleToleratesNoErrorHandler(t *testing.T) {
	options := applied(t, Config{URL: "nats://127.0.0.1:4222"})
	options.DisconnectedErrCB(nil, errors.New("EOF"))
	options.DisconnectedErrCB(nil, nil)
	options.ReconnectedCB(nil)
}
