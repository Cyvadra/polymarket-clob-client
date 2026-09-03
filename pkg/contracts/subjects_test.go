package contracts

import "testing"

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
	for _, test := range []struct {
		conditionID string
		tokenID     string
	}{
		{"", "token"},
		{"condition", ""},
		{"condition.*", "token"},
		{"condition", "token.>"},
	} {
		if _, err := PositionFeaturesSubject(test.conditionID, test.tokenID); err == nil {
			t.Fatalf("expected invalid subject parts %+v", test)
		}
	}
}
