package controllers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// swapCatalog is a CatalogSnapshotter whose catalog a test swaps.
type swapCatalog struct {
	mu      sync.Mutex
	c       *catalog.Catalog
	changed chan struct{}
}

func newSwapCatalog(c *catalog.Catalog) *swapCatalog {
	return &swapCatalog{c: c, changed: make(chan struct{})}
}

func (s *swapCatalog) Current() *catalog.Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

func (s *swapCatalog) Snapshot() (*catalog.Catalog, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c, s.changed
}

func (s *swapCatalog) swap(c *catalog.Catalog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c = c
	close(s.changed)
	s.changed = make(chan struct{})
}

type providerEnv struct {
	clk     *clock.Fake
	st      *memory.Store
	cat     *swapCatalog
	adapter *fake.Adapter
	pc      *ProviderController
}

func newProviderEnv(t *testing.T, adapters ...provider.Adapter) *providerEnv {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &providerEnv{clk: clk, st: memory.New(store.Options{Clock: clk, Logger: discardLogger()}), cat: newSwapCatalog(loadPoC(t))}
	if adapters == nil {
		e.adapter = fake.NewAdapter(fake.Options{Clock: clk})
		adapters = []provider.Adapter{e.adapter}
	}
	reg, err := provider.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	e.pc, err = NewProviderController(e.st, e.cat, reg, ProviderOptions{Clock: clk, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *providerEnv) delivery(t *testing.T, id string) *Delivery {
	t.Helper()
	var d Delivery
	if err := e.st.Get(context.Background(), store.Key{Kind: KindDelivery, Name: id}, &d); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		t.Fatal(err)
	}
	return &d
}

func TestProviderConnectUnknownType(t *testing.T) {
	e := newProviderEnv(t, []provider.Adapter{}...)
	err := e.pc.Connect(context.Background())
	if !errors.Is(err, provider.ErrUnknownType) || !strings.Contains(err.Error(), "github-public") ||
		!strings.Contains(err.Error(), "registered adapter types: []") {
		t.Fatalf("Connect = %v, want an unknown-type error naming the provider and the registered types", err)
	}
	if _, ok := e.pc.Conn(ghProvider); ok {
		t.Error("a connection after a failed Connect")
	}
}

func TestProviderIngressAndNormalisation(t *testing.T) {
	ctx := context.Background()
	e := newProviderEnv(t)
	if err := e.pc.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	conn, ok := e.pc.Conn(ghProvider)
	if !ok {
		t.Fatal("no connection for github-public")
	}

	// The webhook route: stored before the ack, a duplicate is 200.
	h := conn.Handler(e.pc)
	body := `{"delivery":"7","type":"issue.opened","sender":{"login":"gus"},"object":{"kind":"issue","ref":"compliance-framework/docs#42"}}`
	for i, want := range []int{http.StatusAccepted, http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/github-public", strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("delivery %d: %d, want %d", i, rec.Code, want)
		}
	}
	d := e.delivery(t, "fake-7")
	if d == nil || d.Status.Phase != DeliveryReceived || len(d.Spec.Body) == 0 {
		t.Fatalf("stored delivery %+v", d)
	}

	// Normalised into its Event; the raw body is dropped.
	if _, err := e.pc.Reconcile(ctx, "fake-7"); err != nil {
		t.Fatal(err)
	}
	d = e.delivery(t, "fake-7")
	if d.Status.Phase != DeliveryNormalised || len(d.Status.Events) != 1 || d.Status.Events[0] != "fake-7/0" || d.Spec.Body != nil || d.Spec.Headers != nil {
		t.Fatalf("normalised delivery %+v", d)
	}
	var ev v1alpha1.Event
	if err := e.st.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: "fake-7/0"}, &ev); err != nil || ev.Type != v1alpha1.EventIssueOpened {
		t.Fatalf("event: %v %+v", err, ev)
	}
	// Reconciling again changes nothing; after the retention the record goes.
	if res, err := e.pc.Reconcile(ctx, "fake-7"); err != nil || res.RequeueAfter != DefaultDeliveryRetention {
		t.Fatalf("second reconcile: %+v %v", res, err)
	}
	e.clk.Advance(DefaultDeliveryRetention)
	if _, err := e.pc.Reconcile(ctx, "fake-7"); err != nil {
		t.Fatal(err)
	}
	if d := e.delivery(t, "fake-7"); d != nil {
		t.Fatalf("delivery kept past its retention: %+v", d)
	}

	// An invalid delivery fails once and is not retried.
	if err := e.pc.Accept(ctx, provider.RawEvent{ID: "fake-bad", Provider: ghProvider, Body: []byte(`{"type":"nope"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pc.Reconcile(ctx, "fake-bad"); err != nil {
		t.Fatalf("invalid delivery: %v, want no retry", err)
	}
	if d := e.delivery(t, "fake-bad"); d.Status.Phase != DeliveryFailed || d.Status.Reason == "" {
		t.Fatalf("invalid delivery %+v", d)
	}
	// A delivery for a provider with no connection is retried.
	if err := e.pc.Accept(ctx, provider.RawEvent{ID: "x-1", Provider: "nowhere", Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pc.Reconcile(ctx, "x-1"); err == nil {
		t.Fatal("a delivery without a connection was handled")
	}
	if err := e.pc.Accept(ctx, provider.RawEvent{Provider: ghProvider}); !errors.Is(err, provider.ErrInvalid) {
		t.Fatalf("Accept without an id = %v", err)
	}
}

func TestProviderRecover(t *testing.T) {
	ctx := context.Background()
	e := newProviderEnv(t)
	gh := e.adapter.Conn(ghProvider)
	if _, err := gh.Undelivered(fake.Webhook{Delivery: "9", Type: v1alpha1.EventIssueOpened,
		Object: &v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: docsRepo + "#9"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.pc.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.pc.Recover(ctx, t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d := e.delivery(t, "fake-9"); d == nil {
		t.Fatal("the undelivered webhook wasn't recovered")
	}
	// Recovering again skips what the store has.
	if err := e.pc.Recover(ctx, t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestProviderFollowsTheCatalog(t *testing.T) {
	ctx := context.Background()
	e := newProviderEnv(t)
	if err := e.pc.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	stop := runInBackground(t, e.pc.Run)
	if n := e.adapter.Connects(); n != 1 {
		t.Fatalf("connects = %d", n)
	}

	// An unchanged Provider keeps its connection; a changed one reconnects.
	same := loadPoC(t)
	e.cat.swap(same)
	changed := loadPoC(t)
	changed.Providers[ghProvider].Spec.Trust = nil
	e.cat.swap(changed)
	eventually(t, "the changed Provider to reconnect", func() bool { return e.adapter.Connects() == 2 })

	// A removed Provider is dropped.
	removed := loadPoC(t)
	delete(removed.Providers, ghProvider)
	e.cat.swap(removed)
	eventually(t, "the removed Provider to go", func() bool {
		_, ok := e.pc.Conn(ghProvider)
		return !ok
	})
	if err := stop(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProviderSinkStoresBeforeAck(t *testing.T) {
	ctx := context.Background()
	e := newProviderEnv(t)
	if err := e.pc.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	conn, _ := e.pc.Conn(ghProvider)
	// A delivery the store already has is a duplicate: 200, nothing stored again.
	if err := e.st.Create(ctx, &Delivery{TypeMeta: v1alpha1.TypeMeta{Kind: KindDelivery}, Metadata: v1alpha1.ObjectMeta{Name: "fake-1"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	conn.Handler(e.pc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"delivery":"1","type":"issue.opened"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a delivery already stored: %d, want 200", rec.Code)
	}
}
