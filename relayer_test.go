package clobclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRelayerSubmitAndWait drives ExecuteWalletBatch and WaitRelayerTransaction
// against a fake relayer, checking paths, auth headers, the nonce and states.
func TestRelayerSubmitAndWait(t *testing.T) {
	const key = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	const wallet = "0x3333333333333333333333333333333333333333"
	var polls atomic.Int32
	var submitted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("RELAYER_API_KEY") != "rk" || r.Header.Get("RELAYER_API_KEY_ADDRESS") == "" {
			t.Errorf("%s %s: missing relayer auth headers", r.Method, r.URL.Path)
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/account/transactions/params":
			if r.URL.Query().Get("type") != "WALLET" || r.URL.Query().Get("address") == "" {
				t.Errorf("nonce query %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"address":"0x1","nonce":"7"}`))
		case r.Method == "POST" && r.URL.Path == "/submit":
			json.NewDecoder(r.Body).Decode(&submitted)
			w.Write([]byte(`{"transactionID":"tx-1","state":"STATE_NEW"}`))
		case r.Method == "GET" && r.URL.Path == "/v1/account/transactions/tx-1":
			if polls.Add(1) < 2 {
				w.Write([]byte(`[{"transactionID":"tx-1","state":"STATE_MINED"}]`))
				return
			}
			w.Write([]byte(`{"transactionID":"tx-1","state":"STATE_CONFIRMED","transactionHash":"0xabc"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(Config{PrivateKey: key, MakerAddress: wallet, SignatureType: SignatureTypePoly1271,
		Relayer: &RelayerCredentials{APIKey: "rk"}, RelayerHost: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	calls := []WalletCall{{Target: PUSDAddress, Value: "0", Data: "0x1234"}}
	ctx := context.Background()
	tx, err := c.ExecuteWalletBatch(ctx, calls, "test", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if tx.ID != "tx-1" || submitted["nonce"] != "7" || submitted["type"] != "WALLET" || !strings.EqualFold(submitted["from"].(string), c.Address()) {
		t.Fatalf("tx %+v, submitted %v", tx, submitted)
	}
	got, err := c.WaitRelayerTransaction(ctx, tx.ID, time.Millisecond)
	if err != nil || got.State != "STATE_CONFIRMED" || got.Hash != "0xabc" {
		t.Fatalf("wait: %+v, %v", got, err)
	}
}

func TestRelayerHostCheckedOnUse(t *testing.T) {
	c, err := New(Config{RelayerHost: "not a url", Relayer: &RelayerCredentials{APIKey: "rk", Address: "0x1"}})
	if err != nil {
		t.Fatalf("New must not reject an unused relayer host: %v", err)
	}
	if _, err := c.RelayerTransactionStatus(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "invalid relayer host") {
		t.Fatalf("got %v", err)
	}
}

func TestRelayerSubmitOutcome(t *testing.T) {
	const key = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	for _, tc := range []struct {
		status  int
		unknown bool
	}{{http.StatusBadRequest, false}, {http.StatusUnauthorized, false}, {http.StatusRequestTimeout, true}, {http.StatusBadGateway, true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/submit" {
				http.Error(w, `{"error":"no"}`, tc.status)
				return
			}
			w.Write([]byte(`{"address":"0x1","nonce":"7"}`))
		}))
		c, err := New(Config{PrivateKey: key, MakerAddress: "0x3333333333333333333333333333333333333333", SignatureType: SignatureTypePoly1271,
			Relayer: &RelayerCredentials{APIKey: "rk"}, RelayerHost: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.ExecuteWalletBatch(context.Background(), []WalletCall{{Target: PUSDAddress, Value: "0", Data: "0x1234"}}, "test", time.Now().Add(time.Minute))
		srv.Close()
		if err == nil || errors.Is(err, ErrSubmitOutcomeUnknown) != tc.unknown {
			t.Errorf("HTTP %d: want unknown=%v, got %v", tc.status, tc.unknown, err)
		}
	}
}
