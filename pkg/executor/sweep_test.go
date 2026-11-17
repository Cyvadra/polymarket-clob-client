package executor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type knownOrdersStore struct {
	*fakeStore
	known map[string]bool
}

func (s *knownOrdersStore) OrderByExchangeID(_ context.Context, id string) (store.SignedOrderRecord, error) {
	if s.known[id] {
		return store.SignedOrderRecord{ExchangeOrderID: id}, nil
	}
	return store.SignedOrderRecord{}, store.ErrNotFound
}

type sweepCLOB struct {
	*fakeCLOB
	refuse   map[string]bool
	canceled []string
}

func (c *sweepCLOB) CancelOrdersResult(_ context.Context, ids []string) (*clobclient.CancelResult, error) {
	result := &clobclient.CancelResult{NotCanceled: map[string]string{}}
	for _, id := range ids {
		if c.refuse[id] {
			result.NotCanceled[id] = "matched"
			continue
		}
		c.canceled = append(c.canceled, id)
		result.Canceled = append(result.Canceled, id)
	}
	return result, nil
}

type listedOrders []clobclient.Order

func (l listedOrders) AllOpenOrders(context.Context) ([]clobclient.Order, error) { return l, nil }

func TestSweepCancelsOnlyOrdersWithoutRows(t *testing.T) {
	storer := &knownOrdersStore{fakeStore: &fakeStore{}, known: map[string]bool{"stored": true}}
	client := &sweepCLOB{fakeCLOB: &fakeCLOB{}, refuse: map[string]bool{"stuck": true}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	exec.SetErrorHandler(func(err error) { reported = append(reported, err) })
	err = exec.SweepUnknownOrders(context.Background(), listedOrders{{ID: "stored"}, {ID: "orphan"}, {ID: "stuck"}})
	if !reflect.DeepEqual(client.canceled, []string{"orphan"}) {
		t.Fatalf("canceled %v, want only the orphan", client.canceled)
	}
	if !errors.Is(err, ErrOrphanAlert) {
		t.Fatalf("err = %v, want the uncancelled orphan reported", err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrOrphanAlert) {
		t.Fatalf("reported = %v, want the cancelled orphan", reported)
	}
}
