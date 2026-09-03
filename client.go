package clobclient

import (
	"crypto/ecdsa"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
	"github.com/ethereum/go-ethereum/crypto"
)

type Client struct {
	cfg       Config
	key       *ecdsa.PrivateKey
	signer    string
	transport *transport.Client
	metadata  *metadataCache
}

type metadataCache struct {
	mu       sync.RWMutex
	tickSize map[string]float64
	negRisk  map[string]bool
	feeRate  map[string]int
}

func New(cfg Config) (*Client, error) {
	if cfg.Retry.MaxAttempts < 0 {
		return nil, fmt.Errorf("retry max attempts cannot be negative")
	}
	cfg = cfg.withDefaults()
	parsed, err := url.Parse(cfg.Host)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid CLOB host %q", cfg.Host)
	}
	var key *ecdsa.PrivateKey
	var signer string
	if strings.TrimSpace(cfg.PrivateKey) != "" {
		raw := strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKey), "0x")
		key, err = crypto.HexToECDSA(raw)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		signer = crypto.PubkeyToAddress(key.PublicKey).Hex()
	}
	if cfg.MakerAddress == "" {
		cfg.MakerAddress = signer
	}
	return &Client{
		cfg: cfg, key: key, signer: signer,
		transport: transport.New(cfg.Host, cfg.HTTPClient, cfg.Timeout, cfg.Retry.MaxAttempts, cfg.Retry.BaseDelay, cfg.QPS),
		metadata:  &metadataCache{tickSize: map[string]float64{}, negRisk: map[string]bool{}, feeRate: map[string]int{}},
	}, nil
}

func (c *Client) Address() string      { return c.signer }
func (c *Client) MakerAddress() string { return c.cfg.MakerAddress }
func (c *Client) ChainID() int64       { return c.cfg.ChainID }
