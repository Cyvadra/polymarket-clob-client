package clobclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHeartbeatChainsIDs(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/heartbeats" || r.Header.Get("POLY_API_KEY") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body["heartbeat_id"])
		_, _ = w.Write([]byte(`{"heartbeat_id":"hb-` + string(rune('1'+len(got)-1)) + `"}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	first, err := client.Heartbeat(context.Background(), "")
	if err != nil || first != "hb-1" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := client.Heartbeat(context.Background(), first)
	if err != nil || second != "hb-2" || len(got) != 2 || got[0] != "" || got[1] != "hb-1" {
		t.Fatalf("second=%q err=%v sent=%v", second, err, got)
	}
}

func TestOrderResponseDecodesTradeIDs(t *testing.T) {
	var out OrderResponse
	raw := `{"success":true,"orderID":"0xabc","status":"matched","makingAmount":"100000000","takingAmount":"200000000","tradeIDs":["trade-123"],"errorMsg":""}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.TradeIDs) != 1 || out.TradeIDs[0] != "trade-123" || len(out.TransactionsHashes) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
