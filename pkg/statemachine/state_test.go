package statemachine

import "testing"

func TestHappyPathTransitionsToLiveAndFilled(t *testing.T) {
	state := StateIntentReceived
	for _, event := range []Event{EventSigned, EventSubmitStarted, EventSubmitAcknowledged, EventFillObserved} {
		transition, changed, err := Apply(state, event)
		if err != nil {
			t.Fatalf("apply %s from %s: %v", event, state, err)
		}
		if !changed {
			t.Fatalf("expected %s from %s to change state", event, state)
		}
		state = transition.To
	}
	if state != StateFilled || !IsTerminal(state) {
		t.Fatalf("expected filled terminal state, got %s", state)
	}
}

func TestSubmitUnknownKeepsExposureLockedUntilResolved(t *testing.T) {
	state := StateIntentReceived
	for _, event := range []Event{EventSigned, EventSubmitStarted, EventSubmitTimedOut} {
		transition, _, err := Apply(state, event)
		if err != nil {
			t.Fatalf("apply %s from %s: %v", event, state, err)
		}
		state = transition.To
	}
	if state != StateSubmitUnknown {
		t.Fatalf("expected submit unknown, got %s", state)
	}
	if !RequiresLockedExposure(state) {
		t.Fatal("submit unknown must keep exposure locked")
	}

	transition, changed, err := Apply(state, EventOrderLiveObserved)
	if err != nil {
		t.Fatalf("resolve submit unknown: %v", err)
	}
	if !changed || transition.To != StateLive {
		t.Fatalf("expected submit unknown to resolve live, got %+v changed=%v", transition, changed)
	}
}

func TestCancelPendingDoesNotReleaseExposure(t *testing.T) {
	state := StateLive
	for _, event := range []Event{EventCancelRequested, EventCancelAccepted} {
		transition, _, err := Apply(state, event)
		if err != nil {
			t.Fatalf("apply %s from %s: %v", event, state, err)
		}
		state = transition.To
	}
	if state != StateCancelPending {
		t.Fatalf("expected cancel pending, got %s", state)
	}
	if !RequiresLockedExposure(state) {
		t.Fatal("cancel pending must keep exposure locked")
	}
}

func TestTerminalObservationsResolveSubmitting(t *testing.T) {
	for _, tc := range []struct {
		event Event
		state State
	}{
		{EventCancelObserved, StateCanceled},
		{EventExpiredObserved, StateExpired},
		{EventFailedObserved, StateFailed},
	} {
		transition, changed, err := Apply(StateSubmitting, tc.event)
		if err != nil {
			t.Fatalf("apply %s from submitting: %v", tc.event, err)
		}
		if !changed || transition.To != tc.state {
			t.Fatalf("expected %s, got %+v changed=%v", tc.state, transition, changed)
		}
	}
}

func TestLiveObservationDuringCancelKeepsCancelState(t *testing.T) {
	for _, state := range []State{StateCancelRequested, StateCancelPending} {
		transition, changed, err := Apply(state, EventOrderLiveObserved)
		if err != nil {
			t.Fatalf("apply live observation from %s: %v", state, err)
		}
		if changed || transition.To != state {
			t.Fatalf("expected %s to remain unchanged, got %+v changed=%v", state, transition, changed)
		}
	}
}

func TestOrderObservationTreatsMatchedLiveOrderAsPartial(t *testing.T) {
	event, ok := EventForOrderObservation("LIVE", "2", "5", false)
	if !ok || event != EventPartialFillObserved {
		t.Fatalf("expected partial fill event, got %q ok=%v", event, ok)
	}
	event, ok = EventForOrderObservation("UNMATCHED", "0", "5", false)
	if !ok || event != EventCancelObserved {
		t.Fatalf("expected cancel event, got %q ok=%v", event, ok)
	}
	event, ok = EventForOrderObservation("MATCHED", "2.0", "2", false)
	if !ok || event != EventFillObserved {
		t.Fatalf("expected filled event, got %q ok=%v", event, ok)
	}
}

// A zero matched size may arrive with any number of decimal places.
func TestOrderObservationComparesMatchedSizeNumerically(t *testing.T) {
	event, ok := EventForOrderObservation("LIVE", "0.0000", "5", false)
	if !ok || event != EventOrderLiveObserved {
		t.Fatalf("expected a zero match to stay live, got %q ok=%v", event, ok)
	}
}

// An immediate order cannot rest, so its unmatched remainder is gone. Leaving
// it partially filled would keep the child open and its reservation locked.
func TestImmediateOrderPartialMatchIsTerminal(t *testing.T) {
	event, ok := EventForOrderObservation("MATCHED", "2", "5", true)
	if !ok || event != EventCancelObserved {
		t.Fatalf("expected an immediate partial match to be terminal, got %q ok=%v", event, ok)
	}
	event, ok = EventForOrderObservation("PARTIALLY_FILLED", "2", "5", true)
	if !ok || event != EventCancelObserved {
		t.Fatalf("expected an immediate partial fill to be terminal, got %q ok=%v", event, ok)
	}
	event, ok = EventForOrderObservation("MATCHED", "5", "5", true)
	if !ok || event != EventFillObserved {
		t.Fatalf("expected a fully matched immediate order to be filled, got %q ok=%v", event, ok)
	}
}

func TestImmediateRecognizesNonRestingTimeInForce(t *testing.T) {
	for _, timeInForce := range []string{"FOK", "FAK", "fak"} {
		if !Immediate(timeInForce) {
			t.Fatalf("expected %s to be immediate", timeInForce)
		}
	}
	for _, timeInForce := range []string{"GTC", "GTD", ""} {
		if Immediate(timeInForce) {
			t.Fatalf("expected %s to rest on the book", timeInForce)
		}
	}
}

func TestDuplicateTerminalObservationIsIdempotent(t *testing.T) {
	transition, changed, err := Apply(StateCanceled, EventCancelObserved)
	if err != nil {
		t.Fatalf("duplicate terminal observation: %v", err)
	}
	if changed || transition.To != StateCanceled {
		t.Fatalf("expected idempotent canceled observation, got %+v changed=%v", transition, changed)
	}
}
