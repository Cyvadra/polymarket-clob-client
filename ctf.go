package clobclient

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ConditionalTokensAddress is Gnosis's ConditionalTokens (ERC-1155) contract
// on Polygon, which holds every Polymarket outcome token.
const ConditionalTokensAddress = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"

// balanceOfSelector is ERC-1155 balanceOf(address,uint256).
var balanceOfSelector = common.Hex2Bytes("00fdd58e")

type rpcConn struct {
	mu     sync.Mutex
	client *ethclient.Client
}

// TokenBalance returns the maker wallet's balance of an outcome token in base
// units (6 decimals), read from the chain. Unlike the CLOB's balance
// endpoints it keeps answering after a market's order book is removed, which
// is exactly when a settled position needs valuing.
func (c *Client) TokenBalance(ctx context.Context, tokenID string) (string, error) {
	if c.cfg.ChainID != ChainPolygonMainnet {
		return "", fmt.Errorf("on-chain token balance is only configured for Polygon mainnet")
	}
	if c.cfg.MakerAddress == "" {
		return "", fmt.Errorf("token balance needs a maker address")
	}
	id, ok := new(big.Int).SetString(tokenID, 10)
	if !ok || id.Sign() < 0 {
		return "", fmt.Errorf("invalid token id %q", tokenID)
	}
	rpc, err := c.rpcClient(ctx)
	if err != nil {
		return "", err
	}
	contract := common.HexToAddress(ConditionalTokensAddress)
	data := append(append([]byte{}, balanceOfSelector...), common.LeftPadBytes(common.HexToAddress(c.cfg.MakerAddress).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(id.Bytes(), 32)...)
	out, err := rpc.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return "", fmt.Errorf("balanceOf %s: %w", tokenID, err)
	}
	if len(out) != 32 {
		return "", fmt.Errorf("balanceOf %s returned %d bytes", tokenID, len(out))
	}
	return new(big.Int).SetBytes(out).String(), nil
}

// rpcClient dials the RPC endpoint once and reuses the connection.
func (c *Client) rpcClient(ctx context.Context) (*ethclient.Client, error) {
	c.rpc.mu.Lock()
	defer c.rpc.mu.Unlock()
	if c.rpc.client != nil {
		return c.rpc.client, nil
	}
	if c.cfg.RPCEndpoint == "" {
		return nil, fmt.Errorf("no RPC endpoint is configured")
	}
	client, err := ethclient.DialContext(ctx, c.cfg.RPCEndpoint)
	if err != nil {
		return nil, fmt.Errorf("dial RPC: %w", err)
	}
	c.rpc.client = client
	return client, nil
}
