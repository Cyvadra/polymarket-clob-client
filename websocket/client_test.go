package websocket

import "testing"

func TestNewSelectsDefaultURLFromSubscription(t *testing.T) {
	market := New(Config{}, MarketSubscription("asset"))
	if market.cfg.URL != DefaultMarketURL {
		t.Fatalf("market URL = %q", market.cfg.URL)
	}
	user := New(Config{}, UserSubscription(nil, UserCredentials{}))
	if user.cfg.URL != DefaultUserURL {
		t.Fatalf("user URL = %q", user.cfg.URL)
	}
}

func TestNewPreservesConfiguredURL(t *testing.T) {
	client := New(Config{URL: "ws://localhost/example"}, MarketSubscription("asset"))
	if client.cfg.URL != "ws://localhost/example" {
		t.Fatalf("configured URL = %q", client.cfg.URL)
	}
}
