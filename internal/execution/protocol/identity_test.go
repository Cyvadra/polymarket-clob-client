package protocol

import (
	"reflect"
	"testing"
)

type capturedPublisher struct {
	subject string
	value   any
}

func (p *capturedPublisher) PublishJSON(subject string, value any) error {
	p.subject, p.value = subject, value
	return nil
}

func TestNewIdentityFallsBackToTheSignerWithoutAProxyWallet(t *testing.T) {
	if got := NewIdentity("0xsigner", "", nil); got.WalletAddress != "0xsigner" || got.SignerAddress != "0xsigner" {
		t.Fatalf("identity=%+v", got)
	}
	if got := NewIdentity("0xsigner", "0xproxy", nil); got.WalletAddress != "0xproxy" || got.SignerAddress != "0xsigner" {
		t.Fatalf("identity=%+v", got)
	}
	if got := NewIdentity("0xsigner", "", nil).Strategies(); got == nil {
		t.Fatal("expected an empty list, not nil, so replies carry a JSON array")
	}
}

func TestWithIdentityStampsExecutionMessages(t *testing.T) {
	identity := NewIdentity("0xsigner", "0xwallet", []string{"late-gap"})
	publisher := &capturedPublisher{}
	stamped := WithIdentity(publisher, identity)

	messages := []any{
		ExecutionOpenResult{UniqueTag: "lane", Strategy: "late-gap"},
		ExecutionCloseResult{UniqueTag: "lane", Strategy: "late-gap"},
		ExecutionOrderEvent{IntentID: "intent", Strategy: "late-gap"},
		PositionFeature{UniqueTag: "lane"},
		PositionQueryResponse{Positions: []PositionFeature{{UniqueTag: "lane"}}},
		BalanceQueryResponse{CashUSD: 1},
	}
	for _, message := range messages {
		if err := stamped.PublishJSON("subject", message); err != nil {
			t.Fatalf("publish %T: %v", message, err)
		}
		if reflect.TypeOf(publisher.value) != reflect.TypeOf(message) {
			t.Fatalf("published %T for %T", publisher.value, message)
		}
		value := reflect.ValueOf(publisher.value)
		if wallet, signer := value.FieldByName("WalletAddress").String(), value.FieldByName("SignerAddress").String(); wallet != "0xwallet" || signer != "0xsigner" {
			t.Fatalf("%T wallet=%q signer=%q", message, wallet, signer)
		}
	}
	response := publisher.value.(BalanceQueryResponse)
	if !reflect.DeepEqual(response.AllowedStrategies, []string{"late-gap"}) || response.CashUSD != 1 {
		t.Fatalf("response=%+v", response)
	}
}

func TestWithIdentityKeepsStrategyAndStampsNestedPositions(t *testing.T) {
	publisher := &capturedPublisher{}
	stamped := WithIdentity(publisher, NewIdentity("0xsigner", "0xwallet", []string{"late-gap"}))

	original := PositionQueryResponse{Positions: []PositionFeature{{UniqueTag: "lane"}}}
	if err := stamped.PublishJSON("subject", original); err != nil {
		t.Fatal(err)
	}
	response := publisher.value.(PositionQueryResponse)
	if len(response.Positions) != 1 || response.Positions[0].WalletAddress != "0xwallet" || response.Positions[0].UniqueTag != "lane" {
		t.Fatalf("response=%+v", response)
	}
	if original.Positions[0].WalletAddress != "" {
		t.Fatal("expected the caller's positions to be left untouched")
	}

	if err := stamped.PublishJSON("subject", ExecutionOpenResult{Strategy: "late-gap"}); err != nil {
		t.Fatal(err)
	}
	if result := publisher.value.(ExecutionOpenResult); result.Strategy != "late-gap" {
		t.Fatalf("result=%+v", result)
	}
}

func TestWithIdentityPassesOtherPayloadsThrough(t *testing.T) {
	publisher := &capturedPublisher{}
	stamped := WithIdentity(publisher, NewIdentity("0xsigner", "0xwallet", nil))
	payload := map[string]string{"k": "v"}
	if err := stamped.PublishJSON("other.subject", payload); err != nil {
		t.Fatal(err)
	}
	if publisher.subject != "other.subject" || !reflect.DeepEqual(publisher.value, payload) {
		t.Fatalf("subject=%q value=%+v", publisher.subject, publisher.value)
	}
	if WithIdentity(nil, Identity{}) != nil {
		t.Fatal("expected no publisher to stay no publisher")
	}
}
