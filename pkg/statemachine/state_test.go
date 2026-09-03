package statemachine

import "testing"

func TestHappyPathTransitionsToLiveAndFilled(t *testing.T) {
	state := State("")
	for _, event := range []Event{EventIntentAccepted, EventSigned, EventSubmitStarted, EventSubmitAcknowledged, EventFillObserved} {
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
	state := State("")
	for _, event := range []Event{EventIntentAccepted, EventSigned, EventSubmitStarted, EventSubmitTimedOut} {
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

func TestInvalidTransitionRejectsImmediateCancelFromSubmitting(t *testing.T) {
	if _, _, err := Apply(StateSubmitting, EventCancelObserved); err == nil {
		t.Fatal("expected cancel observation from submitting without live evidence to be invalid")
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
