package nats

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
)

var testIdentity = protocol.NewIdentity("0xsigner", "0xwallet", []string{"strategy"})

func testAllowlist(t *testing.T) *Allowlist {
	t.Helper()
	allowed, err := NewAllowlist([]string{"strategy"})
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	return allowed
}

func TestParseAllowlistTrimsAndDropsDuplicates(t *testing.T) {
	allowed, err := ParseAllowlist(" late-gap , momentum,,late-gap ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := allowed.Names(); !reflect.DeepEqual(got, []string{"late-gap", "momentum"}) {
		t.Fatalf("names=%v", got)
	}
	if !allowed.Allows("late-gap") || !allowed.Allows("momentum") {
		t.Fatal("expected configured strategies to be allowed")
	}
	for _, name := range []string{"", "Late-Gap", "late-gap ", "other"} {
		if allowed.Allows(name) {
			t.Fatalf("expected %q to be refused", name)
		}
	}
}

func TestParseAllowlistRequiresAStrategy(t *testing.T) {
	for _, value := range []string{"", "  ", ",", " , "} {
		if _, err := ParseAllowlist(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestNilAllowlistAllowsNothing(t *testing.T) {
	var allowed *Allowlist
	if allowed.Allows("strategy") || len(allowed.Names()) != 0 {
		t.Fatal("expected a nil allowlist to allow nothing")
	}
}

func TestSubscribersRequireAnAllowlist(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := SubscribeOpen(&fakeSubscriber{}, execution, nil); err == nil {
		t.Fatal("expected open subscription without an allowlist to fail")
	}
	if err := SubscribeClose(context.Background(), &fakeSubscriber{}, &fakeCloseExecutor{}, nil, nil); err == nil {
		t.Fatal("expected close subscription without an allowlist to fail")
	}
}

// An open for another wallet's strategy must leave no trace: a failure result
// would read to the strategy as its own wallet refusing the order.
func TestSubscribeOpenDropsAnotherWalletsStrategySilently(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedPublisher{}
	execution.SetEventPublisher(publisher)
	subscriber := &fakeSubscriber{}
	if err := SubscribeOpen(subscriber, execution, testAllowlist(t)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// The style is unsupported, so an open that reached the executor would
	// publish a failure result.
	intent := protocol.ExecutionOpenRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "other", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: "UNSUPPORTED"}}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("expected a silent drop, got %v", err)
	}
	if publisher.value != nil {
		t.Fatalf("expected nothing published, got %+v on %q", publisher.value, publisher.subject)
	}
}

func TestCloseDispatcherDropsAnotherWalletsStrategySilently(t *testing.T) {
	exec := &fakeCloseExecutor{}
	d := newCloseDispatcher(context.Background(), exec, testAllowlist(t), func(err error) {
		t.Errorf("unexpected error: %v", err)
	})
	request := closeReq("lane-a", string(protocol.ExecutionCloseModeForce))
	request.Strategy = "other"
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := d.handle(context.Background(), payload); err != nil {
		t.Fatalf("expected a silent drop, got %v", err)
	}
	if !d.idle() {
		t.Fatal("expected no lane worker for a dropped close")
	}
	if calls := exec.recorded(); len(calls) != 0 {
		t.Fatalf("expected the executor untouched, got %+v", calls)
	}
}

func TestBalanceQueryReplyNamesTheWallet(t *testing.T) {
	connector := &fakeBalanceConnector{}
	if err := SubscribeBalanceQuery(connector, fakeEquity{snapshot: equity.Snapshot{CashUSD: 1}}, testIdentity); err != nil {
		t.Fatal(err)
	}
	if err := deliverBalanceQuery(t, connector, protocol.SchemaVersionV1); err != nil {
		t.Fatal(err)
	}
	assertIdentity(t, connector.response.WalletAddress, connector.response.SignerAddress, connector.response.AllowedStrategies)

	failing := &fakeBalanceConnector{}
	if err := SubscribeBalanceQuery(failing, fakeEquity{err: errors.New("exchange down")}, testIdentity); err != nil {
		t.Fatal(err)
	}
	if err := deliverBalanceQuery(t, failing, protocol.SchemaVersionV1); err == nil || failing.response.Error == "" {
		t.Fatalf("err=%v response=%+v", err, failing.response)
	}
	assertIdentity(t, failing.response.WalletAddress, failing.response.SignerAddress, failing.response.AllowedStrategies)
}

func TestPositionQueryReplyNamesTheWallet(t *testing.T) {
	connector := &fakeReplyConnector{}
	if err := SubscribePositionQuery(connector, fakePositionStore{records: queryPositions()}, testIdentity, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := deliverPositionQuery(t, connector, protocol.PositionQueryRequest{SchemaVersion: protocol.SchemaVersionV1}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	response := connector.response
	assertIdentity(t, response.WalletAddress, response.SignerAddress, response.AllowedStrategies)
	if len(response.Positions) != 3 {
		t.Fatalf("expected three positions, got %d", len(response.Positions))
	}
	for _, position := range response.Positions {
		if position.WalletAddress != "0xwallet" || position.SignerAddress != "0xsigner" {
			t.Fatalf("position without wallet: %+v", position)
		}
	}

	if err := deliverPositionQuery(t, connector, protocol.PositionQueryRequest{}); err == nil || connector.response.Error == "" {
		t.Fatalf("err=%v response=%+v", err, connector.response)
	}
	assertIdentity(t, connector.response.WalletAddress, connector.response.SignerAddress, connector.response.AllowedStrategies)
}

func assertIdentity(t *testing.T, wallet, signer string, strategies []string) {
	t.Helper()
	if wallet != "0xwallet" || signer != "0xsigner" || !reflect.DeepEqual(strategies, []string{"strategy"}) {
		t.Fatalf("wallet=%q signer=%q strategies=%v", wallet, signer, strategies)
	}
}
