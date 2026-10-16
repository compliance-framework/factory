package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/controllers"
)

func get(t *testing.T, h http.Handler, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String(), rec.Header()
}

func TestOps(t *testing.T) {
	ready := make(chan struct{})
	e := newEnv(t, func(o *Options) { o.Ready = ready })
	ops := e.srv.OpsHandler()

	if code, body, _ := get(t, ops, "/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz %d %q", code, body)
	}
	// Not ready: the catalog isn't loaded.
	e.status.set(func(s *controllers.CatalogStatus) { s.Ready = false })
	if code, _, h := get(t, ops, "/readyz"); code != http.StatusServiceUnavailable || !strings.HasPrefix(h.Get("Content-Type"), ProblemContentType) {
		t.Errorf("readyz before the catalog: %d", code)
	}
	// Loaded, but the startup rebuild isn't done.
	e.status.set(func(s *controllers.CatalogStatus) { s.Ready = true })
	if code, _, _ := get(t, ops, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before startup: %d", code)
	}
	close(ready)
	if code, body, _ := get(t, ops, "/readyz"); code != http.StatusOK || body != "ready\n" {
		t.Errorf("readyz: %d %q", code, body)
	}

	e.task("tsk_1", nil)
	if r, _ := e.c.ListTasksWithResponse(e.ctx, &apiclient.ListTasksParams{}, bearer(cliToken)); r.StatusCode() != http.StatusOK {
		t.Fatal(r.Status())
	}
	code, body, h := get(t, ops, "/metrics")
	if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics %d %s", code, h.Get("Content-Type"))
	}
	for _, want := range []string{
		"# TYPE factory_tasks gauge", `factory_tasks{phase="Pending"} 1`, `factory_tasks{phase="Running"} 0`,
		`factory_ready 1`, `factory_sse_subscribers 0`, `factory_http_requests_total{code="200"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics miss %q:\n%s", want, body)
		}
	}

	// The ops routes are not on the public handler, nor the API on the ops handler.
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		res, b := e.do(http.MethodGet, p, nil)
		wantProblemReply(t, "public "+p, res, b, http.StatusNotFound, CodeNotFound)
	}
	if code, _, _ := get(t, ops, BasePath+"/auth/device-config"); code != http.StatusNotFound {
		t.Errorf("the API on the ops handler: %d", code)
	}
}

func TestBodyLimits(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.BodyLimit, o.TranscriptLimit = 512, 4096 })
	agent, tok := e.register("gus", "mac1")
	e.task("tsk_1", nil)
	l := e.lease(agent, tok)

	big := []byte(`{"type":"resync-catalog","pad":"` + strings.Repeat("x", 600) + `"}`)
	res, b := e.do(http.MethodPost, BasePath+"/commands", big, withBearer(cliToken))
	wantProblemReply(t, "a body over the limit", res, b, http.StatusRequestEntityTooLarge, CodeTooLarge)
	// Without a Content-Length (chunked), the limit applies while reading.
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.ts.URL+BasePath+"/commands", io.MultiReader(bytes.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cliToken)
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	wantProblem(t, "a chunked body over the limit", resp, body, http.StatusRequestEntityTooLarge, CodeTooLarge)

	// Transcripts have their own, larger limit.
	blob := gzipped(t, strings.Repeat(`{"line":"abcdefghij"}`+"\n", 30)) // compresses well: under 4 KiB…
	pad := append(append([]byte{}, blob...), make([]byte, 1000)...)      // …padded past 512 bytes
	up, err := e.c.UploadTranscriptWithBodyWithResponse(e.ctx, *l.Lease.Run, "application/gzip", bytes.NewReader(pad), bearer(tok))
	if err != nil || up.JSON200 == nil {
		t.Fatalf("a transcript over the body limit but under its own: %v %s %s", err, up.Status(), up.Body)
	}
	up, err = e.c.UploadTranscriptWithBodyWithResponse(e.ctx, *l.Lease.Run, "application/gzip", bytes.NewReader(make([]byte, 5000)), bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a transcript over its limit", up.HTTPResponse, up.Body, http.StatusRequestEntityTooLarge, CodeTooLarge)
	up, err = e.c.UploadTranscriptWithBodyWithResponse(e.ctx, *l.Lease.Run, "application/gzip", strings.NewReader("not gzip"), bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a transcript that isn't gzip", up.HTTPResponse, up.Body, http.StatusBadRequest, CodeInvalid)
}
