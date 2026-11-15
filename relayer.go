package clobclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// DefaultRelayerHost is Polymarket's relayer, which submits deposit wallet
// batches on chain and pays their gas.
const DefaultRelayerHost = "https://relayer-v2.polymarket.com"

// DepositWalletFactory deploys deposit wallets; a WALLET relayer request is
// addressed to it.
const DepositWalletFactory = "0x00000000000Fb5C9ADea0298D729A0CB3823Cc07"

// RelayerCredentials is a relayer API key, created in Polymarket under
// Settings > API Keys > Relayer API Keys. Address is the account address
// the key was issued to; it defaults to the signing key's address.
type RelayerCredentials struct {
	APIKey  string
	Address string
}

// WalletCall is one call a deposit wallet makes in a batch.
type WalletCall struct {
	Target string `json:"target"`
	// Value is native POL in wei, as a decimal string.
	Value string `json:"value"`
	Data  string `json:"data"`
}

// RelayerTransaction is a relayer transaction's state. State is one of
// STATE_NEW, STATE_EXECUTED, STATE_MINED, STATE_CONFIRMED (done),
// STATE_FAILED and STATE_INVALID (both final failures).
type RelayerTransaction struct {
	ID    string
	Hash  string
	State string
	Error string
}

// Failed reports a final failure; a failed batch spent nothing.
func (t *RelayerTransaction) Failed() bool {
	return t.State == "STATE_FAILED" || t.State == "STATE_INVALID"
}

var (
	batchTypeHash  = crypto.Keccak256Hash([]byte("Batch(address wallet,uint256 nonce,uint256 deadline,Call[] calls)Call(address target,uint256 value,bytes data)"))
	callTypeHash   = crypto.Keccak256Hash([]byte("Call(address target,uint256 value,bytes data)"))
	domainTypeHash = crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
)

// ErrSubmitOutcomeUnknown marks an ExecuteWalletBatch error from the submit
// request itself, other than a 4xx refusal: the relayer may have accepted the
// batch before the answer was lost. Every other ExecuteWalletBatch error
// either happens before anything is sent or is a definite refusal.
var ErrSubmitOutcomeUnknown = errors.New("the relayer may have accepted the batch")

// CheckRelayer reports whether the client can submit wallet batches: a deposit
// wallet, relayer or builder credentials and a valid relayer host. It makes no
// requests.
func (c *Client) CheckRelayer() error {
	if _, err := c.depositWallet(); err != nil {
		return err
	}
	_, _, err := c.relayer()
	return err
}

// ExecuteWalletBatch has the deposit wallet run calls, in order, as one
// transaction submitted through the relayer. It returns once the relayer
// accepted the batch; WaitRelayerTransaction follows it on chain.
func (c *Client) ExecuteWalletBatch(ctx context.Context, calls []WalletCall, metadata string, deadline time.Time) (*RelayerTransaction, error) {
	if len(calls) == 0 {
		return nil, fmt.Errorf("a wallet batch needs at least one call")
	}
	if !deadline.After(c.cfg.Now()) {
		return nil, fmt.Errorf("wallet batch deadline %s is not in the future", deadline.Format(time.RFC3339))
	}
	wallet, err := c.depositWallet()
	if err != nil {
		return nil, err
	}
	relayer, headers, err := c.relayer()
	if err != nil {
		return nil, err
	}
	nonce, err := c.walletNonce(ctx, relayer, headers)
	if err != nil {
		return nil, err
	}
	deadlineUnix := big.NewInt(deadline.Unix())
	signature, err := c.signWalletBatch(wallet, nonce, deadlineUnix, calls)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"type":      "WALLET",
		"from":      c.signer,
		"to":        DepositWalletFactory,
		"nonce":     nonce.String(),
		"signature": signature,
		"metadata":  metadata,
		"depositWalletParams": map[string]any{
			"depositWallet": wallet.Hex(),
			"deadline":      deadlineUnix.String(),
			"calls":         calls,
		},
	}
	var out relayerTx
	// No retry: a resend after a lost answer could not double-spend, since the
	// nonce is spent, but it would report a confusing failure.
	if err := relayer.Do(ctx, transport.Request{Method: "POST", Path: "/submit", Body: body, Headers: headers, NoRetry: true}, &out); err != nil {
		// A 4xx answer is a definite refusal; only a lost answer, a timeout
		// or a server error leaves the outcome open.
		var httpErr *transport.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 && httpErr.StatusCode != http.StatusRequestTimeout {
			return nil, fmt.Errorf("submit wallet batch: relayer refused it: %w", err)
		}
		return nil, fmt.Errorf("submit wallet batch: %w: %w", ErrSubmitOutcomeUnknown, err)
	}
	tx := out.transaction("")
	if tx.ID == "" {
		return nil, fmt.Errorf("relayer accepted the batch without a transaction ID")
	}
	return tx, nil
}

// relayerTx is a relayer transaction as the API spells it; the field names
// have appeared in both camel and snake case.
type relayerTx struct {
	ID         string `json:"transactionID"`
	IDSnake    string `json:"transaction_id"`
	Hash       string `json:"transactionHash"`
	HashSnake  string `json:"transaction_hash"`
	State      string `json:"state"`
	Error      string `json:"errorMsg"`
	ErrorSnake string `json:"error_msg"`
}

func (t relayerTx) transaction(fallbackID string) *RelayerTransaction {
	return &RelayerTransaction{ID: firstNonEmpty(t.ID, t.IDSnake, fallbackID), Hash: firstNonEmpty(t.Hash, t.HashSnake), State: t.State, Error: firstNonEmpty(t.Error, t.ErrorSnake)}
}

// RelayerTransactionStatus reads a relayer transaction's current state.
func (c *Client) RelayerTransactionStatus(ctx context.Context, id string) (*RelayerTransaction, error) {
	relayer, headers, err := c.relayer()
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := relayer.Do(ctx, transport.Request{Method: "GET", Path: "/v1/account/transactions/" + url.PathEscape(id), Headers: headers}, &raw); err != nil {
		return nil, fmt.Errorf("read relayer transaction %s: %w", id, err)
	}
	// Accept both a bare object and a one-element list.
	var list []relayerTx
	if err := json.Unmarshal(raw, &list); err != nil {
		var single relayerTx
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, fmt.Errorf("decode relayer transaction %s: %w", id, err)
		}
		list = []relayerTx{single}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("relayer has no transaction %s", id)
	}
	return list[0].transaction(id), nil
}

// WaitRelayerTransaction polls a relayer transaction until it is confirmed,
// fails, or ctx ends. A failed status read does not end the wait: the batch
// was accepted and may still execute, so it keeps polling and, should ctx
// end first, reports the last read error.
func (c *Client) WaitRelayerTransaction(ctx context.Context, id string, every time.Duration) (*RelayerTransaction, error) {
	if every <= 0 {
		every = 2 * time.Second
	}
	var last *RelayerTransaction
	var readErr error
	for {
		tx, err := c.RelayerTransactionStatus(ctx, id)
		if err == nil {
			last, readErr = tx, nil
			if tx.State == "STATE_CONFIRMED" {
				return tx, nil
			}
			if tx.Failed() {
				return tx, fmt.Errorf("relayer transaction %s ended %s: %s", id, tx.State, tx.Error)
			}
		} else {
			readErr = err
		}
		select {
		case <-ctx.Done():
			if readErr != nil {
				return last, fmt.Errorf("relayer transaction %s state unknown: %w (last read: %v)", id, ctx.Err(), readErr)
			}
			return last, fmt.Errorf("relayer transaction %s still %s: %w", id, last.State, ctx.Err())
		case <-time.After(every):
		}
	}
}

func (c *Client) walletNonce(ctx context.Context, relayer *transport.Client, headers func(string, string, []byte) (http.Header, error)) (*big.Int, error) {
	var out struct {
		Nonce json.RawMessage `json:"nonce"`
	}
	query := url.Values{"address": {c.signer}, "type": {"WALLET"}}
	if err := relayer.Do(ctx, transport.Request{Method: "GET", Path: "/v1/account/transactions/params", Query: query, Headers: headers}, &out); err != nil {
		return nil, fmt.Errorf("read wallet nonce: %w", err)
	}
	nonce, ok := new(big.Int).SetString(strings.Trim(string(out.Nonce), `"`), 10)
	if !ok || nonce.Sign() < 0 {
		return nil, fmt.Errorf("relayer returned nonce %s", out.Nonce)
	}
	return nonce, nil
}

// depositWallet is the configured deposit wallet. Only signature type 3
// wallets execute batches; a Safe goes through a different relayer flow.
func (c *Client) depositWallet() (common.Address, error) {
	if c.key == nil {
		return common.Address{}, fmt.Errorf("a private key is required to sign wallet batches")
	}
	if c.cfg.SignatureType != SignatureTypePoly1271 {
		return common.Address{}, fmt.Errorf("wallet batches need a deposit wallet (signature type 3), not type %d", c.cfg.SignatureType)
	}
	if !common.IsHexAddress(c.cfg.MakerAddress) || strings.EqualFold(c.cfg.MakerAddress, c.signer) {
		return common.Address{}, fmt.Errorf("the maker address must be the deposit wallet")
	}
	return common.HexToAddress(c.cfg.MakerAddress), nil
}

// relayer is the relayer transport with its auth headers: a relayer API key
// when one is configured, otherwise builder credentials.
func (c *Client) relayer() (*transport.Client, func(string, string, []byte) (http.Header, error), error) {
	var headers func(string, string, []byte) (http.Header, error)
	switch {
	case c.cfg.Relayer != nil && c.cfg.Relayer.APIKey != "":
		address := firstNonEmpty(c.cfg.Relayer.Address, c.signer)
		headers = func(string, string, []byte) (http.Header, error) {
			h := make(http.Header)
			h.Set("RELAYER_API_KEY", c.cfg.Relayer.APIKey)
			h.Set("RELAYER_API_KEY_ADDRESS", address)
			return h, nil
		}
	case c.cfg.Builder != nil:
		headers = func(method, path string, body []byte) (http.Header, error) {
			h := make(http.Header)
			return h, addBuilderHeaders(h, c.cfg.Builder, c.cfg.Now(), method, path, body)
		}
	default:
		return nil, nil, fmt.Errorf("the relayer needs POLYMARKET_RELAYER_API_KEY or builder API credentials")
	}
	// The host is checked here, not in New, so a bad relayer setting cannot
	// stop clients that never use the relayer.
	if parsed, err := url.Parse(c.cfg.RelayerHost); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, nil, fmt.Errorf("invalid relayer host %q", c.cfg.RelayerHost)
	}
	c.relayerMu.Lock()
	defer c.relayerMu.Unlock()
	if c.relayerTransport == nil {
		httpClient := c.cfg.HTTPClient
		if httpClient == nil {
			httpClient = transport.NewHTTPClient(c.cfg.ProxyURL, c.cfg.Timeout)
		}
		c.relayerTransport = transport.New(c.cfg.RelayerHost, httpClient, c.cfg.Timeout, c.cfg.Retry.MaxAttempts, c.cfg.Retry.BaseDelay, 0)
	}
	return c.relayerTransport, headers, nil
}

// signWalletBatch signs the EIP-712 Batch the deposit wallet checks in
// execute, returning r||s||v with v in {27, 28}.
func (c *Client) signWalletBatch(wallet common.Address, nonce, deadline *big.Int, calls []WalletCall) (string, error) {
	digest, err := walletBatchDigest(c.cfg.ChainID, wallet, nonce, deadline, calls)
	if err != nil {
		return "", err
	}
	sig, err := crypto.Sign(digest.Bytes(), c.key)
	if err != nil {
		return "", fmt.Errorf("sign wallet batch: %w", err)
	}
	sig[64] += 27
	return hexutil.Encode(sig), nil
}

func walletBatchDigest(chainID int64, wallet common.Address, nonce, deadline *big.Int, calls []WalletCall) (common.Hash, error) {
	domain := crypto.Keccak256Hash(
		domainTypeHash.Bytes(),
		crypto.Keccak256([]byte("DepositWallet")),
		crypto.Keccak256([]byte("1")),
		uintWord(big.NewInt(chainID)),
		word(wallet),
	)
	var callHashes []byte
	for i, call := range calls {
		if !common.IsHexAddress(call.Target) {
			return common.Hash{}, fmt.Errorf("call %d: invalid target %q", i, call.Target)
		}
		value, ok := new(big.Int).SetString(firstNonEmpty(call.Value, "0"), 10)
		if !ok || value.Sign() < 0 {
			return common.Hash{}, fmt.Errorf("call %d: invalid value %q", i, call.Value)
		}
		data, err := hexutil.Decode(call.Data)
		if err != nil {
			return common.Hash{}, fmt.Errorf("call %d: invalid data: %w", i, err)
		}
		callHashes = append(callHashes, crypto.Keccak256(
			callTypeHash.Bytes(),
			word(common.HexToAddress(call.Target)),
			uintWord(value),
			crypto.Keccak256(data),
		)...)
	}
	batch := crypto.Keccak256Hash(
		batchTypeHash.Bytes(),
		word(wallet),
		uintWord(nonce),
		uintWord(deadline),
		crypto.Keccak256(callHashes),
	)
	return crypto.Keccak256Hash([]byte("\x19\x01"), domain.Bytes(), batch.Bytes()), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
