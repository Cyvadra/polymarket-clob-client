package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPositionFeaturesSubject(t *testing.T) {
	subject, err := PositionFeaturesSubject(" condition ", " token ")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}
	if subject != "position.features.condition.token" {
		t.Fatalf("subject=%q", subject)
	}
}

func TestPositionFeaturesSubjectRejectsInvalidTokens(t *testing.T) {
	for _, test := range []struct{ conditionID, tokenID string }{{"", "token"}, {"condition", ""}, {"condition.*", "token"}, {"condition", "token.>"}} {
		if _, err := PositionFeaturesSubject(test.conditionID, test.tokenID); err == nil {
			t.Fatalf("expected invalid subject parts %+v", test)
		}
	}
}

func TestPositionFeatureEmptyPositionUsesNullEntryFields(t *testing.T) {
	feature := PositionFeature{SchemaVersion: SchemaVersionV1, Seq: 7, ConditionID: "condition", TokenID: "token", Outcome: "Up", UpdatedAt: time.Unix(10, 0).UTC(), PublishedAt: time.Unix(11, 0).UTC()}
	payload, err := json.Marshal(feature)
	if err != nil {
		t.Fatalf("marshal position feature: %v", err)
	}
	body := string(payload)
	if !strings.Contains(body, `"entry_price":null`) || !strings.Contains(body, `"entry_time":null`) {
		t.Fatalf("expected null entry fields, got %s", body)
	}
}

func TestExecutionIntentKeepsDecimalValuesAsStrings(t *testing.T) {
	intent := ExecutionIntent{Kind: IntentOpen, SchemaVersion: SchemaVersionV1, IntentID: "intent-1", IdempotencyKey: "strategy:condition:token:1", Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: SideBuy, TargetShares: "12.3456", LimitPrice: "0.42", TimeInForce: TimeInForceGTC, ExpiresAt: time.Unix(12, 0).UTC()}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal execution intent: %v", err)
	}
	body := string(payload)
	if !strings.Contains(body, `"target_shares":"12.3456"`) || !strings.Contains(body, `"limit_price":"0.42"`) {
		t.Fatalf("expected decimal strings in payload, got %s", body)
	}
}

func TestExecutionPolicyTacticsMarshalAsDecimalStrings(t *testing.T) {
	policy := ExecutionPolicy{Style: ExecutionStyleMakerPostOnly, InitialPrice: "0.41", MaxPrice: "0.47", PriceStep: "0.01", RepriceIntervalMillis: 250, MaxReprices: 3, QuoteMaxAgeMillis: 500}
	payload, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal execution policy: %v", err)
	}
	body := string(payload)
	for _, want := range []string{`"style":"MAKER_POST_ONLY"`, `"initial_price":"0.41"`, `"max_price":"0.47"`, `"price_step":"0.01"`, `"reprice_interval_ms":250`, `"max_reprices":3`, `"quote_max_age_ms":500`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %s in payload, got %s", want, body)
		}
	}
}
