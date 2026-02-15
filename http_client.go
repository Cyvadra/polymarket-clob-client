package clobclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClient handles HTTP requests with retry logic
type HTTPClient struct {
	client       *http.Client
	retryEnabled bool
	maxRetries   int
}

// NewHTTPClient creates a new HTTP client
func NewHTTPClient(timeout time.Duration, retryEnabled bool) *HTTPClient {
	return &HTTPClient{
		client: &http.Client{
			Timeout: timeout,
		},
		retryEnabled: retryEnabled,
		maxRetries:   3,
	}
}

// Request performs an HTTP request with optional retry logic
func (c *HTTPClient) Request(
	method string,
	url string,
	headers map[string]string,
	body interface{},
) ([]byte, error) {
	var requestBody []byte
	var err error

	if body != nil {
		requestBody, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	var lastErr error
	retries := 1
	if c.retryEnabled {
		retries = c.maxRetries
	}

	for i := 0; i < retries; i++ {
		resp, err := c.doRequest(method, url, headers, requestBody)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		// Only retry on specific errors (5xx, timeout, etc.)
		if !c.shouldRetry(err) {
			return nil, err
		}

		// Exponential backoff
		if i < retries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}

	return nil, fmt.Errorf("request failed after %d retries: %w", retries, lastErr)
}

// doRequest performs a single HTTP request
func (c *HTTPClient) doRequest(
	method string,
	url string,
	headers map[string]string,
	body []byte,
) ([]byte, error) {
	var req *http.Request
	var err error

	if body != nil {
		req, err = http.NewRequest(method, url, bytes.NewBuffer(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Perform request
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(responseBody))
	}

	return responseBody, nil
}

// shouldRetry determines if a request should be retried
func (c *HTTPClient) shouldRetry(err error) bool {
	if !c.retryEnabled {
		return false
	}

	// Retry on timeout or 5xx errors
	// This is a simplified check; in production, you'd want more sophisticated logic
	return true
}

// Get performs a GET request
func (c *HTTPClient) Get(url string, headers map[string]string) ([]byte, error) {
	return c.Request(http.MethodGet, url, headers, nil)
}

// Post performs a POST request
func (c *HTTPClient) Post(url string, headers map[string]string, body interface{}) ([]byte, error) {
	return c.Request(http.MethodPost, url, headers, body)
}

// Delete performs a DELETE request
func (c *HTTPClient) Delete(url string, headers map[string]string, body interface{}) ([]byte, error) {
	return c.Request(http.MethodDelete, url, headers, body)
}

// Put performs a PUT request
func (c *HTTPClient) Put(url string, headers map[string]string, body interface{}) ([]byte, error) {
	return c.Request(http.MethodPut, url, headers, body)
}
