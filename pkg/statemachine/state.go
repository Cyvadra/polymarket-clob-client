// Package statemachine owns the internal recoverable order lifecycle.
package statemachine

import (
	"fmt"
	"strings"
)

type State string

const (
	StateIntentReceived   State = "INTENT_RECEIVED"
	StateSigned           State = "SIGNED"
	StateSubmitting       State = "SUBMITTING"
	StateSubmitUnknown    State = "SUBMIT_UNKNOWN"
	StateLive             State = "LIVE"
	StatePartiallyFilled  State = "PARTIALLY_FILLED"
	StateCancelRequested  State = "CANCEL_REQUESTED"
	StateCancelPending    State = "CANCEL_PENDING"
	StateCanceled         State = "CANCELED"
	StateFilled           State = "FILLED"
	StateRejected         State = "REJECTED"
	StateExpired          State = "EXPIRED"
	StateFailed           State = "FAILED"
	StateUnknownReconcile State = "UNKNOWN_RECONCILE"
)

type Event string

const (
	EventIntentAccepted        Event = "INTENT_ACCEPTED"
	EventSigned                Event = "SIGNED"
	EventSubmitStarted         Event = "SUBMIT_STARTED"
	EventSubmitAcknowledged    Event = "SUBMIT_ACKNOWLEDGED"
	EventSubmitTimedOut        Event = "SUBMIT_TIMED_OUT"
	EventOrderLiveObserved     Event = "ORDER_LIVE_OBSERVED"
	EventPartialFillObserved   Event = "PARTIAL_FILL_OBSERVED"
	EventFillObserved          Event = "FILL_OBSERVED"
	EventCancelRequested       Event = "CANCEL_REQUESTED"
	EventCancelAccepted        Event = "CANCEL_ACCEPTED"
	EventCancelObserved        Event = "CANCEL_OBSERVED"
	EventRejectedObserved      Event = "REJECTED_OBSERVED"
	EventExpiredObserved       Event = "EXPIRED_OBSERVED"
	EventFailedObserved        Event = "FAILED_OBSERVED"
	EventReconcileInconclusive Event = "RECONCILE_INCONCLUSIVE"
)

type Transition struct {
	From State
	To   State
	By   Event
}

func Apply(current State, event Event) (Transition, bool, error) {
	next, ok := transitions[current][event]
	if !ok {
		if isTerminal(current) && current == terminalObservation(event) {
			return Transition{From: current, To: current, By: event}, false, nil
		}
		return Transition{}, false, fmt.Errorf("invalid transition from %s by %s", current, event)
	}
	return Transition{From: current, To: next, By: event}, next != current, nil
}

func CanApply(current State, event Event) bool {
	_, _, err := Apply(current, event)
	return err == nil
}

// EventForOrderStatus normalizes a Polymarket order status into an internal
// observation event shared by NATS consumers and REST reconciliation.
func EventForOrderStatus(status string) (Event, bool) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "LIVE", "OPEN":
		return EventOrderLiveObserved, true
	case "PARTIALLY_FILLED", "PARTIAL":
		return EventPartialFillObserved, true
	case "FILLED", "MATCHED":
		return EventFillObserved, true
	case "CANCELED", "CANCELLED":
		return EventCancelObserved, true
	case "REJECTED":
		return EventRejectedObserved, true
	case "EXPIRED":
		return EventExpiredObserved, true
	case "FAILED":
		return EventFailedObserved, true
	default:
		return "", false
	}
}

func RequiresLockedExposure(state State) bool {
	switch state {
	case StateSubmitting, StateSubmitUnknown, StateLive, StatePartiallyFilled, StateCancelRequested, StateCancelPending, StateUnknownReconcile:
		return true
	default:
		return false
	}
}

func IsTerminal(state State) bool {
	return isTerminal(state)
}

var transitions = map[State]map[Event]State{
	"": {
		EventIntentAccepted: StateIntentReceived,
	},
	StateIntentReceived: {
		EventSigned:           StateSigned,
		EventRejectedObserved: StateRejected,
	},
	StateSigned: {
		EventSubmitStarted: StateSubmitting,
	},
	StateSubmitting: {
		EventSubmitAcknowledged:    StateLive,
		EventSubmitTimedOut:        StateSubmitUnknown,
		EventOrderLiveObserved:     StateLive,
		EventPartialFillObserved:   StatePartiallyFilled,
		EventFillObserved:          StateFilled,
		EventRejectedObserved:      StateRejected,
		EventReconcileInconclusive: StateUnknownReconcile,
	},
	StateSubmitUnknown: {
		EventSubmitAcknowledged:    StateLive,
		EventOrderLiveObserved:     StateLive,
		EventPartialFillObserved:   StatePartiallyFilled,
		EventFillObserved:          StateFilled,
		EventRejectedObserved:      StateRejected,
		EventReconcileInconclusive: StateUnknownReconcile,
	},
	StateLive: {
		EventOrderLiveObserved:   StateLive,
		EventPartialFillObserved: StatePartiallyFilled,
		EventFillObserved:        StateFilled,
		EventCancelRequested:     StateCancelRequested,
		EventCancelObserved:      StateCanceled,
		EventExpiredObserved:     StateExpired,
		EventFailedObserved:      StateFailed,
	},
	StatePartiallyFilled: {
		EventOrderLiveObserved:   StatePartiallyFilled,
		EventPartialFillObserved: StatePartiallyFilled,
		EventFillObserved:        StateFilled,
		EventCancelRequested:     StateCancelRequested,
		EventCancelObserved:      StateCanceled,
		EventExpiredObserved:     StateExpired,
		EventFailedObserved:      StateFailed,
	},
	StateCancelRequested: {
		EventCancelAccepted:        StateCancelPending,
		EventCancelObserved:        StateCanceled,
		EventPartialFillObserved:   StatePartiallyFilled,
		EventFillObserved:          StateFilled,
		EventReconcileInconclusive: StateUnknownReconcile,
	},
	StateCancelPending: {
		EventCancelObserved:        StateCanceled,
		EventPartialFillObserved:   StatePartiallyFilled,
		EventFillObserved:          StateFilled,
		EventExpiredObserved:       StateExpired,
		EventFailedObserved:        StateFailed,
		EventReconcileInconclusive: StateUnknownReconcile,
	},
	StateUnknownReconcile: {
		EventOrderLiveObserved:   StateLive,
		EventPartialFillObserved: StatePartiallyFilled,
		EventFillObserved:        StateFilled,
		EventCancelObserved:      StateCanceled,
		EventRejectedObserved:    StateRejected,
		EventExpiredObserved:     StateExpired,
		EventFailedObserved:      StateFailed,
	},
	StateCanceled: {
		EventCancelObserved: StateCanceled,
	},
	StateFilled: {
		EventFillObserved: StateFilled,
	},
	StateRejected: {
		EventRejectedObserved: StateRejected,
	},
	StateExpired: {
		EventExpiredObserved: StateExpired,
	},
	StateFailed: {
		EventFailedObserved: StateFailed,
	},
}

func terminalObservation(event Event) State {
	switch event {
	case EventFillObserved:
		return StateFilled
	case EventCancelObserved:
		return StateCanceled
	case EventRejectedObserved:
		return StateRejected
	case EventExpiredObserved:
		return StateExpired
	case EventFailedObserved:
		return StateFailed
	default:
		return ""
	}
}

func isTerminal(state State) bool {
	switch state {
	case StateCanceled, StateFilled, StateRejected, StateExpired, StateFailed:
		return true
	default:
		return false
	}
}
