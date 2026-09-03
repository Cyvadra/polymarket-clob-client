package natsbus

import "testing"

func TestDecodeJSON(t *testing.T) {
	value, err := DecodeJSON[struct {
		ID string `json:"id"`
	}]([]byte(`{"id":"event-1"}`))
	if err != nil || value.ID != "event-1" {
		t.Fatalf("decode JSON: value=%+v err=%v", value, err)
	}
}

func TestNewRequiresURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected missing URL to be rejected")
	}
}
