package clobclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSideValues(t *testing.T) {
	assert.Equal(t, Side("BUY"), SideBuy)
	assert.Equal(t, Side("SELL"), SideSell)
}

func TestOrderTypeValues(t *testing.T) {
	assert.Equal(t, OrderType("GTC"), OrderTypeGTC)
	assert.Equal(t, OrderType("FOK"), OrderTypeFOK)
	assert.Equal(t, OrderType("GTD"), OrderTypeGTD)
	assert.Equal(t, OrderType("FAK"), OrderTypeFAK)
}

func TestChainValues(t *testing.T) {
	assert.Equal(t, 137, int(ChainPolygon))
	assert.Equal(t, 80002, int(ChainAmoy))
}

func TestTickSizeValues(t *testing.T) {
	assert.Equal(t, TickSize("0.1"), TickSize01)
	assert.Equal(t, TickSize("0.01"), TickSize001)
	assert.Equal(t, TickSize("0.001"), TickSize0001)
	assert.Equal(t, TickSize("0.0001"), TickSize00001)
}

func TestValidatePrice(t *testing.T) {
	tests := []struct {
		name      string
		price     float64
		tickSize  TickSize
		expectErr bool
	}{
		{"valid price 0.5 with 0.01", 0.50, TickSize001, false},
		{"valid price 0.52 with 0.01", 0.52, TickSize001, false},
		{"valid price 0.523 with 0.001", 0.523, TickSize0001, false},
		{"invalid price > 1", 1.5, TickSize01, true},
		{"invalid price <= 0", 0.0, TickSize01, true},
		{"invalid price < 0", -0.5, TickSize01, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePrice(tt.price, tt.tickSize)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetRoundConfig(t *testing.T) {
	tests := []struct {
		tickSize TickSize
		expected RoundConfig
	}{
		{TickSize01, RoundConfig{Price: 1, Size: 1, Amount: 1}},
		{TickSize001, RoundConfig{Price: 2, Size: 2, Amount: 2}},
		{TickSize0001, RoundConfig{Price: 3, Size: 3, Amount: 3}},
		{TickSize00001, RoundConfig{Price: 4, Size: 4, Amount: 4}},
	}

	for _, tt := range tests {
		t.Run(string(tt.tickSize), func(t *testing.T) {
			config := getRoundConfig(tt.tickSize)
			assert.Equal(t, tt.expected, config)
		})
	}
}

func TestRoundAmount(t *testing.T) {
	tests := []struct {
		name     string
		amount   float64
		decimals int
		expected float64
	}{
		{"round to 1 decimal", 1.2345, 1, 1.2},
		{"round to 2 decimals", 1.2345, 2, 1.23},
		{"round to 3 decimals", 1.2345, 3, 1.235},
		{"round to 4 decimals", 1.2345, 4, 1.2345},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := roundAmount(tt.amount, tt.decimals)
			assert.InDelta(t, tt.expected, result, 0.0001)
		})
	}
}

func TestCalculateOrderAmounts(t *testing.T) {
	roundConfig := RoundConfig{Price: 2, Size: 2, Amount: 2}

	tests := []struct {
		name      string
		price     float64
		size      float64
		side      Side
		expectErr bool
	}{
		{"valid BUY order", 0.52, 10.0, SideBuy, false},
		{"valid SELL order", 0.52, 10.0, SideSell, false},
		{"invalid price > 1", 1.5, 10.0, SideBuy, true},
		{"invalid price <= 0", 0.0, 10.0, SideBuy, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			makerAmount, takerAmount, err := calculateOrderAmounts(
				tt.price,
				tt.size,
				tt.side,
				roundConfig,
			)

			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotEmpty(t, makerAmount)
				assert.NotEmpty(t, takerAmount)
			}
		})
	}
}

func TestGenerateSalt(t *testing.T) {
	salt1, err := generateSalt()
	assert.NoError(t, err)
	assert.Greater(t, salt1, int64(0))

	salt2, err := generateSalt()
	assert.NoError(t, err)
	assert.Greater(t, salt2, int64(0))

	// Salt values should be different (statistically)
	assert.NotEqual(t, salt1, salt2)
}

func TestIsValidAddress(t *testing.T) {
	tests := []struct {
		name     string
		address  string
		expected bool
	}{
		{"valid address", "0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb0", true},
		{"valid address lowercase", "0x742d35cc6634c0532925a3b844bc9e7595f0beb0", true},
		{"invalid address too short", "0x742d35", false},
		{"invalid address no 0x prefix", "742d35Cc6634C0532925a3b844Bc9e7595f0bEb", false},
		{"empty address", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidAddress(tt.address)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNewClobClient(t *testing.T) {
	host := "https://clob.polymarket.com"
	chainID := 137
	privateKey := "0x1234567890123456789012345678901234567890123456789012345678901234"

	client := NewClobClient(
		host,
		chainID,
		privateKey,
		nil,
		SignatureTypeEOA,
		nil,
	)

	assert.NotNil(t, client)
	assert.Equal(t, host, client.Host)
	assert.Equal(t, chainID, client.ChainID)
	assert.Equal(t, privateKey, client.PrivateKey)
	assert.Equal(t, SignatureTypeEOA, client.SignatureType)
	assert.NotNil(t, client.OrderBuilder)
	assert.NotNil(t, client.HTTPClient)
	assert.False(t, client.UseServerTime)
}

func TestNewOrderBuilder(t *testing.T) {
	privateKey := "0x1234567890123456789012345678901234567890123456789012345678901234"
	chainID := 137
	signatureType := SignatureTypeEOA

	builder := NewOrderBuilder(privateKey, chainID, signatureType, nil)

	assert.NotNil(t, builder)
	assert.Equal(t, privateKey, builder.PrivateKey)
	assert.Equal(t, chainID, builder.ChainID)
	assert.Equal(t, signatureType, builder.SignatureType)
	assert.Nil(t, builder.FunderAddress)
}

func TestNewHTTPClient(t *testing.T) {
	client := NewHTTPClient(30, true)

	assert.NotNil(t, client)
	assert.NotNil(t, client.client)
	assert.True(t, client.retryEnabled)
	assert.Equal(t, 3, client.maxRetries)
}
