package keyfile

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// A sealed key file is
//
//	magic "PKS1" | version | nonce (12 bytes) | AES-256-GCM(keystore v3 JSON)
//
// with the header authenticated as additional data. The keystore inside is
// itself encrypted under a passphrase peppered with the app secret, so
// stripping the envelope still does not yield a file geth or any other wallet
// can open with the operator's passphrase.
var sealMagic = []byte("PKS1")

const (
	sealVersion   = 1
	sealNonceSize = 12
	sealHeaderLen = 4 + 1 + sealNonceSize
)

// errLegacyKeystore reports a plain keystore v3 file written before sealing.
var errLegacyKeystore = fmt.Errorf("key file is an unsealed keystore v3 JSON from an older polykey; convert it with: polykey migrate")

func sealAEAD() (cipher.AEAD, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, appSecret(), nil, []byte("keyfile-envelope-v1")), key); err != nil {
		return nil, fmt.Errorf("derive envelope key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal wraps keystore JSON in the program-bound envelope.
func seal(keyJSON []byte) ([]byte, error) {
	aead, err := sealAEAD()
	if err != nil {
		return nil, err
	}
	header := make([]byte, sealHeaderLen)
	copy(header, sealMagic)
	header[4] = sealVersion
	if _, err := rand.Read(header[5:]); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(header, header[5:], keyJSON, header), nil
}

// unseal checks and removes the envelope, returning the keystore JSON inside.
func unseal(sealed []byte) ([]byte, error) {
	if isLegacyKeystore(sealed) {
		return nil, errLegacyKeystore
	}
	if len(sealed) < sealHeaderLen || !bytes.Equal(sealed[:4], sealMagic) {
		return nil, fmt.Errorf("not a sealed key file")
	}
	if sealed[4] != sealVersion {
		return nil, fmt.Errorf("unsupported sealed key file version %d", sealed[4])
	}
	aead, err := sealAEAD()
	if err != nil {
		return nil, err
	}
	header := sealed[:sealHeaderLen]
	keyJSON, err := aead.Open(nil, header[5:], sealed[sealHeaderLen:], header)
	if err != nil {
		return nil, fmt.Errorf("sealed key file is corrupt or was sealed by a different build")
	}
	return keyJSON, nil
}

func isLegacyKeystore(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// pepper turns the operator's passphrase into the one the inner keystore is
// actually encrypted under.
func pepper(passphrase string) string {
	mac := hmac.New(sha256.New, appSecret())
	mac.Write([]byte("keystore-pass-v1"))
	mac.Write([]byte(passphrase))
	return hex.EncodeToString(mac.Sum(nil))
}
