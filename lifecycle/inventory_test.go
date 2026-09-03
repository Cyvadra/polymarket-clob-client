package lifecycle

import (
	"testing"
	"time"
)

func TestInventoryReservesOnlySellableShares(t *testing.T) {
	now := time.Unix(100, 0)
	inventory := NewInventory(func() time.Time { return now })
	if err := inventory.RecordFill("token", 10); err != nil {
		t.Fatal(err)
	}
	if !inventory.ReserveSell("token", 6) {
		t.Fatal("expected initial reservation")
	}
	if inventory.ReserveSell("token", 5) {
		t.Fatal("reservation exceeded available inventory")
	}
	inventory.ConsumeSell("token", 4)
	position, ok := inventory.Position("token")
	if !ok || position.Shares != 6 || position.Reserved != 2 {
		t.Fatalf("unexpected position %+v", position)
	}
}
