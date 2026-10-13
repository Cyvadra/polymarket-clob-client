package transport

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// NewHTTPClient builds the client the REST transport uses, optionally through
// an outbound proxy. It is tuned for a long-lived trading process: HTTP/2 is
// forced on (a custom DialContext otherwise silently disables it), the idle
// pool is deep enough that a background read never evicts the connection an
// order needs, and TCP keepalives are short enough to notice a dead path.
//
// Each call returns a client with its own connection pool, so callers that
// want traffic isolated (orders vs. background reads) build one per purpose.
func NewHTTPClient(proxy *url.URL, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 0,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// Timing breaks one traced request into its network phases. Phases that did
// not happen (connect and TLS on a reused connection) are zero.
type Timing struct {
	Method, Path string
	// Reused reports whether the request rode an already-open connection;
	// a false here on an order is the cold-path cost this trace exists to find.
	Reused   bool
	WasIdle  bool
	IdleTime time.Duration
	// GetConn is the wait for a connection, including any dial and handshake.
	GetConn   time.Duration
	Connect   time.Duration
	TLS       time.Duration
	FirstByte time.Duration
	Total     time.Duration
	Err       error
}

// tracer collects one request's timings. httptrace callbacks can fire from
// other goroutines (dial races, HTTP/2), so fields are guarded.
type tracer struct {
	mu                            sync.Mutex
	start, connectStart, tlsStart time.Time
	timing                        Timing
}

func newTracer(method, path string) *tracer {
	return &tracer{start: time.Now(), timing: Timing{Method: method, Path: path}}
}

func (t *tracer) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.timing.GetConn = time.Since(t.start)
			t.timing.Reused, t.timing.WasIdle, t.timing.IdleTime = info.Reused, info.WasIdle, info.IdleTime
		},
		ConnectStart: func(string, string) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.connectStart.IsZero() {
				t.connectStart = time.Now()
			}
		},
		ConnectDone: func(string, string, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.connectStart.IsZero() && t.timing.Connect == 0 {
				t.timing.Connect = time.Since(t.connectStart)
			}
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.tlsStart.IsZero() {
				t.timing.TLS = time.Since(t.tlsStart)
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.timing.FirstByte = time.Since(t.start)
		},
	}
}

func (t *tracer) finish(err error) Timing {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timing.Total = time.Since(t.start)
	t.timing.Err = err
	return t.timing
}
