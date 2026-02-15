package clobclient

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// OrderBuilder handles order creation and signing
type OrderBuilder struct {
	PrivateKey    string
	ChainID       int
	SignatureType SignatureType
	FunderAddress *string
}

// NewOrderBuilder creates a new OrderBuilder
func NewOrderBuilder(
	privateKey string,
	chainID int,
	signatureType SignatureType,
	funderAddress *string,
) *OrderBuilder {
	return &OrderBuilder{
		PrivateKey:    privateKey,
		ChainID:       chainID,
		SignatureType: signatureType,
		FunderAddress: funderAddress,
	}
}

// BuildOrder creates and signs an order
func (b *OrderBuilder) BuildOrder(
	userOrder *UserOrder,
	options *CreateOrderOptions,
) (*SignedOrder, error) {
	// Get address
	address, err := GetAddressFromPrivateKey(b.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Determine maker and signer
	maker := address
	if b.FunderAddress != nil {
		maker = *b.FunderAddress
	}

	// Get rounding config based on tick size
	roundConfig := getRoundConfig(options.TickSize)

	// Calculate amounts
	makerAmount, takerAmount, err := calculateOrderAmounts(
		userOrder.Price,
		userOrder.Size,
		userOrder.Side,
		roundConfig,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate amounts: %w", err)
	}

	// Generate salt
	salt, err := generateSalt()
	if err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	// Set defaults
	taker := "0x0000000000000000000000000000000000000000"
	if userOrder.Taker != nil {
		taker = *userOrder.Taker
	}

	nonce := "0"
	if userOrder.Nonce != nil {
		nonce = strconv.FormatInt(*userOrder.Nonce, 10)
	}

	expiration := "0"
	if userOrder.Expiration != nil {
		expiration = strconv.FormatInt(*userOrder.Expiration, 10)
	}

	feeRateBps := "0"
	if userOrder.FeeRateBps != nil {
		feeRateBps = strconv.Itoa(*userOrder.FeeRateBps)
	}

	// Create order
	order := &SignedOrder{
		Salt:          salt,
		Maker:         maker,
		Signer:        address,
		Taker:         taker,
		TokenID:       userOrder.TokenID,
		MakerAmount:   makerAmount,
		TakerAmount:   takerAmount,
		Expiration:    expiration,
		Nonce:         nonce,
		FeeRateBps:    feeRateBps,
		Side:          userOrder.Side,
		SignatureType: b.SignatureType,
	}

	// Sign the order
	signature, err := BuildOrderSignature(b.ChainID, b.PrivateKey, order, b.SignatureType)
	if err != nil {
		return nil, fmt.Errorf("failed to sign order: %w", err)
	}

	order.Signature = signature

	return order, nil
}

// BuildMarketOrder creates and signs a market order
func (b *OrderBuilder) BuildMarketOrder(
	userMarketOrder *UserMarketOrder,
	options *CreateOrderOptions,
) (*SignedOrder, error) {
	// Convert market order to regular order
	// For market orders, we use the amount to calculate size
	var size float64
	var price float64

	if userMarketOrder.Price != nil {
		price = *userMarketOrder.Price
	} else {
		// If no price specified, use a default based on side
		if userMarketOrder.Side == SideBuy {
			price = 1.0 // Buy at max price
		} else {
			price = 0.01 // Sell at min price
		}
	}

	if userMarketOrder.Side == SideBuy {
		// For BUY: amount is in USDC, calculate token size
		size = userMarketOrder.Amount / price
	} else {
		// For SELL: amount is in tokens
		size = userMarketOrder.Amount
	}

	// Create UserOrder from market order
	userOrder := &UserOrder{
		TokenID:    userMarketOrder.TokenID,
		Price:      price,
		Size:       size,
		Side:       userMarketOrder.Side,
		FeeRateBps: userMarketOrder.FeeRateBps,
		Nonce:      userMarketOrder.Nonce,
		Taker:      userMarketOrder.Taker,
	}

	return b.BuildOrder(userOrder, options)
}

// getRoundConfig returns the rounding configuration for a tick size
func getRoundConfig(tickSize TickSize) RoundConfig {
	switch tickSize {
	case TickSize01:
		return RoundConfig{Price: 1, Size: 1, Amount: 1}
	case TickSize001:
		return RoundConfig{Price: 2, Size: 2, Amount: 2}
	case TickSize0001:
		return RoundConfig{Price: 3, Size: 3, Amount: 3}
	case TickSize00001:
		return RoundConfig{Price: 4, Size: 4, Amount: 4}
	default:
		return RoundConfig{Price: 2, Size: 2, Amount: 2}
	}
}

// calculateOrderAmounts calculates maker and taker amounts for an order
func calculateOrderAmounts(
	price float64,
	size float64,
	side Side,
	roundConfig RoundConfig,
) (string, string, error) {
	// Validate price
	if price <= 0 || price > 1 {
		return "", "", fmt.Errorf("price must be between 0 and 1")
	}

	// Calculate raw amounts based on side
	var rawMakerAmount, rawTakerAmount float64

	if side == SideBuy {
		// BUY: maker gives USDC (price * size), receives tokens (size)
		rawMakerAmount = roundAmount(price*size, roundConfig.Amount)
		rawTakerAmount = roundAmount(size, roundConfig.Size)
	} else {
		// SELL: maker gives tokens (size), receives USDC (price * size)
		rawMakerAmount = roundAmount(size, roundConfig.Size)
		rawTakerAmount = roundAmount(price*size, roundConfig.Amount)
	}

	// Convert to wei (6 decimals for USDC and tokens)
	makerAmountWei := new(big.Int)
	takerAmountWei := new(big.Int)

	makerAmountWei.SetString(fmt.Sprintf("%.0f", rawMakerAmount*1e6), 10)
	takerAmountWei.SetString(fmt.Sprintf("%.0f", rawTakerAmount*1e6), 10)

	return makerAmountWei.String(), takerAmountWei.String(), nil
}

// roundAmount rounds an amount to the specified number of decimal places
func roundAmount(amount float64, decimals int) float64 {
	multiplier := math.Pow(10, float64(decimals))
	return math.Round(amount*multiplier) / multiplier
}

// generateSalt generates a random salt for the order
func generateSalt() (int64, error) {
	max := new(big.Int)
	max.SetString("9223372036854775807", 10) // max int64

	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return 0, err
	}

	return n.Int64(), nil
}

// ValidatePrice validates that a price is within valid range and tick size
func ValidatePrice(price float64, tickSize TickSize) error {
	if price <= 0 || price > 1 {
		return fmt.Errorf("price must be between 0 and 1")
	}

	// Parse tick size
	tickSizeFloat, err := strconv.ParseFloat(string(tickSize), 64)
	if err != nil {
		return fmt.Errorf("invalid tick size: %w", err)
	}

	// Check if price is a multiple of tick size with floating point tolerance
	remainder := math.Mod(price, tickSizeFloat)
	// Use a more generous epsilon based on tick size
	epsilon := tickSizeFloat / 100.0
	if remainder > epsilon && (tickSizeFloat-remainder) > epsilon {
		return fmt.Errorf("price must be a multiple of tick size %s", tickSize)
	}

	return nil
}
