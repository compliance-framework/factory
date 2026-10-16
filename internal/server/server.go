// Package server is the dispatcher's HTTP API (design/workstreams/dispatcher-runtime/LLD/
// api-server.md): the generated Echo strict server (internal/apiserver/gen) implemented over
// the scheduler, the controllers, the store and the provider connections; the webhook routes
// delegated to each provider's Conn.Handler; SSE; and the localhost ops listener (healthz,
// readyz, metrics).
//
// Middleware, in order: request ID → access log (no bodies, no query strings) → recover →
// auth (per operation) → body limit (1 MB; transcripts 20 MB) → handler. Every error is
// rendered as application/problem+json by one HTTPErrorHandler.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/auth"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/scheduler"
	"github.com/compliance-framework/factory/internal/store"
)

// BasePath is where the generated API is mounted.
const BasePath = "/api/v1"

// Defaults for Options.
const (
	DefaultBodyLimit       = 1 << 20  // 1 MB
	DefaultTranscriptLimit = 20 << 20 // 20 MB, the store's per-transcript cap
	DefaultMaxSubscribers  = 100
	DefaultKeepAlive       = 15 * time.Second
	DefaultLeaseWait       = 30 * time.Second
	MaxLeaseWait           = 60 * time.Second
	DefaultPageSize        = 100
	MaxPageSize            = 500
)

// Scheduler is what the server calls for agents and leases (*scheduler.Scheduler).
type Scheduler interface {
	Register(ctx context.Context, req scheduler.RegisterRequest) (*scheduler.Registration, error)
	UpdateAgent(ctx context.Context, agentID string, u scheduler.AgentUpdate) (*v1alpha1.Agent, error)
	Deregister(ctx context.Context, agentID string) error
	Lease(ctx context.Context, agentID string, wait time.Duration) (*scheduler.Grant, error)
	Heartbeat(ctx context.Context, agentID, leaseID string, req scheduler.HeartbeatRequest) (*scheduler.HeartbeatResponse, error)
	Complete(ctx context.Context, agentID, leaseID string, res v1alpha1.Result) (v1alpha1.TaskPhase, error)
	Abandon(ctx context.Context, agentID, leaseID string, req scheduler.AbandonRequest) error
}

// Evaluator is the dry-run: the rule controller's evaluation of an event, with nothing applied
// (*controllers.RuleController).
type Evaluator interface {
	DryRun(ctx context.Context, ev v1alpha1.Event, workItem string) (rules.Evaluation, error)
}

// CatalogStatus reports catalog-sync's status (*controllers.CatalogSync).
type CatalogStatus interface {
	Status() controllers.CatalogStatus
}

// CatalogResyncer is the resync-catalog command (*controllers.CatalogSync).
type CatalogResyncer interface {
	Resync()
}

// KillSwitchWriter sets and clears the pause flags (controllers.StoreKillSwitches).
type KillSwitchWriter interface {
	Pause(ctx context.Context, scope, name, by string, at time.Time) error
	Resume(ctx context.Context, scope, name string) error
}

// Deps are what the server works with.
type Deps struct {
	Store       store.Store
	Transcripts store.TranscriptStore
	Catalog     controllers.CatalogProvider
	// CatalogStatus and Resync are catalog-sync; optional (no status: the catalog is
	// "loaded" once Current is set; no resync: resync-catalog is 503).
	CatalogStatus CatalogStatus
	Resync        CatalogResyncer
	Conns         controllers.ConnSource
	// Sink receives webhook deliveries (the provider controller's sink).
	Sink         provider.IngressSink
	Scheduler    Scheduler
	Factory      *controllers.FactoryProvider
	KillSwitches KillSwitchWriter
	Evaluator    Evaluator
	Auth         *auth.Authenticator
}

// Options configure the server. Zero fields take the defaults.
type Options struct {
	// PublicURL is the dispatcher's public base URL; its origin is the only one cookie-
	// authenticated writes are accepted from. Empty: the request's own host.
	PublicURL string
	// CatalogRepo and CatalogRef describe the catalog source for GET /catalog.
	CatalogRepo string
	CatalogRef  string
	// Ready, when set, must be closed before /readyz answers 200 (the startup rebuild), on top
	// of a loaded catalog.
	Ready <-chan struct{}
	// MaxSubscribers bounds the SSE streams (default 100).
	MaxSubscribers int
	// KeepAlive is the SSE keep-alive interval (default 15 s).
	KeepAlive time.Duration
	// BodyLimit and TranscriptLimit bound request bodies (1 MB, 20 MB).
	BodyLimit       int64
	TranscriptLimit int64
	Clock           clock.Clock
	Logger          *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.MaxSubscribers <= 0 {
		o.MaxSubscribers = DefaultMaxSubscribers
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = DefaultKeepAlive
	}
	if o.BodyLimit <= 0 {
		o.BodyLimit = DefaultBodyLimit
	}
	if o.TranscriptLimit <= 0 {
		o.TranscriptLimit = DefaultTranscriptLimit
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Server is the API server. Handler serves the public API (mount the UI on Echo() as the
// catch-all); OpsHandler serves the ops routes, for the localhost listener only.
type Server struct {
	d      Deps
	opts   Options
	clk    clock.Clock
	log    *slog.Logger
	origin string // the public origin ("" = the request's host)

	e   *echo.Echo
	ops *echo.Echo

	// done is closed by Shutdown: streams end. streamMu orders a stream's start (streams.Add)
	// before Shutdown's Wait.
	done     chan struct{}
	streamMu sync.Mutex
	stopped  bool
	streams  sync.WaitGroup

	subscribers atomic.Int64
	requests    sync.Map // status code (int) → *atomic.Int64
}

var _ gen.StrictServerInterface = (*Server)(nil)

// New returns a server.
func New(d Deps, opts Options) (*Server, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("server: no store")
	case d.Transcripts == nil:
		return nil, errors.New("server: no transcript store")
	case d.Catalog == nil:
		return nil, errors.New("server: no catalog")
	case d.Conns == nil:
		return nil, errors.New("server: no provider connections")
	case d.Sink == nil:
		return nil, errors.New("server: no ingress sink")
	case d.Scheduler == nil:
		return nil, errors.New("server: no scheduler")
	case d.Factory == nil:
		return nil, errors.New("server: no factory provider")
	case d.KillSwitches == nil:
		return nil, errors.New("server: no kill switches")
	case d.Evaluator == nil:
		return nil, errors.New("server: no evaluator")
	case d.Auth == nil:
		return nil, errors.New("server: no authenticator")
	}
	opts = opts.withDefaults()
	s := &Server{
		d: d, opts: opts, clk: opts.Clock, log: opts.Logger.With("component", "server"),
		done: make(chan struct{}),
	}
	if opts.PublicURL != "" {
		u, err := url.Parse(opts.PublicURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errors.New("server: PublicURL must be an absolute URL")
		}
		s.origin = u.Scheme + "://" + u.Host
	}
	s.e = s.newEcho()
	s.registerAPI()
	s.ops = s.newEcho()
	s.registerOps()
	return s, nil
}

// Handler is the public API: /api/v1 and /hooks/{provider}.
func (s *Server) Handler() http.Handler { return s.e }

// Echo is the public Echo instance, for `factory serve` to mount the UI catch-all on.
func (s *Server) Echo() *echo.Echo { return s.e }

// OpsHandler serves /healthz, /readyz and /metrics. Serve it on a localhost listener only
// (`--ops-addr`): the routes have no auth.
func (s *Server) OpsHandler() http.Handler { return s.ops }

// Shutdown ends the SSE streams and waits for them (or ctx). Long polls end with their
// requests; the caller shuts the http.Server down too.
func (s *Server) Shutdown(ctx context.Context) error {
	s.streamMu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.done)
	}
	s.streamMu.Unlock()
	waited := make(chan struct{})
	go func() {
		s.streams.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// beginStream counts a new stream, unless the server is shutting down.
func (s *Server) beginStream() bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.stopped {
		return false
	}
	s.streams.Add(1)
	return true
}

func (s *Server) newEcho() *echo.Echo {
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.HTTPErrorHandler = s.errorHandler
	e.Use(s.requestID, s.accessLog, s.recover)
	return e
}

func (s *Server) registerAPI() {
	mw := make(map[string][]echo.MiddlewareFunc, len(operations))
	for op, acc := range operations {
		limit := s.opts.BodyLimit
		if op == "uploadTranscript" {
			limit = s.opts.TranscriptLimit
		}
		mw[op] = []echo.MiddlewareFunc{s.authorize(acc), bodyLimit(limit)}
	}
	gen.RegisterHandlersWithOptions(s.e, gen.NewStrictHandler(s, nil), gen.RegisterHandlersOptions{
		BaseURL: BasePath, OperationMiddlewares: mw,
	})
	// receiveWebhook: not generated; the provider's handler verifies the signature.
	s.e.POST("/hooks/:provider", s.webhook, bodyLimit(s.opts.BodyLimit))
}

// webhook delegates a delivery to its provider connection's handler (HMAC, then sink.Accept
// before the ack).
func (s *Server) webhook(c echo.Context) error {
	name := c.Param("provider")
	conn, ok := s.d.Conns.Conn(name)
	if !ok {
		return problemf(http.StatusNotFound, CodeNotFound, "no provider %q", name)
	}
	return echo.WrapHandler(conn.Handler(s.d.Sink))(c)
}
