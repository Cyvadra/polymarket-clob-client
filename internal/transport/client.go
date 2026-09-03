package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

type HTTPError struct {
	StatusCode   int
	Method, Path string
	Body         []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d for %s %s: %s", e.StatusCode, e.Method, e.Path, string(e.Body))
}
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

type Client struct {
	base      *url.URL
	http      *http.Client
	attempts  int
	baseDelay time.Duration
	limiter   *rate.Limiter
}

func New(host string, provided *http.Client, timeout time.Duration, attempts int, baseDelay time.Duration, qps int) *Client {
	base, _ := url.Parse(strings.TrimRight(host, "/"))
	if provided == nil {
		provided = &http.Client{Timeout: timeout}
	}
	if attempts < 1 {
		attempts = 1
	}
	var limiter *rate.Limiter
	if qps > 0 {
		limiter = rate.NewLimiter(rate.Limit(qps), qps)
	}
	return &Client{base: base, http: provided, attempts: attempts, baseDelay: baseDelay, limiter: limiter}
}

type Request struct {
	Method    string
	Path      string
	Query     url.Values
	Body      any
	Headers   func(method, requestPath string, body []byte) (http.Header, error)
	Retryable bool
}

func (c *Client) Do(ctx context.Context, spec Request, out any) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("wait for rate limit: %w", err)
		}
	}
	if spec.Method == "" || !strings.HasPrefix(spec.Path, "/") {
		return fmt.Errorf("invalid request %q %q", spec.Method, spec.Path)
	}
	var body []byte
	var err error
	if spec.Body != nil {
		body, err = json.Marshal(spec.Body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
	}
	requestURL := *c.base
	requestURL.Path = strings.TrimRight(c.base.Path, "/") + spec.Path
	requestURL.RawQuery = spec.Query.Encode()
	requestPath := requestURL.EscapedPath()
	if requestURL.RawQuery != "" {
		requestPath += "?" + requestURL.RawQuery
	}
	attempts := 1
	if spec.Retryable || defaultRetryable(spec.Method, spec.Path) {
		attempts = c.attempts
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := wait(ctx, retryDelay(c.baseDelay, attempt)); err != nil {
				return err
			}
		}
		headers := make(http.Header)
		if spec.Headers != nil {
			headers, err = spec.Headers(spec.Method, requestPath, body)
			if err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, spec.Method, requestURL.String(), bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header = headers
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			last = err
			if attempt+1 < attempts {
				continue
			}
			return fmt.Errorf("request %s %s: %w", spec.Method, requestPath, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read response: %w", readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			last = &HTTPError{StatusCode: resp.StatusCode, Method: spec.Method, Path: requestPath, Body: data}
			if api, ok := last.(*HTTPError); ok && api.Retryable() && attempt+1 < attempts {
				continue
			}
			return last
		}
		if out == nil {
			return nil
		}
		if len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return fmt.Errorf("empty response for %s %s", spec.Method, requestPath)
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response for %s %s: %w", spec.Method, requestPath, err)
		}
		return nil
	}
	return last
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func retryDelay(base time.Duration, attempt int) time.Duration {
	delay := base * time.Duration(1<<min(attempt-1, 5))
	return delay + time.Duration(rand.Int63n(int64(max(delay/4, 1))))
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func defaultRetryable(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodDelete:
		return true
	case http.MethodPost:
		return path == "/books"
	default:
		return false
	}
}
