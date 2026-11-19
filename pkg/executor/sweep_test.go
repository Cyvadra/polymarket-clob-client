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
func (l listedOrders) Market(_ context.Context, id string) (*clobclient.Market, error) {
	return &clobclient.Market{ConditionID: id, Slug: "btc-updown-5m-1"}, nil
}

type slugLister struct {
	orders  []clobclient.Order
	slugs   map[string]string
	listed  int
	lookups map[string]int
}

func (l *slugLister) AllOpenOrders(context.Context) ([]clobclient.Order, error) {
	l.listed++
	return l.orders, nil
}

func (l *slugLister) Market(_ context.Context, id string) (*clobclient.Market, error) {
	if l.lookups == nil {
		l.lookups = map[string]int{}
	}
	l.lookups[id]++
	slug, ok := l.slugs[id]
	if !ok {
		return nil, errors.New("no such market")
	}
	return &clobclient.Market{ConditionID: id, Slug: slug}, nil
}

var btcSweep = UnknownOrderSweep{MarketPrefixes: []string{"btc-updown-5m"}}

func TestSweepLeavesOtherMarketsAlone(t *testing.T) {
	storer := &knownOrdersStore{fakeStore: &fakeStore{}, known: map[string]bool{}}
	client := &sweepCLOB{fakeCLOB: &fakeCLOB{}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	exec.SetErrorHandler(func(error) {})
	lister := &slugLister{
		orders: []clobclient.Order{{ID: "a", Market: "btc"}, {ID: "b", Market: "btc"}, {ID: "manual", Market: "election"}, {ID: "lost", Market: "gone"}},
		slugs:  map[string]string{"btc": "btc-updown-5m-1763500000", "election": "us-election-2028"},
	}
	err = exec.SweepUnknownOrders(context.Background(), lister, btcSweep)
	if !reflect.DeepEqual(client.canceled, []string{"a", "b"}) {
		t.Fatalf("canceled %v, want only the btc orders", client.canceled)
	}
	if err == nil {
		t.Fatal("want the unresolved market reported")
	}
	if lister.lookups["btc"] != 1 {
		t.Fatalf("btc looked up %d times, want 1", lister.lookups["btc"])
	}
}

func TestSweepSkippedWhenDisabledOrUnscoped(t *testing.T) {
	for _, opts := range []UnknownOrderSweep{{}, {Disabled: true, MarketPrefixes: []string{"btc"}}} {
		client := &sweepCLOB{fakeCLOB: &fakeCLOB{}}
		exec, err := New(&knownOrdersStore{fakeStore: &fakeStore{}}, client, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		lister := &slugLister{orders: []clobclient.Order{{ID: "x", Market: "btc"}}, slugs: map[string]string{"btc": "btc"}}
		if err := exec.SweepUnknownOrders(context.Background(), lister, opts); err != nil {
			t.Fatal(err)
		}
		if lister.listed != 0 || len(client.canceled) != 0 {
			t.Fatalf("opts %+v: listed %d canceled %v, want nothing", opts, lister.listed, client.canceled)
		}
	}
}

func TestSweepCancelsOnlyOrdersWithoutRows(t *testing.T) {
	storer := &knownOrdersStore{fakeStore: &fakeStore{}, known: map[string]bool{"stored": true}}
	client := &sweepCLOB{fakeCLOB: &fakeCLOB{}, refuse: map[string]bool{"stuck": true}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	exec.SetErrorHandler(func(err error) { reported = append(reported, err) })
	err = exec.SweepUnknownOrders(context.Background(), listedOrders{{ID: "stored"}, {ID: "orphan"}, {ID: "stuck"}}, btcSweep)
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

func TestUnknownOrderSweepMatchesGlobs(t *testing.T) {
	opts := UnknownOrderSweep{MarketPrefixes: []string{"*-updown-*", "sol-5m"}}
	for slug, want := range map[string]bool{
		"btc-updown-5m-1763500000":  true,
		"eth-updown-15m-1763500000": true,
		"sol-5m-x":                  true,
		"us-election-2028":          false,
		"updown-btc":                false,
	} {
		if got := opts.matches(slug); got != want {
			t.Errorf("matches(%q) = %v, want %v", slug, got, want)
		}
	}
	if ValidateMarketPattern("[btc") == nil {
		t.Error("want a malformed pattern rejected")
	}
}
