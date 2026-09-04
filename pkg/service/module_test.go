package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type fakeModule struct {
	name     string
	events   *[]string
	initErr  error
	runErr   error
	closeErr error
	runReady chan struct{}
	onRun    func(context.Context) error
	mu       sync.Mutex
	runs     int
}

func (m *fakeModule) Init(context.Context) error {
	*m.events = append(*m.events, "init:"+m.name)
	return m.initErr
}

func (m *fakeModule) Run(ctx context.Context) error {
	m.mu.Lock()
	m.runs++
	m.mu.Unlock()
	if m.runReady != nil {
		close(m.runReady)
	}
	if m.onRun != nil {
		return m.onRun(ctx)
	}
	if m.runErr != nil {
		return m.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *fakeModule) Close(context.Context) error {
	*m.events = append(*m.events, "close:"+m.name)
	return m.closeErr
}

func TestServiceInitAndCloseOrder(t *testing.T) {
	var events []string
	svc, err := New(
		NamedModule{Name: "one", Module: &fakeModule{name: "one", events: &events}},
		NamedModule{Name: "two", Module: &fakeModule{name: "two", events: &events}},
	)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	want := []string{"init:one", "init:two", "close:two", "close:one"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestServiceRunCancelsPeersOnError(t *testing.T) {
	var events []string
	ready := make(chan struct{})
	boom := errors.New("boom")
	blocked := &fakeModule{name: "blocked", events: &events, runReady: ready}
	failing := &fakeModule{name: "failing", events: &events, onRun: func(context.Context) error {
		<-ready
		return boom
	}}
	svc, err := New(
		NamedModule{Name: "blocked", Module: blocked},
		NamedModule{Name: "failing", Module: failing},
	)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = svc.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "run failing") || !errors.Is(err, boom) {
		t.Fatalf("expected named run error wrapping boom, got %v", err)
	}
}

func TestServiceInitClosesInitializedModulesOnFailure(t *testing.T) {
	var events []string
	boom := errors.New("boom")
	svc, err := New(
		NamedModule{Name: "one", Module: &fakeModule{name: "one", events: &events}},
		NamedModule{Name: "two", Module: &fakeModule{name: "two", events: &events, initErr: boom}},
	)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = svc.Init(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("expected init error wrapping boom, got %v", err)
	}
	want := []string{"init:one", "init:two", "close:one"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestServiceRejectsInvalidModules(t *testing.T) {
	if _, err := New(NamedModule{Name: "missing"}); err == nil {
		t.Fatal("expected nil module error")
	}
	if _, err := New(NamedModule{Module: &fakeModule{}}); err == nil {
		t.Fatal("expected missing name error")
	}
}
