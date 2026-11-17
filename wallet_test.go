package clobclient

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

const testABI = `[
{"name":"approve","type":"function","inputs":[{"type":"address"},{"type":"uint256"}]},
{"name":"transfer","type":"function","inputs":[{"type":"address"},{"type":"uint256"}]},
{"name":"safeTransferFrom","type":"function","inputs":[{"type":"address"},{"type":"address"},{"type":"uint256"},{"type":"uint256"},{"type":"bytes"}]},
{"name":"splitPosition","type":"function","inputs":[{"type":"address"},{"type":"bytes32"},{"type":"bytes32"},{"type":"uint256[]"},{"type":"uint256"}]},
{"name":"mergePositions","type":"function","inputs":[{"type":"address"},{"type":"bytes32"},{"type":"bytes32"},{"type":"uint256[]"},{"type":"uint256"}]},
{"name":"setApprovalForAll","type":"function","inputs":[{"type":"address"},{"type":"bool"}]}
]`

func TestWalletCallEncodingMatchesABI(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(testABI))
	if err != nil {
		t.Fatal(err)
	}
	a := common.HexToAddress("0x1111111111111111111111111111111111111111")
	b := common.HexToAddress("0x2222222222222222222222222222222222222222")
	amount := big.NewInt(12_500_000)
	cond := common.HexToHash("0xab4feb978bc15bc6035b72fa95ca9aaf0de5dbea0ac0fbf21a7cbd92b4460fcf")
	cases := []struct {
		name string
		got  WalletCall
		want func() ([]byte, error)
	}{
		{"approve", walletCall(a, approveSelector, word(b), uintWord(amount)), func() ([]byte, error) { return parsed.Pack("approve", b, amount) }},
		{"transfer", walletCall(a, transferSelector, word(b), uintWord(amount)), func() ([]byte, error) { return parsed.Pack("transfer", b, amount) }},
		{"safeTransferFrom", walletCall(a, safeTransferFromSelector, word(a), word(b), uintWord(big.NewInt(77)), uintWord(amount), uintWord(big.NewInt(160)), make([]byte, 32)),
			func() ([]byte, error) { return parsed.Pack("safeTransferFrom", a, b, big.NewInt(77), amount, []byte{}) }},
		{"mergePositions", binaryPositionCall(a, mergePositionsSelector, cond.Bytes(), amount),
			func() ([]byte, error) {
				return parsed.Pack("mergePositions", common.HexToAddress(PUSDAddress), [32]byte{}, [32]byte(cond), []*big.Int{big.NewInt(1), big.NewInt(2)}, amount)
			}},
		{"setApprovalForAll", walletCall(a, setApprovalForAllSelector, word(b), uintWord(big.NewInt(1))), func() ([]byte, error) { return parsed.Pack("setApprovalForAll", b, true) }},
		{"splitPosition", binaryPositionCall(a, splitPositionSelector, cond.Bytes(), amount),
			func() ([]byte, error) {
				return parsed.Pack("splitPosition", common.HexToAddress(PUSDAddress), [32]byte{}, [32]byte(cond), []*big.Int{big.NewInt(1), big.NewInt(2)}, amount)
			}},
	}
	for _, tc := range cases {
		want, err := tc.want()
		if err != nil {
			t.Fatal(err)
		}
		if got := hexutil.MustDecode(tc.got.Data); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got %x\nwant %x", tc.name, got, want)
		}
	}
}

func TestWalletBatchDigestMatchesTypedData(t *testing.T) {
	wallet := common.HexToAddress("0xc797b62d9480b2613eeA32F47FDe19FF55693760")
	calls := []WalletCall{
		{Target: PUSDAddress, Value: "0", Data: "0x095ea7b3"},
		{Target: CtfCollateralAdapter, Value: "5", Data: "0x"},
	}
	got, err := walletBatchDigest(137, wallet, big.NewInt(9), big.NewInt(1794700000), calls)
	if err != nil {
		t.Fatal(err)
	}
	callMaps := []interface{}{}
	for _, c := range calls {
		callMaps = append(callMaps, map[string]interface{}{"target": c.Target, "value": c.Value, "data": c.Data})
	}
	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {{Name: "name", Type: "string"}, {Name: "version", Type: "string"}, {Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"}},
			"Call":         {{Name: "target", Type: "address"}, {Name: "value", Type: "uint256"}, {Name: "data", Type: "bytes"}},
			"Batch":        {{Name: "wallet", Type: "address"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint256"}, {Name: "calls", Type: "Call[]"}},
		},
		PrimaryType: "Batch",
		Domain:      apitypes.TypedDataDomain{Name: "DepositWallet", Version: "1", ChainId: math.NewHexOrDecimal256(137), VerifyingContract: wallet.Hex()},
		Message:     apitypes.TypedDataMessage{"wallet": wallet.Hex(), "nonce": "9", "deadline": "1794700000", "calls": callMaps},
	}
	want, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("digest %x, want %x", got, want)
	}
}

func TestSignWalletBatchRecovers(t *testing.T) {
	key, _ := crypto.GenerateKey()
	c := &Client{cfg: Config{ChainID: 137}, key: key}
	wallet := common.HexToAddress("0xc797b62d9480b2613eeA32F47FDe19FF55693760")
	calls := []WalletCall{{Target: PUSDAddress, Value: "0", Data: "0x"}}
	sig, err := c.signWalletBatch(wallet, big.NewInt(1), big.NewInt(2), calls)
	if err != nil {
		t.Fatal(err)
	}
	raw := hexutil.MustDecode(sig)
	if raw[64] != 27 && raw[64] != 28 {
		t.Fatalf("v = %d", raw[64])
	}
	raw[64] -= 27
	digest, _ := walletBatchDigest(137, wallet, big.NewInt(1), big.NewInt(2), calls)
	pub, err := crypto.SigToPub(digest.Bytes(), raw)
	if err != nil || crypto.PubkeyToAddress(*pub) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("signature does not recover the signer: %v", err)
	}
}

func TestUnits(t *testing.T) {
	for in, want := range map[string]string{"1": "1000000", "12.5": "12500000", "0.000001": "1", ".5": "500000"} {
		got, err := ParseUnits(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseUnits(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.0000001", "abc", "1e6"} {
		if _, err := ParseUnits(bad); err == nil {
			t.Errorf("ParseUnits(%q) accepted", bad)
		}
	}
	for in, want := range map[int64]string{0: "0", 1: "0.000001", 12_500_000: "12.5", 131_761_105: "131.761105"} {
		if got := FormatUnits(big.NewInt(in)); got != want {
			t.Errorf("FormatUnits(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestUnitsEdgeCases(t *testing.T) {
	if got := FormatUnits(nil); got != "0" {
		t.Fatalf("FormatUnits(nil) = %q", got)
	}
	if got := FormatUnits(big.NewInt(-1500000)); got != "-1.5" {
		t.Fatalf("FormatUnits(-1.5) = %q", got)
	}
	if _, err := ParseUnits(strings.Repeat("9", 80)); err == nil {
		t.Fatal("ParseUnits accepted a value wider than 256 bits")
	}
	if _, err := parseTokenID(new(big.Int).Lsh(big.NewInt(1), 256).String()); err == nil {
		t.Fatal("parseTokenID accepted 2^256")
	}
}
