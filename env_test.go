package clobclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"

	"github.com/Cyvadra/polymarket-clob-client/internal/keyfile"
)

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
	for _, name := range []string{"POLYMARKET_PRIVATE_KEY", "POLYMARKET_PRIVATE_KEY_FILE", "POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE", "POLYMARKET_PRIVATE_KEY_PASSPHRASE"} {
		t.Setenv(name, "")
	}
}

const testPrivateKeyHex = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func TestConfigFromEnvDecryptsPrivateKeyFile(t *testing.T) {
	clearCredentialEnv(t)
	keyPath, passphrasePath := writeTestKeystore(t, "hunter2")
	t.Setenv("POLYMARKET_PRIVATE_KEY_FILE", keyPath)
	t.Setenv("POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE", passphrasePath)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrivateKey != "0x"+testPrivateKeyHex {
		t.Fatalf("PrivateKey = %q, want 0x%s", cfg.PrivateKey, testPrivateKeyHex)
	}
}

func TestConfigFromEnvAcceptsInlinePassphrase(t *testing.T) {
	clearCredentialEnv(t)
	keyPath, _ := writeTestKeystore(t, "hunter2")
	t.Setenv("POLYMARKET_PRIVATE_KEY_FILE", keyPath)
	t.Setenv("POLYMARKET_PRIVATE_KEY_PASSPHRASE", "hunter2")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrivateKey != "0x"+testPrivateKeyHex {
		t.Fatalf("PrivateKey = %q, want 0x%s", cfg.PrivateKey, testPrivateKeyHex)
	}
}

func TestConfigFromEnvRejectsBothPrivateKeySources(t *testing.T) {
	clearCredentialEnv(t)
	keyPath, passphrasePath := writeTestKeystore(t, "hunter2")
	t.Setenv("POLYMARKET_PRIVATE_KEY", "0x"+testPrivateKeyHex)
	t.Setenv("POLYMARKET_PRIVATE_KEY_FILE", keyPath)
	t.Setenv("POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE", passphrasePath)

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected an error when both private key sources are set")
	}
}

func TestConfigFromEnvRequiresPassphraseForKeyFile(t *testing.T) {
	clearCredentialEnv(t)
	keyPath, _ := writeTestKeystore(t, "hunter2")
	t.Setenv("POLYMARKET_PRIVATE_KEY_FILE", keyPath)

	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected an error when no passphrase source is set")
	}
}

func TestConfigFromEnvWithoutPrivateKey(t *testing.T) {
	clearCredentialEnv(t)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrivateKey != "" {
		t.Fatalf("PrivateKey = %q, want empty", cfg.PrivateKey)
	}
}

// writeTestKeystore encrypts the shared test key with the light KDF
// parameters; the standard ones cost about a second per decryption.
func writeTestKeystore(t *testing.T, passphrase string) (keyPath, passphrasePath string) {
	t.Helper()
	dir := t.TempDir()
	keyPath = filepath.Join(dir, "private-key.json")
	passphrasePath = filepath.Join(dir, "private-key.pass")

	encrypted, err := keyfile.Encrypt(testPrivateKeyHex, passphrase, keystore.LightScryptN, keystore.LightScryptP)
	if err != nil {
		t.Fatalf("encrypt test key: %v", err)
	}
	if err := keyfile.Write(keyPath, encrypted, true); err != nil {
		t.Fatalf("write test keystore: %v", err)
	}
	if err := os.WriteFile(passphrasePath, []byte(passphrase+"\n"), 0o600); err != nil {
		t.Fatalf("write test passphrase: %v", err)
	}
	if err := os.Chmod(passphrasePath, 0o600); err != nil {
		t.Fatalf("chmod test passphrase: %v", err)
	}
	return keyPath, passphrasePath
}
