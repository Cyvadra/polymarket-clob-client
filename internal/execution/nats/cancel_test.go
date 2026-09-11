package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// fakeCloseExecutor records the close requests it is handed. When entered and
// release are non-nil, each call announces itself on entered and then blocks
// until it receives on release, so a test can hold a call open and observe
// what the dispatcher does with concurrent lanes.
type fakeCloseExecutor struct {
	entered chan protocol.ExecutionCloseRequest
	release chan struct{}
	err     error

	mu    sync.Mutex
	calls []protocol.ExecutionCloseRequest
}

func (f *fakeCloseExecutor) ExecuteClose(_ context.Context, req protocol.ExecutionCloseRequest) error {
	if f.entered != nil {
		f.entered <- req
	}
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.err
}

func (f *fakeCloseExecutor) recorded() []protocol.ExecutionCloseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.ExecutionCloseRequest(nil), f.calls...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func closeReq(tag, mode string) protocol.ExecutionCloseRequest {
	return protocol.ExecutionCloseRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: tag, Strategy: "strategy",
		ConditionID: "condition", AssetID: "token", Outcome: "Up",
		Mode: protocol.ExecutionCloseMode(mode),
	}
}

func TestSubscribeCloseWiresTheSubject(t *testing.T) {
	subscriber := &fakeSubscriber{}
	if err := SubscribeClose(context.Background(), subscriber, &fakeCloseExecutor{}, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != protocol.SubjectStrategyExecutionClose || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
}

func TestCloseDispatcherRejectsMalformedJSON(t *testing.T) {
	d := newCloseDispatcher(context.Background(), &fakeCloseExecutor{}, nil)
	if err := d.handle(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("expected malformed JSON to be rejected synchronously")
	}
}

func TestCloseDispatcherRunsOneLaneInOrderAndKeepsOnlyLatestPending(t *testing.T) {
	exec := &fakeCloseExecutor{entered: make(chan protocol.ExecutionCloseRequest, 1), release: make(chan struct{})}
	d := newCloseDispatcher(context.Background(), exec, nil)

	d.enqueue(closeReq("lane-a", "A"))
	got := <-exec.entered // the worker is now inside ExecuteClose(A)
	if got.Mode != "A" {
		t.Fatalf("expected the first close to run first, got %q", got.Mode)
	}
	// B then C arrive while A is still running; C must supersede B.
	d.enqueue(closeReq("lane-a", "B"))
	d.enqueue(closeReq("lane-a", "C"))
	exec.release <- struct{}{} // let A finish

	got = <-exec.entered
	if got.Mode != "C" {
		t.Fatalf("expected the latest pending close to run next, got %q", got.Mode)
	}
	exec.release <- struct{}{}

	waitFor(t, d.idle)
	modes := []string{}
	for _, c := range exec.recorded() {
		modes = append(modes, string(c.Mode))
	}
	if len(modes) != 2 || modes[0] != "A" || modes[1] != "C" {
		t.Fatalf("expected [A C], got %v", modes)
	}
}

func TestCloseDispatcherRunsDifferentLanesConcurrently(t *testing.T) {
	exec := &fakeCloseExecutor{entered: make(chan protocol.ExecutionCloseRequest, 2), release: make(chan struct{})}
	d := newCloseDispatcher(context.Background(), exec, nil)

	d.enqueue(closeReq("lane-a", "A"))
	d.enqueue(closeReq("lane-b", "B"))

	// Both calls must be inside ExecuteClose at the same time; if the
	// dispatcher were serial across lanes the second read would block here
	// until the test's 2s deadline.
	seen := map[string]bool{}
	for range 2 {
		select {
		case req := <-exec.entered:
			seen[req.UniqueTag] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only one lane ran; the dispatcher serialized across lanes")
		}
	}
	if !seen["lane-a"] || !seen["lane-b"] {
		t.Fatalf("expected both lanes running, saw %v", seen)
	}
	exec.release <- struct{}{}
	exec.release <- struct{}{}
	waitFor(t, d.idle)
}

func TestCloseDispatcherReportsExecuteErrorThroughOnError(t *testing.T) {
	exec := &fakeCloseExecutor{err: fmt.Errorf("boom")}
	var mu sync.Mutex
	var got error
	d := newCloseDispatcher(context.Background(), exec, func(err error) {
		mu.Lock()
		got = err
		mu.Unlock()
	})

	payload, _ := json.Marshal(closeReq("lane-a", string(protocol.ExecutionCloseModeForce)))
	if err := d.handle(context.Background(), payload); err != nil {
		t.Fatalf("handle should not return the async error: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got != nil
	})
	mu.Lock()
	defer mu.Unlock()
	if got == nil || got.Error() != "handle NATS subject strategy.execution.close: boom" {
		t.Fatalf("unexpected reported error: %v", got)
	}
}
