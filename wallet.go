package clobclient

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Polygon mainnet contracts the deposit wallet calls. See
// docs.polymarket.com/trading/positions/manage.
const (
	// PUSDAddress is pUSD, Polymarket's collateral token (6 decimals).
	PUSDAddress = "0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"
	// CtfCollateralAdapter splits pUSD on a standard CTF market: it pulls the
	// pUSD, unwraps it, splits it in ConditionalTokens and returns both
	// outcome tokens to the caller.
	CtfCollateralAdapter = "0xAdA100Db00Ca00073811820692005400218FcE1f"
	// NegRiskCtfCollateralAdapter does the same for neg-risk markets.
	NegRiskCtfCollateralAdapter = "0xadA2005600Dec949baf300f4C6120000bDB6eAab"
)

var pusd = common.HexToAddress(PUSDAddress)

var (
	approveSelector           = selector("approve(address,uint256)")
	allowanceSelector         = selector("allowance(address,address)")
	erc20BalanceSelector      = selector("balanceOf(address)")
	transferSelector          = selector("transfer(address,uint256)")
	safeTransferFromSelector  = selector("safeTransferFrom(address,address,uint256,uint256,bytes)")
	splitPositionSelector     = selector("splitPosition(address,bytes32,bytes32,uint256[],uint256)")
	mergePositionsSelector    = selector("mergePositions(address,bytes32,bytes32,uint256[],uint256)")
	setApprovalForAllSelector = selector("setApprovalForAll(address,bool)")
	isApprovedForAllSelector  = selector("isApprovedForAll(address,address)")
)

// SplitCalls builds the deposit wallet calls that turn amount pUSD (base
// units, 6 decimals) into amount of each outcome token of a binary CTF
// market: a pUSD approval when the adapter's allowance falls short, then the
// split. Only markets with a 32-byte condition ID (Gamma version "v1") are
// supported; V2 markets split through Polymarket's Router instead.
func (c *Client) SplitCalls(ctx context.Context, conditionID string, amount *big.Int) ([]WalletCall, *Market, error) {
	wallet, err := c.walletAndAmount(amount, "split")
	if err != nil {
		return nil, nil, err
	}
	id, market, adapter, err := c.binaryMarket(ctx, conditionID)
	if err != nil {
		return nil, nil, err
	}
	if market.Closed {
		return nil, nil, fmt.Errorf("market %s is closed; its tokens are worth only their payout", conditionID)
	}
	// The /clob-markets fallback never reports a market closed, and the chain
	// resolves before the CLOB closes it, so ask the chain too.
	if c.cfg.ChainID == ChainPolygonMainnet {
		payouts, err := c.ConditionPayouts(ctx, conditionID)
		if err != nil {
			return nil, nil, fmt.Errorf("read resolution: %w", err)
		}
		if payouts != nil {
			return nil, nil, fmt.Errorf("condition %s is resolved; its tokens are worth only their payout", conditionID)
		}
	}
	if err := c.requirePUSD(ctx, wallet, amount, "split needs"); err != nil {
		return nil, nil, err
	}
	allowance, err := c.callUint(ctx, pusd, allowanceSelector, word(wallet), word(adapter))
	if err != nil {
		return nil, nil, fmt.Errorf("read pUSD allowance: %w", err)
	}
	var calls []WalletCall
	if allowance.Cmp(amount) < 0 {
		calls = append(calls, walletCall(pusd, approveSelector, word(adapter), uintWord(amount)))
	}
	// splitPosition(collateral, parentCollectionId=0, conditionId, partition=[1,2], amount)
	calls = append(calls, binaryPositionCall(adapter, splitPositionSelector, id, amount))
	return calls, market, nil
}

// MergeCalls builds the deposit wallet calls that turn amount (base units) of
// each outcome token of a binary CTF market back into amount pUSD: an
// ERC-1155 approval when the adapter may not yet move the wallet's tokens,
// then the merge. Unlike a split it works on a closed or resolved market, so
// leftover pairs can always be merged.
func (c *Client) MergeCalls(ctx context.Context, conditionID string, amount *big.Int) ([]WalletCall, *Market, error) {
	wallet, err := c.walletAndAmount(amount, "merge")
	if err != nil {
		return nil, nil, err
	}
	id, market, adapter, err := c.binaryMarket(ctx, conditionID)
	if err != nil {
		return nil, nil, err
	}
	ctf := common.HexToAddress(ConditionalTokensAddress)
	for _, token := range market.Tokens {
		tokenID, err := parseTokenID(token.TokenID)
		if err != nil {
			return nil, nil, err
		}
		balance, err := c.callUint(ctx, ctf, balanceOfSelector, word(wallet), uintWord(tokenID))
		if err != nil {
			return nil, nil, fmt.Errorf("read token %s balance: %w", token.TokenID, err)
		}
		if balance.Cmp(amount) < 0 {
			return nil, nil, fmt.Errorf("deposit wallet holds %s of token %s, merge needs %s", FormatUnits(balance), token.TokenID, FormatUnits(amount))
		}
	}
	approved, err := c.callUint(ctx, ctf, isApprovedForAllSelector, word(wallet), word(adapter))
	if err != nil {
		return nil, nil, fmt.Errorf("read token approval: %w", err)
	}
	var calls []WalletCall
	if approved.Sign() == 0 {
		calls = append(calls, walletCall(ctf, setApprovalForAllSelector, word(adapter), uintWord(big.NewInt(1))))
	}
	calls = append(calls, binaryPositionCall(adapter, mergePositionsSelector, id, amount))
	return calls, market, nil
}

// binaryMarket reads a binary market and picks the collateral adapter that
// splits and merges it. Only a 32-byte condition ID (Gamma version "v1") is
// supported; V2 markets go through Polymarket's Router instead.
func (c *Client) binaryMarket(ctx context.Context, conditionID string) ([]byte, *Market, common.Address, error) {
	id := common.FromHex(conditionID)
	if len(id) != 32 {
		if len(id) == 31 {
			return nil, nil, common.Address{}, fmt.Errorf("condition %s is a V2 market, which is not supported yet", conditionID)
		}
		return nil, nil, common.Address{}, fmt.Errorf("invalid condition id %q", conditionID)
	}
	market, err := c.Market(ctx, conditionID)
	if err != nil {
		return nil, nil, common.Address{}, fmt.Errorf("read market %s: %w", conditionID, err)
	}
	if len(market.Tokens) != 2 {
		return nil, nil, common.Address{}, fmt.Errorf("market %s has %d outcome tokens, want 2", conditionID, len(market.Tokens))
	}
	negRisk, err := c.NegRisk(ctx, market.Tokens[0].TokenID)
	if err != nil {
		return nil, nil, common.Address{}, fmt.Errorf("read neg-risk flag: %w", err)
	}
	adapter := common.HexToAddress(CtfCollateralAdapter)
	if negRisk {
		adapter = common.HexToAddress(NegRiskCtfCollateralAdapter)
	}
	return id, market, adapter, nil
}

// binaryPositionCall encodes splitPosition or mergePositions on an adapter:
// (collateral=pUSD, parentCollectionId=0, conditionId, partition=[1,2], amount).
func binaryPositionCall(adapter common.Address, sel, conditionID []byte, amount *big.Int) WalletCall {
	return walletCall(adapter, sel,
		word(pusd),
		make([]byte, 32),
		conditionID,
		uintWord(big.NewInt(5*32)), // offset of the partition array
		uintWord(amount),
		uintWord(big.NewInt(2)), uintWord(big.NewInt(1)), uintWord(big.NewInt(2)),
	)
}

// SendCollateralCalls builds the call that sends amount pUSD (base units)
// from the deposit wallet to another address.
func (c *Client) SendCollateralCalls(ctx context.Context, to string, amount *big.Int) ([]WalletCall, error) {
	wallet, recipient, err := c.sendParties(to, amount)
	if err != nil {
		return nil, err
	}
	if err := c.requirePUSD(ctx, wallet, amount, "cannot send"); err != nil {
		return nil, err
	}
	return []WalletCall{walletCall(pusd, transferSelector, word(recipient), uintWord(amount))}, nil
}

// SendTokenCalls builds the call that sends amount (base units) of a CTF
// outcome token from the deposit wallet to another address.
func (c *Client) SendTokenCalls(ctx context.Context, to, tokenID string, amount *big.Int) ([]WalletCall, error) {
	wallet, recipient, err := c.sendParties(to, amount)
	if err != nil {
		return nil, err
	}
	id, err := parseTokenID(tokenID)
	if err != nil {
		return nil, err
	}
	balance, err := c.callUint(ctx, common.HexToAddress(ConditionalTokensAddress), balanceOfSelector, word(wallet), uintWord(id))
	if err != nil {
		return nil, fmt.Errorf("read token %s balance: %w", tokenID, err)
	}
	if balance.Cmp(amount) < 0 {
		return nil, fmt.Errorf("deposit wallet holds %s of token %s, cannot send %s", FormatUnits(balance), tokenID, FormatUnits(amount))
	}
	// safeTransferFrom(from, to, id, amount, data="")
	return []WalletCall{walletCall(common.HexToAddress(ConditionalTokensAddress), safeTransferFromSelector,
		word(wallet), word(recipient), uintWord(id), uintWord(amount),
		uintWord(big.NewInt(5*32)), // offset of the empty data
		make([]byte, 32),
	)}, nil
}

// requirePUSD fails unless the wallet holds at least amount pUSD.
func (c *Client) requirePUSD(ctx context.Context, wallet common.Address, amount *big.Int, verb string) error {
	balance, err := c.callUint(ctx, pusd, erc20BalanceSelector, word(wallet))
	if err != nil {
		return fmt.Errorf("read pUSD balance: %w", err)
	}
	if balance.Cmp(amount) < 0 {
		return fmt.Errorf("deposit wallet holds %s pUSD, %s %s", FormatUnits(balance), verb, FormatUnits(amount))
	}
	return nil
}

func (c *Client) walletAndAmount(amount *big.Int, verb string) (common.Address, error) {
	wallet, err := c.depositWallet()
	if err != nil {
		return common.Address{}, err
	}
	if amount == nil || amount.Sign() <= 0 {
		return common.Address{}, fmt.Errorf("%s amount must be positive", verb)
	}
	return wallet, nil
}

func (c *Client) sendParties(to string, amount *big.Int) (common.Address, common.Address, error) {
	wallet, err := c.walletAndAmount(amount, "send")
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	if !common.IsHexAddress(to) {
		return common.Address{}, common.Address{}, fmt.Errorf("invalid recipient %q", to)
	}
	recipient := common.HexToAddress(to)
	if recipient == (common.Address{}) || recipient == wallet {
		return common.Address{}, common.Address{}, fmt.Errorf("recipient %s is the zero address or the wallet itself", recipient.Hex())
	}
	return wallet, recipient, nil
}

// callUint reads a uint256 from a view function.
func (c *Client) callUint(ctx context.Context, contract common.Address, sel []byte, args ...[]byte) (*big.Int, error) {
	if c.cfg.ChainID != ChainPolygonMainnet {
		return nil, fmt.Errorf("contract reads are only configured for Polygon mainnet")
	}
	rpc, err := c.rpcClient(ctx)
	if err != nil {
		return nil, err
	}
	out, err := rpc.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: calldata(sel, args...)}, nil)
	if err != nil {
		return nil, err
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("returned %d bytes", len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

// ParseUnits parses a decimal amount such as "12.5" into 6-decimal base
// units, refusing more precision than that.
func ParseUnits(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	if strings.Trim(value, ".") == "" {
		return nil, fmt.Errorf("an amount is required")
	}
	whole, frac, _ := strings.Cut(value, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 || strings.ContainsAny(whole+frac, "+-") {
		return nil, fmt.Errorf("invalid amount %q: at most 6 decimals, no sign", value)
	}
	out, ok := new(big.Int).SetString(whole+frac+strings.Repeat("0", 6-len(frac)), 10)
	if !ok || out.BitLen() > 256 {
		return nil, fmt.Errorf("invalid amount %q", value)
	}
	return out, nil
}

// FormatUnits renders 6-decimal base units as a decimal string.
func FormatUnits(v *big.Int) string {
	if v == nil {
		return "0"
	}
	if v.Sign() < 0 {
		return "-" + FormatUnits(new(big.Int).Neg(v))
	}
	s := fmt.Sprintf("%07s", v.String())
	whole, frac := s[:len(s)-6], strings.TrimRight(s[len(s)-6:], "0")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

func selector(signature string) []byte { return crypto.Keccak256([]byte(signature))[:4] }

func word(a common.Address) []byte { return common.LeftPadBytes(a.Bytes(), 32) }

// uintWord ABI-encodes a uint256. Callers check the range first (ParseUnits,
// parseTokenID); a wider value would shift every later word, so it panics.
func uintWord(v *big.Int) []byte {
	if v.Sign() < 0 || v.BitLen() > 256 {
		panic(fmt.Sprintf("uintWord: %s is not a uint256", v))
	}
	return common.LeftPadBytes(v.Bytes(), 32)
}

// parseTokenID parses a decimal ERC-1155 token ID.
func parseTokenID(tokenID string) (*big.Int, error) {
	id, ok := new(big.Int).SetString(tokenID, 10)
	if !ok || id.Sign() <= 0 || id.BitLen() > 256 {
		return nil, fmt.Errorf("invalid token ID %q", tokenID)
	}
	return id, nil
}

func walletCall(target common.Address, sel []byte, words ...[]byte) WalletCall {
	return WalletCall{Target: target.Hex(), Value: "0", Data: hexutil.Encode(calldata(sel, words...))}
}

func calldata(sel []byte, words ...[]byte) []byte {
	data := append([]byte{}, sel...)
	for _, w := range words {
		data = append(data, w...)
	}
	return data
}
