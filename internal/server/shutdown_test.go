package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/controllers"
)

// Shutdown stops accepting leases: a parked long poll answers 204 at once, and so does a later
// one, without waiting for its timer.
func TestShutdownEndsLeasePolls(t *testing.T) {
	e := newEnv(t)
	agent, tok := e.register("gus", "gus-mbp")
	wait := "60s"
	parked := make(chan *apiclient.LeaseTaskResponse, 1)
	go func() {
		r, err := e.c.LeaseTaskWithResponse(e.ctx, agent, &apiclient.LeaseTaskParams{Wait: &wait}, bearer(tok))
		if err != nil {
			t.Error(err)
		}
		parked <- r
	}()
	e.clk.BlockUntil(1) // the poll's timer: it is parked
	if err := e.srv.Shutdown(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-parked:
		if r == nil || r.StatusCode() != http.StatusNoContent {
			t.Fatalf("parked poll after shutdown: %v", r)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the parked lease poll didn't end on shutdown")
	}
	r, err := e.c.LeaseTaskWithResponse(e.ctx, agent, &apiclient.LeaseTaskParams{Wait: &wait}, bearer(tok))
	if err != nil || r.StatusCode() != http.StatusNoContent {
		t.Fatalf("poll after shutdown: %v %v", err, r.Status())
	}
}

func TestMetricsControllerPanics(t *testing.T) {
	ctrl := controllers.NewController("rule", controllers.ReconcilerFunc(nil), controllers.Options{})
	e := newEnv(t, func(o *Options) { o.Controllers = []*controllers.Controller{ctrl} })
	code, body, _ := get(t, e.srv.OpsHandler(), "/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics %d", code)
	}
	for _, want := range []string{"# TYPE factory_controller_panics_total counter", `factory_controller_panics_total{controller="rule"} 0`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics miss %q:\n%s", want, body)
		}
	}
}
