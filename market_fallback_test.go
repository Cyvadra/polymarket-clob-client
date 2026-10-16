package clobclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Market reads /markets/{id}; if that undocumented route is removed it falls
// back to /clob-markets/{id}, which keeps the tokens in outcome order but no
// resolution.
func TestMarketFallsBackToCLOBMarkets(t *testing.T) {
	var gone bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/markets/") && gone:
			w.WriteHeader(http.StatusGone)
		case strings.HasPrefix(r.URL.Path, "/markets/"):
			_, _ = w.Write([]byte(`{"condition_id":"0xc","closed":true,"tokens":[{"token_id":"u","outcome":"Up","winner":true},{"token_id":"d","outcome":"Down"}]}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"c":"0xc","t":[{"t":"u","o":"Up"},{"t":"d","o":"Down"}],"fd":{"r":0.07,"e":1,"to":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	market, err := client.Market(context.Background(), "0xc")
	if err != nil || !market.Closed || len(market.Tokens) != 2 || !market.Tokens[0].Winner {
		t.Fatalf("market=%+v err=%v", market, err)
	}
	gone = true
	market, err = client.Market(context.Background(), "0xc")
	if err != nil || market.Closed || market.ConditionID != "0xc" || len(market.Tokens) != 2 ||
		market.Tokens[0].TokenID != "u" || market.Tokens[1].Outcome != "Down" || market.Tokens[0].Winner {
		t.Fatalf("fallback market=%+v err=%v", market, err)
	}
}
