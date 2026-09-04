package protocol

import (
	"fmt"
	"strings"
)

func PositionFeaturesSubject(conditionID, tokenID string) (string, error) {
	conditionID = strings.TrimSpace(conditionID)
	tokenID = strings.TrimSpace(tokenID)
	if conditionID == "" || tokenID == "" {
		return "", fmt.Errorf("condition ID and token ID are required")
	}
	if strings.ContainsAny(conditionID, "*.") || strings.ContainsAny(tokenID, "*.") || strings.Contains(conditionID, ">") || strings.Contains(tokenID, ">") {
		return "", fmt.Errorf("condition ID and token ID cannot contain NATS wildcard tokens")
	}
	return SubjectPositionFeaturesPrefix + "." + conditionID + "." + tokenID, nil
}
