package execution

import (
	"context"
	"strings"
	"testing"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type readerStub struct{ order *clobclient.Order }

func (s readerStub) Order(context.Context, string) (*clobclient.Order, error) { return s.order, nil }

func TestResolverNormalizesCanceledTerminalOrder(t *testing.T) {
	resolver := Resolver{Client: readerStub{order: &clobclient.Order{Status: "cancelled", SizeMatched: "1.25", AvgPrice: "0.42"}}}
	exec, err := resolver.Resolve(context.Background(), "order-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !exec.Terminal || exec.Status != clobclient.ExecutionCanceled || exec.MatchedShares != 1.25 || exec.AveragePrice != .42 {
		t.Fatalf("unexpected execution %+v", exec)
	}
}

func TestResolverRejectsUnknownStatus(t *testing.T) {
	resolver := Resolver{Client: readerStub{order: &clobclient.Order{Status: "PENDING_REVIEW"}}}
	exec, err := resolver.Resolve(context.Background(), "order-1", 2)
	if err == nil || !strings.Contains(err.Error(), "PENDING_REVIEW") {
		t.Fatalf("expected unsupported-status error, got execution=%+v err=%v", exec, err)
	}
	if exec.Terminal {
		t.Fatalf("unknown status must not be terminal: %+v", exec)
	}
}

func TestResolverTreatsExpiredAsTerminal(t *testing.T) {
	resolver := Resolver{Client: readerStub{order: &clobclient.Order{Status: "EXPIRED"}}}
	exec, err := resolver.Resolve(context.Background(), "order-1", 2)
	if err != nil || !exec.Terminal || exec.Status != clobclient.ExecutionCanceled {
		t.Fatalf("unexpected expired execution=%+v err=%v", exec, err)
	}
}
