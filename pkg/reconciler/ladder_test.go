package reconciler

import (
	"context"
	"net/http"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// ladderAwareStore is fakeStore that can also read an intent's children, as the
// Postgres store can: the reconciler then recognises a ladder's levels.
type ladderAwareStore struct {
	*fakeStore
	ladder bool
}

func (s ladderAwareStore) Intent(context.Context, string) (store.OrderIntentRecord, error) {
	policy := []byte(`{"style":"LIMIT"}`)
	if s.ladder {
		policy = []byte(`{"style":"MULTI_LEVEL_LIMIT"}`)
	}
	return store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentOpen, Policy: policy}, nil
}
func (s ladderAwareStore) IntentOrders(context.Context, string) ([]store.SignedOrderRecord, error) {
	return nil, nil
}
func (s ladderAwareStore) LadderPhase(context.Context, string) (store.LadderPhase, error) {
	return store.LadderActive, nil
}
func (s ladderAwareStore) MoveLadderPhase(context.Context, string, store.LadderPhase, ...store.LadderPhase) (bool, error) {
	return false, nil
}
func (s ladderAwareStore) OrderAveragePrice(context.Context, string) (string, error) {
	return "", store.ErrNotFound
}

func reconcileOne(t *testing.T, order store.SignedOrderRecord, ladder bool, now time.Time) (*fakeStore, error) {
	t.Helper()
	repository := &fakeStore{orders: []store.SignedOrderRecord{order}}
	client := &fakeCLOB{err: &clobclient.APIError{StatusCode: http.StatusNotFound}}
	reconciler, err := New(ladderAwareStore{repository, ladder}, client, nil, "", func() time.Time { return now }, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return repository, reconciler.Reconcile(context.Background())
}

func TestReconcileLeavesAParkedLadderLevelAlone(t *testing.T) {
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 3, ExchangeOrderID: "refused-order", State: statemachine.StateRetryPending, Revision: 4, UpdatedAt: time.Unix(100, 0).UTC()}
	// A long-parked level too: the exchange never held it, so a lookup would
	// only ever fail.
	repository, err := reconcileOne(t, order, true, time.Unix(100000, 0).UTC())
	if err != nil {
		t.Fatalf("reconcile reported %v for a parked level, want nothing to look up", err)
	}
	if len(repository.updates) != 0 {
		t.Fatalf("updates=%#v, want the parked level untouched", repository.updates)
	}
}

func TestReconcileGivesAReppostingLadderLevelAGrace(t *testing.T) {
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 3, ExchangeOrderID: "order-1", State: statemachine.StateSubmitting, Revision: 5, UpdatedAt: time.Unix(100, 0).UTC()}
	repository, err := reconcileOne(t, order, true, time.Unix(101, 0).UTC())
	if err != nil || len(repository.updates) != 0 {
		t.Fatalf("err=%v updates=%#v: a level posted a second ago is not lost", err, repository.updates)
	}
	// Stuck for longer than the grace, it is reconciled as any submission is.
	repository, err = reconcileOne(t, order, true, time.Unix(100, 0).Add(submittingGrace+time.Second).UTC())
	if err != nil || len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile {
		t.Fatalf("err=%v updates=%#v, want a stuck ladder level made inconclusive", err, repository.updates)
	}
}

func TestReconcileKeepsTheImmediateReadingForOrdinarySubmissions(t *testing.T) {
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateSubmitting, Revision: 2, UpdatedAt: time.Unix(100, 0).UTC()}
	repository, err := reconcileOne(t, order, false, time.Unix(101, 0).UTC())
	if err != nil || len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile {
		t.Fatalf("err=%v updates=%#v, want an ordinary submission read as before", err, repository.updates)
	}
}
