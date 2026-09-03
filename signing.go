package clobclient

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/polymarket/go-order-utils/pkg/eip712"
)

var (
	v2NameHash               = crypto.Keccak256Hash([]byte("Polymarket CTF Exchange"))
	v2VersionHash            = crypto.Keccak256Hash([]byte("2"))
	v2OrderType              = "Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)"
	v2OrderHash              = crypto.Keccak256Hash([]byte(v2OrderType))
	v2OrderStruct            = []abi.Type{eip712.Bytes32, eip712.Uint256, eip712.Address, eip712.Address, eip712.Uint256, eip712.Uint256, eip712.Uint256, eip712.Uint8, eip712.Uint8, eip712.Uint256, eip712.Bytes32, eip712.Bytes32}
	poly1271TypeHash         = crypto.Keccak256Hash([]byte("TypedDataSign(Order contents,string name,string version,uint256 chainId,address verifyingContract,bytes32 salt)" + v2OrderType))
	depositWalletNameHash    = crypto.Keccak256Hash([]byte("DepositWallet"))
	depositWalletVersionHash = crypto.Keccak256Hash([]byte("1"))
	poly1271Struct           = []abi.Type{eip712.Bytes32, eip712.Bytes32, eip712.Bytes32, eip712.Bytes32, eip712.Uint256, eip712.Address, eip712.Bytes32}
)

func l2Headers(key *ecdsa.PrivateKey, creds *Credentials, now time.Time, method, path string, body []byte) (map[string]string, error) {
	if key == nil || creds == nil || creds.APIKey == "" || creds.Secret == "" || creds.Passphrase == "" {
		return nil, fmt.Errorf("complete API credentials and private key are required")
	}
	ts := now.Unix()
	sig, err := hmacSignature(creds.Secret, ts, method, path, body)
	if err != nil {
		return nil, err
	}
	return map[string]string{"POLY_ADDRESS": crypto.PubkeyToAddress(key.PublicKey).Hex(), "POLY_SIGNATURE": sig, "POLY_TIMESTAMP": strconv.FormatInt(ts, 10), "POLY_API_KEY": creds.APIKey, "POLY_PASSPHRASE": creds.Passphrase}, nil
}

func hmacSignature(secret string, timestamp int64, method, path string, body []byte) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(secret)
	}
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(secret)
	}
	if err != nil {
		return "", fmt.Errorf("decode API secret: %w", err)
	}
	mac := hmac.New(sha256.New, decoded)
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + method + path))
	_, _ = mac.Write(body)
	return base64.URLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (c *Client) signOrder(order UserOrder, tickSize float64, negRisk bool) (SignedOrderV2, error) {
	if c.key == nil {
		return SignedOrderV2{}, fmt.Errorf("private key is required to sign orders")
	}
	if order.TokenID == "" || (order.Side != SideBuy && order.Side != SideSell) || order.Shares <= 0 || math.IsNaN(order.Shares) || math.IsInf(order.Shares, 0) {
		return SignedOrderV2{}, fmt.Errorf("invalid order")
	}
	if tickSize <= 0 {
		return SignedOrderV2{}, fmt.Errorf("invalid tick size")
	}
	price := math.Round(order.Price/tickSize) * tickSize
	if price <= 0 || price >= 1 || math.Abs(price-order.Price) > 1e-9 {
		return SignedOrderV2{}, fmt.Errorf("price %.8f is not aligned to tick size %.8f", order.Price, tickSize)
	}
	makerAmt, takerAmt := orderAmounts(order.Side, price, order.Shares, order.OrderType)
	if makerAmt.Sign() <= 0 || takerAmt.Sign() <= 0 {
		return SignedOrderV2{}, fmt.Errorf("order amounts rounded to zero")
	}
	contract, err := v2Exchange(c.cfg.ChainID, negRisk)
	if err != nil {
		return SignedOrderV2{}, err
	}
	maker := common.HexToAddress(c.cfg.MakerAddress)
	signer := common.HexToAddress(c.signer)
	if maker == (common.Address{}) || signer == (common.Address{}) {
		return SignedOrderV2{}, fmt.Errorf("maker and signer addresses are required")
	}
	salt := c.cfg.Now().UnixNano() / int64(time.Millisecond)
	timestamp := c.cfg.Now().UnixMilli()
	metadata := common.Hash{}.Hex()
	builder := normalizeBytes32(order.Builder)
	domain, err := eip712.BuildEIP712DomainSeparator(v2NameHash, v2VersionHash, big.NewInt(c.cfg.ChainID), contract)
	if err != nil {
		return SignedOrderV2{}, fmt.Errorf("build order domain: %w", err)
	}
	side := uint8(0)
	if order.Side == SideSell {
		side = 1
	}
	values := []interface{}{v2OrderHash, big.NewInt(salt), maker, signer, decimalBig(order.TokenID), makerAmt, takerAmt, side, uint8(c.cfg.SignatureType), big.NewInt(timestamp), common.HexToHash(metadata), common.HexToHash(builder)}
	var signature string
	if c.cfg.SignatureType == SignatureTypePoly1271 {
		signature, err = poly1271Signature(c.key, domain, maker, c.cfg.ChainID, values)
	} else {
		var digest common.Hash
		digest, err = eip712.HashTypedDataV4(domain, v2OrderStruct, values)
		if err == nil {
			var sig []byte
			sig, err = crypto.Sign(digest.Bytes(), c.key)
			if err == nil {
				sig[64] += 27
				signature = hexutil.Encode(sig)
			}
		}
	}
	if err != nil {
		return SignedOrderV2{}, fmt.Errorf("sign V2 order: %w", err)
	}
	expiration := "0"
	if order.Expiration > 0 {
		expiration = strconv.FormatInt(order.Expiration, 10)
	}
	return SignedOrderV2{Salt: salt, Maker: maker.Hex(), Signer: signer.Hex(), TokenID: order.TokenID, MakerAmount: makerAmt.String(), TakerAmount: takerAmt.String(), Expiration: expiration, Side: string(order.Side), SignatureType: int(c.cfg.SignatureType), Timestamp: strconv.FormatInt(timestamp, 10), Metadata: metadata, Builder: builder, Signature: signature}, nil
}

func orderAmounts(side Side, price, shares float64, kind OrderType) (*big.Int, *big.Int) {
	priceRat := decimalRat(price)
	sharesRat := decimalRat(shares)
	usdc := roundScaledRat(new(big.Rat).Mul(priceRat, sharesRat), 1_000_000)
	share := roundScaledRat(sharesRat, 1_000_000)
	if side == SideBuy && (kind == OrderTypeFOK || kind == OrderTypeFAK) {
		floorToMultiple(usdc, 10_000)
		floorToMultiple(share, 100)
	} else {
		floorToMultiple(usdc, 100)
		floorToMultiple(share, 10_000)
	}
	if side == SideBuy {
		return usdc, share
	}
	return share, usdc
}

func decimalRat(value float64) *big.Rat {
	rat, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	if !ok {
		return new(big.Rat)
	}
	return rat
}

func roundScaledRat(value *big.Rat, scale int64) *big.Int {
	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt64(scale))
	num := new(big.Int).Set(scaled.Num())
	den := scaled.Denom()
	quotient, remainder := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(remainder, big.NewInt(2)).Cmp(den) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func floorToMultiple(value *big.Int, multiple int64) {
	divisor := big.NewInt(multiple)
	value.Div(value, divisor).Mul(value, divisor)
}

func decimalBig(value string) *big.Int {
	v, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return big.NewInt(0)
	}
	return v
}
func normalizeBytes32(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return common.Hash{}.Hex()
	}
	return common.HexToHash(raw).Hex()
}
func v2Exchange(chainID int64, negRisk bool) (common.Address, error) {
	if chainID != ChainPolygonMainnet && chainID != ChainPolygonAmoy {
		return common.Address{}, fmt.Errorf("unsupported V2 chain %d", chainID)
	}
	if negRisk {
		return common.HexToAddress("0xe2222d279d744050d28e00520010520000310F59"), nil
	}
	return common.HexToAddress("0xE111180000d2663C0091e4f400237545B87B996B"), nil
}

func poly1271Signature(key *ecdsa.PrivateKey, domain common.Hash, maker common.Address, chainID int64, orderValues []interface{}) (string, error) {
	encodedOrder, err := eip712.Encode(v2OrderStruct, orderValues)
	if err != nil {
		return "", err
	}
	contentsHash := crypto.Keccak256Hash(encodedOrder)
	typedValues := []interface{}{poly1271TypeHash, contentsHash, depositWalletNameHash, depositWalletVersionHash, big.NewInt(chainID), maker, common.Hash{}}
	encodedTyped, err := eip712.Encode(poly1271Struct, typedValues)
	if err != nil {
		return "", err
	}
	typedHash := crypto.Keccak256Hash(encodedTyped)
	digest := crypto.Keccak256Hash([]byte("\x19\x01"), domain.Bytes(), typedHash.Bytes())
	inner, err := crypto.Sign(digest.Bytes(), key)
	if err != nil {
		return "", err
	}
	inner[64] += 27
	contentsType := hexutil.Encode([]byte(v2OrderType))[2:]
	return "0x" + hexutil.Encode(inner)[2:] + hexutil.Encode(domain.Bytes())[2:] + hexutil.Encode(contentsHash.Bytes())[2:] + contentsType + fmt.Sprintf("%04x", len(v2OrderType)), nil
}
