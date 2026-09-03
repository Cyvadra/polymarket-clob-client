package clobclient

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func NormalizeExecutionStatus(raw string) (ExecutionStatus, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "LIVE":
		return ExecutionLive, true
	case "MATCHED":
		return ExecutionMatched, true
	case "CANCELED", "CANCELLED", "EXPIRED", "FAILED":
		return ExecutionCanceled, true
	case "DELAYED":
		return ExecutionDelayed, true
	default:
		return ExecutionUnknown, false
	}
}

func IsTerminalExecutionStatus(status ExecutionStatus) bool {
	return status == ExecutionMatched || status == ExecutionCanceled
}

func ExecutionFromOrder(orderID string, order *Order, requestedShares float64, now time.Time) (Execution, error) {
	if order == nil {
		return Execution{}, fmt.Errorf("order is required")
	}
	if requestedShares <= 0 || math.IsNaN(requestedShares) || math.IsInf(requestedShares, 0) {
		return Execution{}, fmt.Errorf("requested shares must be finite and positive")
	}
	status, ok := NormalizeExecutionStatus(order.Status)
	exec := Execution{OrderID: orderID, Status: status, RequestedShares: requestedShares, Terminal: IsTerminalExecutionStatus(status), UpdatedAt: now}
	if !ok {
		return exec, fmt.Errorf("unsupported order status %q", order.Status)
	}
	if order.SizeMatched != "" {
		matched, err := strconv.ParseFloat(order.SizeMatched, 64)
		if err != nil {
			return exec, fmt.Errorf("parse size_matched: %w", err)
		}
		if matched < 0 || matched > requestedShares || math.IsNaN(matched) || math.IsInf(matched, 0) {
			return exec, fmt.Errorf("invalid size_matched %q", order.SizeMatched)
		}
		exec.MatchedShares = matched
	}
	if order.AvgPrice != "" {
		average, err := strconv.ParseFloat(order.AvgPrice, 64)
		if err != nil {
			return exec, fmt.Errorf("parse avg_price: %w", err)
		}
		if average < 0 || average >= 1 || math.IsNaN(average) || math.IsInf(average, 0) {
			return exec, fmt.Errorf("invalid avg_price %q", order.AvgPrice)
		}
		exec.AveragePrice = average
	}
	if exec.MatchedShares > 0 && exec.AveragePrice <= 0 {
		return exec, fmt.Errorf("avg_price is required for matched shares")
	}
	return exec, nil
}
