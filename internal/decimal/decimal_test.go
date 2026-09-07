package decimal

import "testing"

// Real share sizes are rarely representable exactly in binary. Flooring to the
// exchange precision first keeps the float64 short enough to round-trip.
func TestFloorSharesFloatAcceptsOrdinaryDecimalPositions(t *testing.T) {
	for _, tc := range []struct {
		value  string
		digits int
		want   float64
	}{
		{"12.34", 2, 12.34},
		{"12.3456", 2, 12.34},
		{"0.1", 2, 0.1},
		{"3.3", 2, 3.3},
		{"2", 2, 2},
		{"7.259", 4, 7.259},
	} {
		got, err := FloorSharesFloat(tc.value, tc.digits)
		if err != nil || got != tc.want {
			t.Fatalf("FloorSharesFloat(%q, %d) = %v, %v; want %v", tc.value, tc.digits, got, err, tc.want)
		}
	}
}

func TestFloorSharesFloatRejectsNonPositiveAndUnparsable(t *testing.T) {
	for _, value := range []string{"0", "-1", "", "abc", "0.001"} {
		if _, err := FloorSharesFloat(value, 2); err == nil {
			t.Fatalf("expected FloorSharesFloat(%q, 2) to fail", value)
		}
	}
}

func TestFloorToTruncatesTowardZero(t *testing.T) {
	for _, tc := range []struct {
		value  string
		digits int
		want   string
	}{
		{"1.999", 2, "1.99"},
		{"1.001", 2, "1.00"},
		{"5", 0, "5"},
		{"2.3255", 4, "2.3255"},
	} {
		got, ok := FloorTo(tc.value, tc.digits)
		if !ok || got != tc.want {
			t.Fatalf("FloorTo(%q, %d) = %q, %v; want %q", tc.value, tc.digits, got, ok, tc.want)
		}
	}
	if _, ok := FloorTo("0.004", 2); ok {
		t.Fatal("expected a value that floors to zero to be rejected")
	}
}

// Signing rejects any price that is not an exact tick multiple, so alignment
// must not leave binary residue behind.
func TestTickAlignmentLandsOnTheGrid(t *testing.T) {
	for _, tc := range []struct {
		price, tick, floor, ceil float64
	}{
		{0.545, 0.01, 0.54, 0.55},
		{0.43, 0.01, 0.43, 0.43},
		{0.07, 0.01, 0.07, 0.07},
		{0.4321, 0.001, 0.432, 0.433},
	} {
		if got := FloorToTick(tc.price, tc.tick); got != tc.floor {
			t.Fatalf("FloorToTick(%v, %v) = %v; want %v", tc.price, tc.tick, got, tc.floor)
		}
		if got := CeilToTick(tc.price, tc.tick); got != tc.ceil {
			t.Fatalf("CeilToTick(%v, %v) = %v; want %v", tc.price, tc.tick, got, tc.ceil)
		}
	}
}

func TestTickAlignmentIsIdentityWithoutATick(t *testing.T) {
	if got := FloorToTick(0.545, 0); got != 0.545 {
		t.Fatalf("expected an unknown tick to leave the price alone, got %v", got)
	}
}

func TestDivideAndRoundDown(t *testing.T) {
	if got, ok := DivideAndRoundDown("1", "0.43", 4); !ok || got != "2.3255" {
		t.Fatalf("DivideAndRoundDown = %q, %v", got, ok)
	}
	if got, ok := DivideAndRoundDown("1", "0.43", 2); !ok || got != "2.32" {
		t.Fatalf("DivideAndRoundDown = %q, %v", got, ok)
	}
	if _, ok := DivideAndRoundDown("0.00001", "0.99", 4); ok {
		t.Fatal("expected a quotient below precision to be rejected")
	}
	if _, ok := DivideAndRoundDown("1", "0", 4); ok {
		t.Fatal("expected division by zero to be rejected")
	}
}

func TestCompareOrdersDecimalStringsNumerically(t *testing.T) {
	if Compare("2.50", "2.5") != 0 {
		t.Fatal("expected numerically equal values to compare equal")
	}
	if Compare("10", "9.9") <= 0 {
		t.Fatal("expected 10 to sort above 9.9")
	}
	if Compare("0.1", "0.2") >= 0 {
		t.Fatal("expected 0.1 to sort below 0.2")
	}
}

func TestPriceRejectsValuesOutsideTheOpenUnitInterval(t *testing.T) {
	for _, value := range []string{"0", "1", "-0.5", "1.5", "", "abc"} {
		if _, err := Price(value); err == nil {
			t.Fatalf("expected Price(%q) to fail", value)
		}
	}
	if got, err := Price("0.42"); err != nil || got != 0.42 {
		t.Fatalf("Price(0.42) = %v, %v", got, err)
	}
}

func TestPositiveComparesNumericallyNotTextually(t *testing.T) {
	for _, value := range []string{"0", "0.0", "0.0000", "-1", ""} {
		if Positive(value) {
			t.Fatalf("expected %q to be non-positive", value)
		}
	}
	if !Positive("0.0001") {
		t.Fatal("expected 0.0001 to be positive")
	}
}
