package clobclient

import (
	"net/http"
	"net/url"
	"time"
)

const DefaultHost = "https://clob.polymarket.com"

type Config struct {
	Host          string
	ChainID       int64
	RPCEndpoint   string
	PrivateKey    string
	Credentials   *Credentials
	Builder       *BuilderCredentials
	MakerAddress  string
	SignatureType SignatureType
	HTTPClient    *http.Client
	// ProxyURL is the outbound proxy the REST transport uses. It is kept
	// separately so the websocket account stream can reach the exchange
	// through the same egress instead of falling back to the process
	// environment.
	ProxyURL *url.URL
	Timeout  time.Duration
	QPS      int
	Retry    RetryConfig
	Now      func() time.Time
}

type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
}

func (c Config) withDefaults() Config {
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.ChainID == 0 {
		c.ChainID = ChainPolygonMainnet
	}
	if c.RPCEndpoint == "" && c.ChainID == ChainPolygonMainnet {
		c.RPCEndpoint = "https://polygon-rpc.com"
	}
	if c.Timeout <= 0 {
		c.Timeout = 20 * time.Second
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 3
	}
	if c.Retry.BaseDelay <= 0 {
		c.Retry.BaseDelay = 250 * time.Millisecond
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
