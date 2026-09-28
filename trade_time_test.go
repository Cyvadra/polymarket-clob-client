package clobclient

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTradeTimeReadsStreamAndRESTFields(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       time.Time
	}{
		{"stream milliseconds", `{"timestamp":"1759049953000"}`, time.UnixMilli(1759049953000).UTC()},
		{"REST seconds as string", `{"match_time":"1759049953"}`, time.Unix(1759049953, 0).UTC()},
		{"REST seconds as number", `{"match_time":1759049953}`, time.Unix(1759049953, 0).UTC()},
		{"neither", `{"match_time":null}`, time.Time{}},
	} {
		var trade Trade
		if err := json.Unmarshal([]byte(tc.body), &trade); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := trade.Time(); !got.Equal(tc.want) {
			t.Fatalf("%s: time %v, want %v", tc.name, got, tc.want)
		}
	}
}
