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

func TestExecutionOpenKeepsDecimalValuesAsStrings(t *testing.T) {
	intent := ExecutionOpenRequest{SchemaVersion: SchemaVersionV1, Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: SideBuy, TargetUSD: "12.3456", LimitPrice: "0.42", TimeInForce: TimeInForceGTC, ExpiresAt: time.Unix(12, 0).UTC()}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal execution open request: %v", err)
	}
	body := string(payload)
	if !strings.Contains(body, `"target_usd":"12.3456"`) || !strings.Contains(body, `"limit_price":"0.42"`) {
		t.Fatalf("expected decimal strings in payload, got %s", body)
	}
}

func TestExecutionPolicyTacticsMarshalAsDecimalStrings(t *testing.T) {
	policy := ExecutionPolicy{Style: ExecutionStyleMakerPostOnly, MidPrice: "0.44", InitialPrice: "0.41", MaxPrice: "0.47", PriceStep: "0.01", QuoteMaxAgeMillis: 500}
	payload, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal execution policy: %v", err)
	}
	body := string(payload)
	for _, want := range []string{`"style":"MAKER_POST_ONLY"`, `"mid_price":"0.44"`, `"initial_price":"0.41"`, `"max_price":"0.47"`, `"price_step":"0.01"`, `"quote_max_age_ms":500`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %s in payload, got %s", want, body)
		}
	}
}

func TestCommandSubjectsAreStable(t *testing.T) {
	for want, got := range map[string]string{
		"strategy.execution.open":           SubjectStrategyExecutionOpen,
		"execution.open.result":             SubjectExecutionOpenResult,
		"strategy.execution.close":          SubjectStrategyExecutionClose,
		"execution.close.result":            SubjectExecutionCloseResult,
		"strategy.execution.position.query": SubjectStrategyExecutionPositionQuery,
	} {
		if want != got {
			t.Fatalf("expected subject %q, got %q", want, got)
		}
	}
}

type recordingPublisher struct {
	subject string
	value   any
}

func (p *recordingPublisher) PublishJSON(subject string, value any) error {
	p.subject, p.value = subject, value
	return nil
}

func TestOpenResultPublishSetsSchemaAndSubject(t *testing.T) {
	publisher := &recordingPublisher{}
	result := ExecutionOpenResult{ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: SideBuy, Status: ResultSucceeded, Reason: "done", FilledShares: "1", OccurredAt: time.Unix(5, 0).UTC()}
	if err := PublishExecutionOpenResult(publisher, result); err != nil {
		t.Fatalf("publish open result: %v", err)
	}
	if publisher.subject != SubjectExecutionOpenResult {
		t.Fatalf("expected result subject, got %q", publisher.subject)
	}
	payload, err := json.Marshal(publisher.value)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(payload), `"schema_version":"execution.v1"`) {
		t.Fatalf("expected schema version in result payload, got %s", payload)
	}
}

func TestCloseAndQueryMessagesRoundTrip(t *testing.T) {
	closeReq := ExecutionCloseRequest{SchemaVersion: SchemaVersionV1, Strategy: "strategy", ConditionID: "condition", AssetID: "asset", Outcome: "Up", Mode: ExecutionCloseModeForce}
	payload, err := json.Marshal(closeReq)
	if err != nil {
		t.Fatalf("marshal close request: %v", err)
	}
	var decoded ExecutionCloseRequest
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal close request: %v", err)
	}
	if decoded.Mode != closeReq.Mode || decoded.AssetID != closeReq.AssetID || decoded.ConditionID != closeReq.ConditionID {
		t.Fatalf("close request round trip mismatch: %+v", decoded)
	}

	query := PositionQueryRequest{SchemaVersion: SchemaVersionV1, ConditionID: "condition-a"}
	payload, err = json.Marshal(query)
	if err != nil {
		t.Fatalf("marshal query request: %v", err)
	}
	var decodedQuery PositionQueryRequest
	if err := json.Unmarshal(payload, &decodedQuery); err != nil {
		t.Fatalf("unmarshal query request: %v", err)
	}
	if decodedQuery.ConditionID != query.ConditionID || decodedQuery.MarketID != "" {
		t.Fatalf("query request round trip mismatch: %+v", decodedQuery)
	}
}
