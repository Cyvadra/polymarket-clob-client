// Package keyfile stores an Ethereum signing key encrypted at rest.
//
// The key is encrypted as Web3 Secret Storage v3 (the keystore JSON geth and
// clef write) under a passphrase peppered with an app secret compiled into
// this program, and that JSON is sealed in an AES-256-GCM envelope keyed from
// the same secret (see seal.go). The key file and the passphrase together are
// therefore not enough to recover the key without this program; this is
// deliberate, and it means the files no longer open in geth or other wallets.
// The passphrase lives in a separate file so an operator can keep the two on
// different media; both files must be readable only by their owner.
package keyfile

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
)

// ScryptN and ScryptP are the KDF parameters used for new keystores. The
// standard (not "light") parameters cost roughly a second of CPU per
// decryption, which is paid once per process start.
//
// They also cost memory: scrypt allocates 128*N*r bytes, so with
// StandardScryptN (1<<18) and r=8 a decryption transiently needs ~256 MiB.
// A host that cannot spare that at startup will OOM-kill the daemon, which
// under pm2 becomes a restart loop; size the box accordingly, or re-encrypt
// the keystore with LightScryptN (~4 MiB) and accept the weaker KDF.
const (
	ScryptN = keystore.StandardScryptN
	ScryptP = keystore.StandardScryptP
)

// Encrypt converts a hex-encoded secp256k1 private key into a sealed key file.
// The hex may carry a "0x" prefix and surrounding whitespace, matching what
// clobclient.Config.PrivateKey accepts.
func Encrypt(privateKeyHex, passphrase string, scryptN, scryptP int) ([]byte, error) {
	key, err := parseHexKey(privateKeyHex)
	if err != nil {
		return nil, err
	}
	if passphrase == "" {
		return nil, fmt.Errorf("passphrase must not be empty")
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generate keystore id: %w", err)
	}
	encrypted, err := keystore.EncryptKey(&keystore.Key{
		Id:         id,
		Address:    crypto.PubkeyToAddress(key.PublicKey),
		PrivateKey: key,
	}, pepper(passphrase), scryptN, scryptP)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}
	return seal(encrypted)
}

// Decrypt returns the hex-encoded private key and its address from a sealed
// key file. The returned string is secret: hand it straight to the client and
// never log it or write it back to disk or the environment.
func Decrypt(keyJSON []byte, passphrase string) (privateKeyHex, address string, err error) {
	inner, err := unseal(keyJSON)
	if err != nil {
		return "", "", err
	}
	key, err := keystore.DecryptKey(inner, pepper(passphrase))
	if err != nil {
		return "", "", fmt.Errorf("decrypt key: %w", err)
	}
	return hexKey(key.PrivateKey), key.Address.Hex(), nil
}

// DecryptLegacy decrypts a plain keystore v3 file written before sealing, with
// the raw passphrase. It exists only so polykey migrate can convert one.
func DecryptLegacy(keyJSON []byte, passphrase string) (privateKeyHex, address string, err error) {
	if !isLegacyKeystore(keyJSON) {
		return "", "", fmt.Errorf("not an unsealed keystore v3 JSON")
	}
	key, err := keystore.DecryptKey(keyJSON, passphrase)
	if err != nil {
		return "", "", fmt.Errorf("decrypt key: %w", err)
	}
	return hexKey(key.PrivateKey), key.Address.Hex(), nil
}

// ReadKeyFile reads a key file after the same permission check Load applies.
func ReadKeyFile(keyPath string) ([]byte, error) {
	return readPrivateFile(keyPath, "private key file")
}

// Load reads a keystore file and decrypts it. The file must not be readable
// by group or other.
func Load(keyPath, passphrase string) (privateKeyHex, address string, err error) {
	keyJSON, err := readPrivateFile(keyPath, "private key file")
	if err != nil {
		return "", "", err
	}
	privateKeyHex, address, err = Decrypt(keyJSON, passphrase)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", keyPath, err)
	}
	return privateKeyHex, address, nil
}

// Address reports which account a keystore file holds without needing the
// passphrase (but, being sealed, not without this program), so an operator (or the deploy preflight) can confirm the file
// is the one they meant to ship.
func Address(keyPath string) (string, error) {
	keyJSON, err := readPrivateFile(keyPath, "private key file")
	if err != nil {
		return "", err
	}
	inner, err := unseal(keyJSON)
	if err != nil {
		return "", fmt.Errorf("%s: %w", keyPath, err)
	}
	var header struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(inner, &header); err != nil {
		return "", fmt.Errorf("%s: parse keystore JSON: %w", keyPath, err)
	}
	if header.Address == "" {
		return "", fmt.Errorf("%s: keystore JSON has no address field", keyPath)
	}
	if !common.IsHexAddress(header.Address) {
		return "", fmt.Errorf("%s: keystore address %q is not a hex address", keyPath, header.Address)
	}
	return common.HexToAddress(header.Address).Hex(), nil
}

// ReadPassphrase reads a passphrase from a file, trimming the trailing
// newline an editor or `echo` leaves behind. The file must not be readable by
// group or other.
func ReadPassphrase(passphraseFile string) (string, error) {
	raw, err := readPrivateFile(passphraseFile, "passphrase file")
	if err != nil {
		return "", err
	}
	passphrase := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(passphrase) == "" {
		return "", fmt.Errorf("%s: passphrase file is empty", passphraseFile)
	}
	return passphrase, nil
}

// Write stores keystore JSON with owner-only permissions. It refuses to
// clobber an existing file unless force is set.
//
// Overwriting goes through a sibling temp file and a rename, so an interrupted
// write leaves the previous keystore intact rather than a truncated one. Both
// the file and its parent directory are fsynced, so a keystore that is visible
// after a crash is also complete.
func Write(keyPath string, keyJSON []byte, force bool) error {
	if !force {
		// O_EXCL reserves the name without touching any existing file, so the
		// no-force path can write in place: there is nothing to destroy.
		file, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", keyPath)
			}
			return err
		}
		if err := writeAndSync(file, keyPath, keyJSON); err != nil {
			return err
		}
		return syncDir(filepath.Dir(keyPath))
	}

	dir := filepath.Dir(keyPath)
	temp, err := os.CreateTemp(dir, ".keyfile-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	// CreateTemp already makes the file 0600, but be explicit: this is the
	// file that becomes the keystore.
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("chmod %s: %w", tempPath, err)
	}
	if err := writeAndSync(temp, tempPath, keyJSON); err != nil {
		return err
	}
	if err := os.Rename(tempPath, keyPath); err != nil {
		return fmt.Errorf("replace %s: %w", keyPath, err)
	}
	// The rename itself is only durable once the directory entry is synced;
	// without this a crash can surface the new name pointing at no data.
	return syncDir(dir)
}

// writeAndSync writes the payload and flushes it to stable storage before
// closing, so the bytes survive a power loss and not just a process death.
func writeAndSync(file *os.File, path string, payload []byte) error {
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// readPrivateFile reads a file after checking that no group or other bits are
// set, the way ssh refuses to use a world-readable identity file.
func readPrivateFile(path, kind string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s path must not be empty", kind)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s %s is a directory", kind, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s %s has mode %#o; it must not be readable by group or other (chmod 600 %s)", kind, path, perm, path)
	}
	return os.ReadFile(path)
}

func parseHexKey(privateKeyHex string) (*ecdsa.PrivateKey, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(privateKeyHex), "0x")
	if raw == "" {
		return nil, fmt.Errorf("private key must not be empty")
	}
	key, err := crypto.HexToECDSA(raw)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return key, nil
}

func hexKey(key *ecdsa.PrivateKey) string {
	return "0x" + common.Bytes2Hex(crypto.FromECDSA(key))
}
