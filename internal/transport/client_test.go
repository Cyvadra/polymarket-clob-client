package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetriesRetryableServerFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := New(server.URL, nil, time.Second, 2, time.Millisecond, 0)
	var response struct {
		OK bool `json:"ok"`
	}
	if err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x", Retryable: true}, &response); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !response.OK {
		t.Fatalf("calls=%d response=%+v", calls.Load(), response)
	}
}

func TestDoesNotRetryNonRetryableRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := New(server.URL, nil, time.Second, 3, time.Millisecond, 0)
	err := client.Do(context.Background(), Request{Method: http.MethodPost, Path: "/order", Retryable: false}, nil)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}

func TestNegativeAttemptsStillMakesOneRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := New(server.URL, nil, time.Second, -1, time.Millisecond, 0)
	var response struct {
		OK bool `json:"ok"`
	}
	if err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"}, &response); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !response.OK {
		t.Fatalf("calls=%d response=%+v", calls.Load(), response)
	}
}
