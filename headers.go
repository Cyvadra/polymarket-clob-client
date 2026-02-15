package clobclient

import (
	"fmt"
	"strconv"
	"time"
)

// CreateL1Headers creates headers for L1 authentication (wallet signature)
func CreateL1Headers(
	chainID int,
	privateKey string,
	nonce string,
) (map[string]string, error) {
	timestamp := time.Now().Unix()

	// Get address from private key
	address, err := GetAddressFromPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Build EIP712 signature
	signature, err := BuildClobEip712Signature(chainID, privateKey, timestamp, nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to build signature: %w", err)
	}

	headers := map[string]string{
		"POLY_ADDRESS":   address,
		"POLY_SIGNATURE": signature,
		"POLY_TIMESTAMP": strconv.FormatInt(timestamp, 10),
		"POLY_NONCE":     nonce,
	}

	return headers, nil
}

// CreateL2Headers creates headers for L2 authentication (API key)
func CreateL2Headers(
	privateKey string,
	creds *ApiKeyCreds,
	method string,
	requestPath string,
	body string,
) (map[string]string, error) {
	timestamp := time.Now().Unix()

	// Get address from private key
	address, err := GetAddressFromPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Build HMAC signature
	signature, err := BuildPolyHmacSignature(
		creds.Secret,
		timestamp,
		method,
		requestPath,
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build HMAC signature: %w", err)
	}

	headers := map[string]string{
		"POLY_ADDRESS":    address,
		"POLY_SIGNATURE":  signature,
		"POLY_TIMESTAMP":  strconv.FormatInt(timestamp, 10),
		"POLY_API_KEY":    creds.Key,
		"POLY_PASSPHRASE": creds.Passphrase,
	}

	return headers, nil
}

// CreateL2HeadersWithTimestamp creates L2 headers with a specific timestamp
func CreateL2HeadersWithTimestamp(
	privateKey string,
	creds *ApiKeyCreds,
	method string,
	requestPath string,
	body string,
	timestamp int64,
) (map[string]string, error) {
	// Get address from private key
	address, err := GetAddressFromPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Build HMAC signature
	signature, err := BuildPolyHmacSignature(
		creds.Secret,
		timestamp,
		method,
		requestPath,
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build HMAC signature: %w", err)
	}

	headers := map[string]string{
		"POLY_ADDRESS":    address,
		"POLY_SIGNATURE":  signature,
		"POLY_TIMESTAMP":  strconv.FormatInt(timestamp, 10),
		"POLY_API_KEY":    creds.Key,
		"POLY_PASSPHRASE": creds.Passphrase,
	}

	return headers, nil
}

// InjectBuilderHeaders injects builder API key headers into existing L2 headers
func InjectBuilderHeaders(
	headers map[string]string,
	builderCreds *BuilderApiKey,
	method string,
	requestPath string,
	body string,
	timestamp int64,
) (map[string]string, error) {
	// Build HMAC signature for builder
	signature, err := BuildPolyHmacSignature(
		builderCreds.Secret,
		timestamp,
		method,
		requestPath,
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build builder HMAC signature: %w", err)
	}

	// Add builder headers
	headers["POLY_BUILDER_API_KEY"] = builderCreds.Key
	headers["POLY_BUILDER_TIMESTAMP"] = strconv.FormatInt(timestamp, 10)
	headers["POLY_BUILDER_PASSPHRASE"] = builderCreds.Passphrase
	headers["POLY_BUILDER_SIGNATURE"] = signature

	return headers, nil
}
