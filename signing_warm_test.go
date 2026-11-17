package clobclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWarmSignerSignsWithoutNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("WarmSigner made a request: %s %s", r.Method, r.URL)
	}))
	defer server.Close()
	if err := newTestClient(t, server.URL).WarmSigner(); err != nil {
		t.Fatalf("WarmSigner: %v", err)
	}
}

func TestWarmSignerFailsWithoutKey(t *testing.T) {
	client, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.WarmSigner(); err == nil {
		t.Fatal("expected an error without a signing key")
	}
}
