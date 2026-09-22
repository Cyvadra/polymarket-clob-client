package keyfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
)

// Tests use the light KDF parameters; the standard ones take about a second
// per decryption, which is fine once at startup but not in a test loop.
const (
	testScryptN = keystore.LightScryptN
	testScryptP = keystore.LightScryptP
)

const (
	testKeyHex  = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	testAddress = "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	encrypted, err := Encrypt(testKeyHex, "correct horse", testScryptN, testScryptP)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	hexKey, address, err := Decrypt(encrypted, "correct horse")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if hexKey != "0x"+testKeyHex {
		t.Fatalf("hex key = %q, want 0x%s", hexKey, testKeyHex)
	}
	if !strings.EqualFold(address, testAddress) {
		t.Fatalf("address = %q, want %q", address, testAddress)
	}
}

func TestEncryptAcceptsPrefixedAndPaddedHex(t *testing.T) {
	encrypted, err := Encrypt("  0x"+testKeyHex+"\n", "pass", testScryptN, testScryptP)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	hexKey, _, err := Decrypt(encrypted, "pass")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if hexKey != "0x"+testKeyHex {
		t.Fatalf("hex key = %q, want 0x%s", hexKey, testKeyHex)
	}
}

func TestEncryptRejectsBadInput(t *testing.T) {
	if _, err := Encrypt("", "pass", testScryptN, testScryptP); err == nil {
		t.Fatal("expected an error for an empty private key")
	}
	if _, err := Encrypt("not-hex", "pass", testScryptN, testScryptP); err == nil {
		t.Fatal("expected an error for a non-hex private key")
	}
	if _, err := Encrypt(testKeyHex, "", testScryptN, testScryptP); err == nil {
		t.Fatal("expected an error for an empty passphrase")
	}
}

func TestDecryptWrongPassphrase(t *testing.T) {
	encrypted, err := Encrypt(testKeyHex, "right", testScryptN, testScryptP)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, _, err := Decrypt(encrypted, "wrong"); err == nil {
		t.Fatal("expected an error for the wrong passphrase")
	}
}

func TestLoadAndAddress(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	encrypted, err := Encrypt(testKeyHex, "pass", testScryptN, testScryptP)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := Write(keyPath, encrypted, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	hexKey, address, err := Load(keyPath, "pass")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if hexKey != "0x"+testKeyHex {
		t.Fatalf("hex key = %q, want 0x%s", hexKey, testKeyHex)
	}

	// Address must agree with Load without needing the passphrase.
	peeked, err := Address(keyPath)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if peeked != address {
		t.Fatalf("Address = %q, Load reported %q", peeked, address)
	}
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatalf("HexToECDSA: %v", err)
	}
	if want := crypto.PubkeyToAddress(key.PublicKey).Hex(); peeked != want {
		t.Fatalf("Address = %q, want %q", peeked, want)
	}
}

func TestWriteRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	if err := Write(keyPath, []byte(`{"address":"x"}`), false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Write(keyPath, []byte(`{"address":"y"}`), false); err == nil {
		t.Fatal("expected an error when overwriting without force")
	}
	if err := Write(keyPath, []byte(`{"address":"y"}`), true); err != nil {
		t.Fatalf("Write with force: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %#o, want 0600", perm)
	}
}

func TestReadPassphraseTrimsTrailingNewline(t *testing.T) {
	path := writeFile(t, "hunter2 with spaces\n", 0o600)
	passphrase, err := ReadPassphrase(path)
	if err != nil {
		t.Fatalf("ReadPassphrase: %v", err)
	}
	if passphrase != "hunter2 with spaces" {
		t.Fatalf("passphrase = %q", passphrase)
	}
}

func TestReadPassphraseRejectsEmpty(t *testing.T) {
	path := writeFile(t, "   \n", 0o600)
	if _, err := ReadPassphrase(path); err == nil {
		t.Fatal("expected an error for a blank passphrase file")
	}
}

func TestReadPassphraseRejectsLoosePermissions(t *testing.T) {
	path := writeFile(t, "hunter2\n", 0o644)
	_, err := ReadPassphrase(path)
	if err == nil {
		t.Fatal("expected an error for a group/world-readable passphrase file")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error should tell the operator how to fix it, got: %v", err)
	}
}

func TestLoadRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	encrypted, err := Encrypt(testKeyHex, "pass", testScryptN, testScryptP)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := os.WriteFile(keyPath, encrypted, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := Load(keyPath, "pass"); err == nil {
		t.Fatal("expected an error for a group-readable keystore file")
	}
}

func TestAddressRejectsMalformedJSON(t *testing.T) {
	if _, err := Address(writeFile(t, "not json", 0o600)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if _, err := Address(writeFile(t, `{"version":3}`, 0o600)); err == nil {
		t.Fatal("expected an error for JSON without an address")
	}
}

func writeFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	return path
}
