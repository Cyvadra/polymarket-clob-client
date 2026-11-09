package nats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

type recordingQuotes struct {
	got []protocol.ExecutionQuoteRequest
	err error
}

func (r *recordingQuotes) ApplyQuote(_ context.Context, req protocol.ExecutionQuoteRequest) error {
	r.got = append(r.got, req)
	return r.err
}

func TestSubscribeQuoteAppliesAllowedStrategiesOnly(t *testing.T) {
	bus := &fakeSubscriber{}
	quotes := &recordingQuotes{}
	allowed, err := ParseAllowlist("QuoteVUp")
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	if err := SubscribeQuote(context.Background(), bus, quotes, allowed, func(err error) { reported = append(reported, err) }); err != nil {
		t.Fatal(err)
	}
	if bus.subject != protocol.SubjectStrategyExecutionQuote {
		t.Fatalf("subscribed to %q", bus.subject)
	}
	send := func(strategy string) {
		payload, _ := json.Marshal(protocol.ExecutionQuoteRequest{SchemaVersion: protocol.SchemaVersionV1, Strategy: strategy, Seq: 1, Levels: []string{"0.52"}})
		if err := bus.handler(context.Background(), payload); err != nil {
			t.Fatal(err)
		}
	}
	send("QuoteVUp")
	send("OtherStrategy")
	if len(quotes.got) != 1 || quotes.got[0].Strategy != "QuoteVUp" || quotes.got[0].Levels[0] != "0.52" {
		t.Fatalf("applied=%+v", quotes.got)
	}
	quotes.err = errors.New("invalid")
	send("QuoteVUp")
	if len(reported) != 1 {
		t.Fatalf("reported=%v", reported)
	}
}
