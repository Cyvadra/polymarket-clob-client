package clobclient

import "testing"

func TestConfigFromEnv(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("POLYMARKET_PRIVATE_KEY", "private")
	t.Setenv("POLYMARKET_MAKER_ADDRESS", "0x1234")
	t.Setenv("POLYMARKET_CHAIN_ID", "137")
	t.Setenv("POLYMARKET_SIGNATURE_TYPE", "2")
	t.Setenv("POLYMARKET_PROXY_URL", "http://localhost:7890")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrivateKey != "private" || cfg.MakerAddress != "0x1234" || cfg.ChainID != 137 || cfg.SignatureType != SignatureTypeGnosisSafe || cfg.HTTPClient == nil {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestConfigFromEnvRejectsPartialCredentials(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("POLYMARKET_API_KEY", "key")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected partial credential error")
	}
}

func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"POLYMARKET_API_KEY", "POLYMARKET_API_SECRET", "POLYMARKET_API_PASSPHRASE", "POLYMARKET_BUILDER_API_KEY", "POLYMARKET_BUILDER_API_SECRET", "POLYMARKET_BUILDER_API_PASSPHRASE"} {
		t.Setenv(name, "")
	}
}
