package clobclient

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

func (c *Client) DeriveCredentials(ctx context.Context) (*Credentials, error) {
	return c.l1Credentials(ctx, http.MethodGet, "/auth/derive-api-key")
}

func (c *Client) CreateCredentials(ctx context.Context) (*Credentials, error) {
	return c.l1Credentials(ctx, http.MethodPost, "/auth/api-key")
}

func (c *Client) CreateOrDeriveCredentials(ctx context.Context) (*Credentials, error) {
	credentials, err := c.DeriveCredentials(ctx)
	if err == nil {
		return credentials, nil
	}
	return c.CreateCredentials(ctx)
}

func (c *Client) ProxyWallets(ctx context.Context) ([]string, error) {
	if c.key == nil {
		return nil, fmt.Errorf("private key is required")
	}
	var response struct {
		Wallets []string `json:"proxy-wallets"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: http.MethodGet, Path: "/auth/get-proxy-wallets", Headers: c.l1Headers}, &response)
	if err != nil {
		return nil, err
	}
	return response.Wallets, nil
}

// ResolveProxyWallet returns the Safe/deposit wallet associated with the signer.
// It falls back to the CTF Exchange contract because the CLOB discovery
// endpoint is not deployed in every environment.
func (c *Client) ResolveProxyWallet(ctx context.Context) (string, error) {
	wallets, endpointErr := c.ProxyWallets(ctx)
	if endpointErr == nil && len(wallets) > 0 && wallets[0] != "" {
		return wallets[0], nil
	}
	if c.cfg.RPCEndpoint == "" {
		return "", fmt.Errorf("proxy wallet endpoint failed and no RPC endpoint is configured: %w", endpointErr)
	}
	if c.cfg.ChainID != ChainPolygonMainnet {
		return "", fmt.Errorf("proxy wallet endpoint failed (%v); on-chain fallback is only configured for Polygon mainnet", endpointErr)
	}
	rpcClient, err := ethclient.DialContext(ctx, c.cfg.RPCEndpoint)
	if err != nil {
		return "", fmt.Errorf("proxy wallet endpoint failed (%v); dial RPC: %w", endpointErr, err)
	}
	defer rpcClient.Close()
	exchange := common.HexToAddress("0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E")
	signer := common.HexToAddress(c.signer)
	data := append(common.Hex2Bytes("a287bdf1"), common.LeftPadBytes(signer.Bytes(), 32)...)
	response, err := rpcClient.CallContract(ctx, ethereum.CallMsg{To: &exchange, Data: data}, nil)
	if err != nil {
		return "", fmt.Errorf("query Safe address: %w", err)
	}
	if len(response) < 32 {
		return "", fmt.Errorf("getSafeAddress returned %d bytes", len(response))
	}
	wallet := common.BytesToAddress(response[12:32])
	if wallet == (common.Address{}) {
		return "", fmt.Errorf("no proxy wallet found for %s", c.signer)
	}
	return wallet.Hex(), nil
}

// WithResolvedProxyWallet returns a new client configured to sign orders for
// the discovered Safe/deposit wallet. The source client remains unchanged.
func (c *Client) WithResolvedProxyWallet(ctx context.Context) (*Client, error) {
	wallet, err := c.ResolveProxyWallet(ctx)
	if err != nil {
		return nil, err
	}
	cfg := c.cfg
	cfg.MakerAddress = wallet
	cfg.SignatureType = SignatureTypeGnosisSafe
	return New(cfg)
}

func (c *Client) l1Credentials(ctx context.Context, method, path string) (*Credentials, error) {
	if c.key == nil {
		return nil, fmt.Errorf("private key is required")
	}
	var response struct {
		APIKey     string `json:"apiKey"`
		Secret     string `json:"secret"`
		Passphrase string `json:"passphrase"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: method, Path: path, Headers: c.l1Headers}, &response)
	if err != nil {
		return nil, err
	}
	if response.APIKey == "" || response.Secret == "" || response.Passphrase == "" {
		return nil, fmt.Errorf("CLOB returned incomplete credentials")
	}
	return &Credentials{APIKey: response.APIKey, Secret: response.Secret, Passphrase: response.Passphrase}, nil
}

func (c *Client) l1Headers(_ string, _ string, _ []byte) (http.Header, error) {
	timestamp := c.cfg.Now().Unix()
	typedData := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {{Name: "name", Type: "string"}, {Name: "version", Type: "string"}, {Name: "chainId", Type: "uint256"}},
			"ClobAuth":     {{Name: "address", Type: "address"}, {Name: "timestamp", Type: "string"}, {Name: "nonce", Type: "uint256"}, {Name: "message", Type: "string"}},
		},
		PrimaryType: "ClobAuth",
		Domain:      apitypes.TypedDataDomain{Name: "ClobAuthDomain", Version: "1", ChainId: math.NewHexOrDecimal256(c.cfg.ChainID)},
		Message:     apitypes.TypedDataMessage{"address": c.signer, "timestamp": strconv.FormatInt(timestamp, 10), "nonce": math.NewHexOrDecimal256(0), "message": "This message attests that I control the given wallet"},
	}
	domain, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return nil, err
	}
	message, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return nil, err
	}
	digest := crypto.Keccak256Hash([]byte("\x19\x01"), domain, message)
	signature, err := crypto.Sign(digest.Bytes(), c.key)
	if err != nil {
		return nil, err
	}
	signature[64] += 27
	headers := make(http.Header)
	headers.Set("POLY_ADDRESS", c.signer)
	headers.Set("POLY_SIGNATURE", hexutil.Encode(signature))
	headers.Set("POLY_TIMESTAMP", strconv.FormatInt(timestamp, 10))
	headers.Set("POLY_NONCE", "0")
	return headers, nil
}
