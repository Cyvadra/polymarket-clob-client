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

// payoutDenominatorSelector is ConditionalTokens payoutDenominator(bytes32),
// payoutNumeratorsSelector payoutNumerators(bytes32,uint256).
var (
	payoutDenominatorSelector = common.Hex2Bytes("dd34de67")
	payoutNumeratorsSelector  = common.Hex2Bytes("0504c814")
)

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

// ConditionPayouts reports which outcomes of a condition pay out, by outcome
// index, as the ConditionalTokens contract records them. It is nil while the
// condition is unresolved. The chain resolves minutes before the CLOB marks
// the market closed, and Polymarket redeems winners in between.
func (c *Client) ConditionPayouts(ctx context.Context, conditionID string) ([]bool, error) {
	if c.cfg.ChainID != ChainPolygonMainnet {
		return nil, fmt.Errorf("on-chain condition payouts are only configured for Polygon mainnet")
	}
	id := common.FromHex(conditionID)
	if len(id) != 32 {
		return nil, fmt.Errorf("invalid condition id %q", conditionID)
	}
	rpc, err := c.rpcClient(ctx)
	if err != nil {
		return nil, err
	}
	contract := common.HexToAddress(ConditionalTokensAddress)
	call := func(data []byte) (*big.Int, error) {
		out, err := rpc.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
		if err != nil {
			return nil, err
		}
		if len(out) != 32 {
			return nil, fmt.Errorf("returned %d bytes", len(out))
		}
		return new(big.Int).SetBytes(out), nil
	}
	denominator, err := call(append(append([]byte{}, payoutDenominatorSelector...), id...))
	if err != nil {
		return nil, fmt.Errorf("payoutDenominator %s: %w", conditionID, err)
	}
	if denominator.Sign() == 0 {
		return nil, nil
	}
	// Polymarket markets are binary.
	pays := make([]bool, 2)
	for i := range pays {
		data := append(append([]byte{}, payoutNumeratorsSelector...), id...)
		data = append(data, common.LeftPadBytes(big.NewInt(int64(i)).Bytes(), 32)...)
		numerator, err := call(data)
		if err != nil {
			return nil, fmt.Errorf("payoutNumerators %s/%d: %w", conditionID, i, err)
		}
		pays[i] = numerator.Sign() > 0
	}
	return pays, nil
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
