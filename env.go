package clobclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/keyfile"
)

// ConfigFromEnv loads client configuration from POLYMARKET_* environment
// variables. L2 credentials are optional because they can be derived from the
// private key with CreateOrDeriveCredentials.
//
// The signing key comes from either POLYMARKET_PRIVATE_KEY (plaintext hex,
// intended for development) or POLYMARKET_PRIVATE_KEY_FILE (a keystore v3
// file decrypted at startup, intended for deployments). See privateKeyFromEnv.
func ConfigFromEnv() (Config, error) {
	privateKey, err := privateKeyFromEnv()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Host:         os.Getenv("POLYMARKET_HOST"),
		RPCEndpoint:  os.Getenv("POLYMARKET_RPC_ENDPOINT"),
		PrivateKey:   privateKey,
		MakerAddress: os.Getenv("POLYMARKET_MAKER_ADDRESS"),
	}
	if cfg.ChainID, err = envInt64("POLYMARKET_CHAIN_ID"); err != nil {
		return Config{}, err
	}
	if raw := os.Getenv("POLYMARKET_SIGNATURE_TYPE"); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < int(SignatureTypeEOA) || value > int(SignatureTypePoly1271) {
			return Config{}, fmt.Errorf("POLYMARKET_SIGNATURE_TYPE must be an integer from 0 through 3")
		}
		cfg.SignatureType = SignatureType(value)
	}
	credentials, err := credentialsFromEnv("POLYMARKET_API_KEY", "POLYMARKET_API_SECRET", "POLYMARKET_API_PASSPHRASE")
	if err != nil {
		return Config{}, err
	}
	cfg.Credentials = credentials
	builder, err := credentialsFromEnv("POLYMARKET_BUILDER_API_KEY", "POLYMARKET_BUILDER_API_SECRET", "POLYMARKET_BUILDER_API_PASSPHRASE")
	if err != nil {
		return Config{}, err
	}
	if builder != nil {
		cfg.Builder = &BuilderCredentials{APIKey: builder.APIKey, Secret: builder.Secret, Passphrase: builder.Passphrase}
	}
	if raw := os.Getenv("POLYMARKET_PROXY_URL"); raw != "" {
		proxyURL, parseErr := url.Parse(raw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("parse POLYMARKET_PROXY_URL: %w", parseErr)
		}
		if proxyURL.Scheme == "" || proxyURL.Host == "" {
			return Config{}, fmt.Errorf("POLYMARKET_PROXY_URL must be an absolute URL")
		}
		cfg.ProxyURL = proxyURL
		cfg.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext}, Timeout: 20 * time.Second}
	}
	return cfg, nil
}

// privateKeyFromEnv resolves the signing key, preferring the encrypted-at-rest
// form. Setting both sources is an error rather than a silent preference: an
// operator who leaves a stale plaintext key behind while adding a keystore
// should be told, not quietly signed with one of the two.
func privateKeyFromEnv() (string, error) {
	plaintext := strings.TrimSpace(os.Getenv("POLYMARKET_PRIVATE_KEY"))
	keyPath := strings.TrimSpace(os.Getenv("POLYMARKET_PRIVATE_KEY_FILE"))
	if keyPath == "" {
		return plaintext, nil
	}
	if plaintext != "" {
		return "", fmt.Errorf("POLYMARKET_PRIVATE_KEY and POLYMARKET_PRIVATE_KEY_FILE are both set; use exactly one")
	}
	passphrase, err := passphraseFromEnv()
	if err != nil {
		return "", err
	}
	privateKey, _, err := keyfile.Load(keyPath, passphrase)
	if err != nil {
		return "", fmt.Errorf("POLYMARKET_PRIVATE_KEY_FILE: %w", err)
	}
	return privateKey, nil
}

func passphraseFromEnv() (string, error) {
	passphraseFile := strings.TrimSpace(os.Getenv("POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE"))
	if passphraseFile != "" {
		passphrase, err := keyfile.ReadPassphrase(passphraseFile)
		if err != nil {
			return "", fmt.Errorf("POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE: %w", err)
		}
		return passphrase, nil
	}
	if passphrase := os.Getenv("POLYMARKET_PRIVATE_KEY_PASSPHRASE"); passphrase != "" {
		return passphrase, nil
	}
	return "", fmt.Errorf("POLYMARKET_PRIVATE_KEY_FILE is set but neither POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE nor POLYMARKET_PRIVATE_KEY_PASSPHRASE is")
}

func credentialsFromEnv(keyName, secretName, passphraseName string) (*Credentials, error) {
	values := []string{strings.TrimSpace(os.Getenv(keyName)), strings.TrimSpace(os.Getenv(secretName)), strings.TrimSpace(os.Getenv(passphraseName))}
	set := 0
	for _, value := range values {
		if value != "" {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	}
	if set != len(values) {
		return nil, fmt.Errorf("%s, %s, and %s must be set together", keyName, secretName, passphraseName)
	}
	return &Credentials{APIKey: values[0], Secret: values[1], Passphrase: values[2]}, nil
}

func envInt64(name string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return value, nil
}
