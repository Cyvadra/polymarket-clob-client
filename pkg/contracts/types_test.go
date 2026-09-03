package contracts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPositionFeatureEmptyPositionUsesNullEntryFields(t *testing.T) {
	feature := PositionFeature{
		SchemaVersion:     SchemaVersionV1,
		Seq:               7,
		ConditionID:       "condition",
		TokenID:           "token",
		Outcome:           "Up",
		HasPosition:       false,
		SecondsSinceEntry: 0,
		UpdatedAt:         time.Unix(10, 0).UTC(),
		PublishedAt:       time.Unix(11, 0).UTC(),
	}

	payload, err := json.Marshal(feature)
	if err != nil {
		t.Fatalf("marshal position feature: %v", err)
	}
	body := string(payload)
	if !strings.Contains(body, `"entry_price":null`) || !strings.Contains(body, `"entry_time":null`) {
		t.Fatalf("expected null entry fields, got %s", body)
	}
	if strings.Contains(body, "pnl") || strings.Contains(body, "order_status") || strings.Contains(body, "exchange_order_id") {
		t.Fatalf("position feature leaked execution-only fields: %s", body)
	}
}

func TestExecutionIntentKeepsDecimalValuesAsStrings(t *testing.T) {
	intent := ExecutionIntent{
		SchemaVersion:      SchemaVersionV1,
		IntentID:           "intent-1",
		IdempotencyKey:     "strategy:condition:token:1",
		Strategy:           "LateGap",
		ConditionID:        "condition",
		TokenID:            "token",
		Outcome:            "Up",
		Side:               SideBuy,
		TargetShares:       "12.3456",
		LimitPrice:         "0.42",
		TimeInForce:        TimeInForceGTC,
		FeatureCompletedAt: time.Unix(10, 0).UTC(),
		CreatedAt:          time.Unix(11, 0).UTC(),
		ExpiresAt:          time.Unix(12, 0).UTC(),
	}

	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal execution intent: %v", err)
	}
	body := string(payload)
	if !strings.Contains(body, `"target_shares":"12.3456"`) || !strings.Contains(body, `"limit_price":"0.42"`) {
		t.Fatalf("expected decimal strings in payload, got %s", body)
	}
}
