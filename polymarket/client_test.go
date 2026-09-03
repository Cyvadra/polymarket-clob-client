package polymarket

import "testing"

func TestNew(t *testing.T) {
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if client.CLOB == nil || client.Gamma == nil || client.Data == nil {
		t.Fatalf("incomplete client %+v", client)
	}
}
