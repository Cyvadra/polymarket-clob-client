package natsbus

import (
	"context"
	"testing"
	"time"
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
