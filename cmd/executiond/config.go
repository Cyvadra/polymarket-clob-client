package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"gopkg.in/yaml.v3"
)

const defaultConfigPath = "/etc/executiond/config.yaml"

type yamlConfig struct {
	PrivateKey    string              `yaml:"private_key"`
	DepositWallet string              `yaml:"deposit_wallet"`
	Credentials   *yamlCredentials    `yaml:"credentials"`
	Polygon       yamlPolygon         `yaml:"polygon"`
	Polymarket    yamlPolymarket      `yaml:"polymarket"`
	Executiond    yamlExecutionConfig `yaml:"executiond"`
}

type yamlCredentials struct {
	APIKey    string `yaml:"api_key"`
	Secret    string `yaml:"secret"`
	Passphrase string `yaml:"passphrase"`
}

type yamlPolygon struct {
	ChainID     int64  `yaml:"chain_id"`
	RPCEndpoint string `yaml:"rpc_endpoint"`
}

type yamlPolymarket struct {
	Rest struct {
		BaseURL           string   `yaml:"base_url"`
		QPSLimit          int      `yaml:"qps_limit"`
		TimeoutSeconds    int      `yaml:"timeout_seconds"`
		MaxRetries        int      `yaml:"max_retries"`
		RetryDelaySeconds float64  `yaml:"retry_delay_seconds"`
		ProxyURLs         []string `yaml:"proxy_urls"`
	} `yaml:"rest"`
	MakerAddress  string `yaml:"maker_address"`
	SignatureType int    `yaml:"signature_type"`
}

type yamlExecutionConfig struct {
	NATSURL                 string `yaml:"nats_url"`
	PostgresURL             string `yaml:"postgres_url"`
	MaxOpenBuyNotionalUSD   string `yaml:"max_open_buy_notional_usd"`
	PositionFeatureInterval string `yaml:"position_feature_interval"`
	ReconcileInterval       string `yaml:"reconcile_interval"`
	MissingOrderGracePeriod string `yaml:"missing_order_grace_period"`
	ReconcileMaxTradeAge    string `yaml:"reconcile_max_trade_age"`
	ConnectTimeout          string `yaml:"connect_timeout"`
	ShutdownGracePeriod     string `yaml:"shutdown_grace_period"`
}

func loadConfig(path string) (config, clobclient.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, clobclient.Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var file yamlConfig
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return config{}, clobclient.Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	daemon, err := daemonConfigFromYAML(file.Executiond)
	if err != nil {
		return config{}, clobclient.Config{}, err
	}
	clob, err := clobConfigFromYAML(file)
	if err != nil {
		return config{}, clobclient.Config{}, err
	}
	return daemon, clob, nil
}

func daemonConfigFromYAML(raw yamlExecutionConfig) (config, error) {
	cfg := config{
		NATSURL:               valueOr(raw.NATSURL, "nats://127.0.0.1:4222"),
		PostgresURL:           strings.TrimSpace(raw.PostgresURL),
		MaxOpenBuyNotionalUSD: strings.TrimSpace(raw.MaxOpenBuyNotionalUSD),
		FeatureInterval:       parseDuration(raw.PositionFeatureInterval, 500*time.Millisecond),
		ReconcileInterval:     parseDuration(raw.ReconcileInterval, 30*time.Second),
		MissingOrderGrace:     parseDuration(raw.MissingOrderGracePeriod, 2*time.Minute),
		MaxTradeAge:           parseDuration(raw.ReconcileMaxTradeAge, 24*time.Hour),
		ConnectTimeout:        parseDuration(raw.ConnectTimeout, 10*time.Second),
		ShutdownGracePeriod:   parseDuration(raw.ShutdownGracePeriod, 10*time.Second),
	}
	if cfg.PostgresURL == "" {
		return config{}, fmt.Errorf("executiond.postgres_url is required")
	}
	if cfg.MaxOpenBuyNotionalUSD != "" && !decimal.Positive(cfg.MaxOpenBuyNotionalUSD) {
		return config{}, fmt.Errorf("executiond.max_open_buy_notional_usd must be a positive decimal")
	}
	if cfg.FeatureInterval <= 0 || cfg.ReconcileInterval <= 0 || cfg.MissingOrderGrace <= 0 || cfg.ConnectTimeout <= 0 || cfg.ShutdownGracePeriod <= 0 {
		return config{}, fmt.Errorf("executiond durations must be positive")
	}
	return cfg, nil
}

func clobConfigFromYAML(file yamlConfig) (clobclient.Config, error) {
	privateKey := strings.TrimSpace(file.PrivateKey)
	if privateKey == "" {
		return clobclient.Config{}, fmt.Errorf("private_key is required")
	}
	cfg := clobclient.Config{
		Host:         file.Polymarket.Rest.BaseURL,
		ChainID:      file.Polygon.ChainID,
		RPCEndpoint:  file.Polygon.RPCEndpoint,
		PrivateKey:   privateKey,
		MakerAddress: valueOr(file.Polymarket.MakerAddress, file.DepositWallet),
	}
	if file.Polymarket.SignatureType != 0 {
		if file.Polymarket.SignatureType < int(clobclient.SignatureTypeEOA) || file.Polymarket.SignatureType > int(clobclient.SignatureTypePoly1271) {
			return clobclient.Config{}, fmt.Errorf("polymarket.signature_type must be an integer from 0 through 3")
		}
		cfg.SignatureType = clobclient.SignatureType(file.Polymarket.SignatureType)
	}
	if file.Credentials != nil {
		values := []string{
			strings.TrimSpace(file.Credentials.APIKey),
			strings.TrimSpace(file.Credentials.Secret),
			strings.TrimSpace(file.Credentials.Passphrase),
		}
		set := 0
		for _, value := range values {
			if value != "" {
				set++
			}
		}
		if set != 0 && set != len(values) {
			return clobclient.Config{}, fmt.Errorf("credentials.api_key, credentials.secret, and credentials.passphrase must be set together")
		}
		if set == len(values) {
			cfg.Credentials = &clobclient.Credentials{APIKey: values[0], Secret: values[1], Passphrase: values[2]}
		}
	}
	rest := file.Polymarket.Rest
	cfg.QPS = rest.QPSLimit
	cfg.Timeout = secondsDuration(rest.TimeoutSeconds)
	cfg.Retry.MaxAttempts = rest.MaxRetries
	cfg.Retry.BaseDelay = secondsDurationFloat(rest.RetryDelaySeconds)
	if len(rest.ProxyURLs) > 0 && strings.TrimSpace(rest.ProxyURLs[0]) != "" {
		proxyURL, err := url.Parse(strings.TrimSpace(rest.ProxyURLs[0]))
		if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
			return clobclient.Config{}, fmt.Errorf("polymarket.rest.proxy_urls[0] must be an absolute URL")
		}
		cfg.ProxyURL = proxyURL
		cfg.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: cfg.Timeout}
	}
	return cfg, nil
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func parseDuration(value string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(strings.TrimSpace(value)); err == nil {
		return parsed
	}
	return -1
}

func secondsDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func secondsDurationFloat(seconds float64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
