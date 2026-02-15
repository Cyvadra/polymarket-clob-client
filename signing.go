package clobclient

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

const (
	ClobAuthDomain = "ClobAuthDomain"
	ClobVersion    = "1"
)

// BuildClobEip712Signature creates an EIP712 signature for CLOB authentication
func BuildClobEip712Signature(
	chainID int,
	privateKey string,
	timestamp int64,
	nonce string,
) (string, error) {
	// Create the typed data for EIP712
	typedData := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
			},
			"ClobAuth": []apitypes.Type{
				{Name: "address", Type: "address"},
				{Name: "timestamp", Type: "string"},
				{Name: "nonce", Type: "string"},
				{Name: "message", Type: "string"},
			},
		},
		PrimaryType: "ClobAuth",
		Domain: apitypes.TypedDataDomain{
			Name:    ClobAuthDomain,
			Version: ClobVersion,
			ChainId: (*math.HexOrDecimal256)(big.NewInt(int64(chainID))),
		},
		Message: apitypes.TypedDataMessage{},
	}

	// Get address from private key
	privateKeyBytes, err := hexutil.Decode(privateKey)
	if err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}

	key, err := crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	address := crypto.PubkeyToAddress(key.PublicKey)

	// Set message data
	typedData.Message["address"] = address.Hex()
	typedData.Message["timestamp"] = fmt.Sprintf("%d", timestamp)
	typedData.Message["nonce"] = nonce
	typedData.Message["message"] = "Signing in to ClobAuth"

	// Hash the typed data
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return "", fmt.Errorf("failed to hash domain: %w", err)
	}

	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return "", fmt.Errorf("failed to hash message: %w", err)
	}

	// Create the final hash: keccak256("\x19\x01" + domainSeparator + messageHash)
	rawData := []byte{0x19, 0x01}
	rawData = append(rawData, domainSeparator...)
	rawData = append(rawData, messageHash...)
	hash := crypto.Keccak256(rawData)

	// Sign the hash
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		return "", fmt.Errorf("failed to sign: %w", err)
	}

	// Adjust V value (add 27 to the recovery ID)
	if signature[64] < 27 {
		signature[64] += 27
	}

	return hexutil.Encode(signature), nil
}

// BuildOrderSignature creates an EIP712 signature for an order
func BuildOrderSignature(
	chainID int,
	privateKey string,
	order *SignedOrder,
	signatureType SignatureType,
) (string, error) {
	// Create the typed data for order signing
	typedData := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Order": []apitypes.Type{
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
		PrimaryType: "Order",
		Domain: apitypes.TypedDataDomain{
			Name:              "Polymarket CTF Exchange",
			Version:           "1",
			ChainId:           (*math.HexOrDecimal256)(big.NewInt(int64(chainID))),
			VerifyingContract: getExchangeAddress(chainID),
		},
		Message: apitypes.TypedDataMessage{},
	}

	// Convert order fields to message
	typedData.Message["salt"] = fmt.Sprintf("%d", order.Salt)
	typedData.Message["maker"] = order.Maker
	typedData.Message["signer"] = order.Signer
	typedData.Message["taker"] = order.Taker
	typedData.Message["tokenId"] = order.TokenID
	typedData.Message["makerAmount"] = order.MakerAmount
	typedData.Message["takerAmount"] = order.TakerAmount
	typedData.Message["expiration"] = order.Expiration
	typedData.Message["nonce"] = order.Nonce
	typedData.Message["feeRateBps"] = order.FeeRateBps

	// Convert side to uint8
	var sideValue uint8
	if order.Side == SideBuy {
		sideValue = 0
	} else {
		sideValue = 1
	}
	typedData.Message["side"] = fmt.Sprintf("%d", sideValue)
	typedData.Message["signatureType"] = fmt.Sprintf("%d", signatureType)

	// Get private key
	privateKeyBytes, err := hexutil.Decode(privateKey)
	if err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}

	key, err := crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	// Hash the typed data
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return "", fmt.Errorf("failed to hash domain: %w", err)
	}

	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return "", fmt.Errorf("failed to hash message: %w", err)
	}

	// Create the final hash
	rawData := []byte{0x19, 0x01}
	rawData = append(rawData, domainSeparator...)
	rawData = append(rawData, messageHash...)
	hash := crypto.Keccak256(rawData)

	// Sign the hash
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		return "", fmt.Errorf("failed to sign: %w", err)
	}

	// Adjust V value
	if signature[64] < 27 {
		signature[64] += 27
	}

	return hexutil.Encode(signature), nil
}

// getExchangeAddress returns the exchange contract address for a given chain
func getExchangeAddress(chainID int) string {
	switch chainID {
	case 137: // Polygon
		return "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	case 80002: // Amoy
		return "0xdFE02Eb6733538f8Ea35D585af8DE5958AD99E40"
	default:
		return "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	}
}

// BuildPolyHmacSignature creates an HMAC signature for API authentication
func BuildPolyHmacSignature(
	secret string,
	timestamp int64,
	method string,
	requestPath string,
	body string,
) (string, error) {
	// Create the message to sign: timestamp + method + requestPath + body
	message := fmt.Sprintf("%d%s%s%s", timestamp, method, requestPath, body)

	// Create HMAC-SHA256 hash
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(message))
	hash := h.Sum(nil)

	// Base64 URL encode the hash
	signature := base64.URLEncoding.EncodeToString(hash)

	// Remove trailing '=' padding for URL safety
	signature = strings.TrimRight(signature, "=")

	return signature, nil
}

// GetAddressFromPrivateKey returns the Ethereum address from a private key
func GetAddressFromPrivateKey(privateKey string) (string, error) {
	privateKeyBytes, err := hexutil.Decode(privateKey)
	if err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}

	key, err := crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	address := crypto.PubkeyToAddress(key.PublicKey)
	return address.Hex(), nil
}

// IsValidAddress checks if a string is a valid Ethereum address
func IsValidAddress(address string) bool {
	return common.IsHexAddress(address)
}
