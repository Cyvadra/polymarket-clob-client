package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestNewHTTPClientForcesHTTP2ThroughProxy(t *testing.T) {
	proxy, _ := url.Parse("http://localhost:7890")
	client := NewHTTPClient(proxy, time.Second)
	transport := client.Transport.(*http.Transport)
	if !transport.ForceAttemptHTTP2 || transport.MaxIdleConnsPerHost < 2 || transport.IdleConnTimeout == 0 {
		t.Fatalf("transport not tuned: %+v", transport)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://clob.polymarket.com/time", nil)
	got, err := transport.Proxy(request)
	if err != nil || got == nil || got.Host != "localhost:7890" {
		t.Fatalf("proxy = %v, %v", got, err)
	}
}

func TestTraceReportsConnectionReuse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := New(server.URL, server.Client(), time.Second, 1, time.Millisecond, 0)
	var timings []Timing
	client.SetTraceHook(func(timing Timing) { timings = append(timings, timing) })
	for i := 0; i < 2; i++ {
		if err := client.Do(context.Background(), Request{Method: http.MethodPost, Path: "/order", Trace: true}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/untraced"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(timings) != 2 {
		t.Fatalf("timings = %d, want 2 (untraced request must not report)", len(timings))
	}
	if timings[0].Reused || timings[0].TLS == 0 {
		t.Fatalf("first request should dial and handshake: %+v", timings[0])
	}
	if !timings[1].Reused || timings[1].TLS != 0 || timings[1].Total <= 0 {
		t.Fatalf("second request should reuse the warm connection: %+v", timings[1])
	}
}
