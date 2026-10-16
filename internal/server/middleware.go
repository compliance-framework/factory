package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/compliance-framework/factory/internal/auth"
)

// access is who may call an operation (the spec's security requirements, api-server.md §Auth).
type access int

const (
	// public: no credential (the OAuth routes, device config).
	public access = iota
	// registration: the join token and the owner token headers (checked by the scheduler).
	registration
	// agentOnly: an agent token. Lease and task routes also check the lease's holder.
	agentOnly
	// reader: a UI session of a reader (or admin), or the CLI token.
	reader
	// readerOrAgent: reader, or an agent token (the handler checks the lease).
	readerOrAgent
	// admin: a UI session of an admin (the maintainers Group), or the CLI token.
	admin
	// sessionOnly: a UI session (getMe).
	sessionOnly
)

// operations is the access of every generated operation, by operationId. A test checks it
// covers exactly the spec's operations.
var operations = map[string]access{
	"registerAgent":    registration,
	"listAgents":       reader,
	"updateAgent":      agentOnly,
	"deregisterAgent":  agentOnly,
	"leaseTask":        agentOnly,
	"heartbeatLease":   agentOnly,
	"completeLease":    agentOnly,
	"abandonLease":     agentOnly,
	"listTasks":        reader,
	"getTask":          readerOrAgent,
	"getTaskPrompt":    agentOnly,
	"getTaskContext":   agentOnly,
	"getTranscript":    readerOrAgent,
	"uploadTranscript": agentOnly,
	"createEffect":     agentOnly,
	"listWorkItems":    reader,
	"getWorkItem":      reader,
	"getRun":           reader,
	"listQueues":       reader,
	"listPools":        reader,
	"listProviders":    reader,
	"getCatalog":       reader,
	"getCatalogObject": reader,
	"dryRunCatalog":    reader,
	"postCommand":      admin,
	"streamEvents":     reader,
	"login":            public,
	"authCallback":     public,
	"logout":           public,
	"getMe":            sessionOnly,
	"getDeviceConfig":  public,
}

// Registration headers (the joinToken and ownerToken security schemes).
const (
	HeaderJoinToken  = "X-Factory-Join-Token"
	HeaderOwnerToken = "X-Factory-Owner-Token"
)

type (
	requestKey   struct{}
	requestIDKey struct{}
)

// requestIDFrom is the request's id (X-Request-Id).
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestFrom returns the HTTP request a strict handler serves (cookies, headers).
func requestFrom(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.PrincipalFrom(ctx)
	return p
}

// authorize authenticates the request for acc and puts the principal and the request in its
// context (strict handlers only get the context).
func (s *Server) authorize(acc access) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			ctx := r.Context()
			p, err := s.authenticate(ctx, r, acc)
			if err != nil {
				return err
			}
			if p.Kind != "" {
				ctx = auth.WithPrincipal(ctx, p)
			}
			c.SetRequest(r.WithContext(context.WithValue(ctx, requestKey{}, r)))
			return next(c)
		}
	}
}

func (s *Server) authenticate(ctx context.Context, r *http.Request, acc access) (auth.Principal, error) {
	a := s.d.Auth
	switch acc {
	case public:
		return auth.Principal{}, s.sameOrigin(r)
	case registration:
		if r.Header.Get(HeaderJoinToken) == "" || r.Header.Get(HeaderOwnerToken) == "" {
			return auth.Principal{}, problemf(http.StatusUnauthorized, CodeUnauthenticated,
				"registration needs the %s and %s headers", HeaderJoinToken, HeaderOwnerToken)
		}
		return auth.Principal{}, nil
	case agentOnly:
		return a.Agent(ctx, r)
	case readerOrAgent:
		if strings.HasPrefix(auth.BearerToken(r), "agt_") {
			return a.Agent(ctx, r)
		}
		return s.user(ctx, r, false)
	case reader:
		return s.user(ctx, r, false)
	case admin:
		return s.user(ctx, r, true)
	case sessionOnly:
		u, err := a.Session(ctx, r)
		if err != nil {
			return auth.Principal{}, err
		}
		return auth.Principal{Kind: auth.KindUser, User: u}, nil
	}
	return auth.Principal{}, fmt.Errorf("unknown access %d", acc)
}

// user authenticates the CLI token or a UI session; the session's user must be a reader (or,
// with needAdmin, an admin). A session's writes must come from the public origin.
func (s *Server) user(ctx context.Context, r *http.Request, needAdmin bool) (auth.Principal, error) {
	a := s.d.Auth
	if p, ok := a.CLI(r); ok {
		return p, nil
	}
	if auth.BearerToken(r) != "" {
		return auth.Principal{}, problemf(http.StatusUnauthorized, CodeUnauthenticated, "this route takes a UI session or the CLI token")
	}
	u, err := a.Session(ctx, r)
	if err != nil {
		return auth.Principal{}, err
	}
	if err := s.sameOrigin(r); err != nil {
		return auth.Principal{}, err
	}
	if needAdmin {
		ok, err := a.IsAdmin(ctx, u)
		if err != nil {
			return auth.Principal{}, fmt.Errorf("check admin: %w", err)
		}
		if !ok {
			return auth.Principal{}, problemf(http.StatusForbidden, CodeNotAdmin, "commands need the maintainers Group")
		}
	} else {
		ok, err := a.IsReader(ctx, u)
		if err != nil {
			return auth.Principal{}, fmt.Errorf("check membership: %w", err)
		}
		if !ok {
			return auth.Principal{}, problemf(http.StatusForbidden, CodeNotMember, "not a member of the factory's Groups")
		}
	}
	return auth.Principal{Kind: auth.KindUser, User: u}, nil
}

// sameOrigin refuses a state-changing request sent by another site (CSRF, on top of the
// SameSite=Lax cookies): its Origin must be the public origin (or, without one configured,
// the request's host), and without an Origin, Sec-Fetch-Site must not be cross-site.
func (s *Server) sameOrigin(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		ok := err == nil && u.Host != ""
		if ok && s.origin != "" {
			ok = strings.EqualFold(u.Scheme+"://"+u.Host, s.origin)
		} else if ok {
			ok = strings.EqualFold(u.Host, r.Host)
		}
		if !ok {
			return problemf(http.StatusForbidden, CodeCrossSite, "cross-site request refused")
		}
		return nil
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return problemf(http.StatusForbidden, CodeCrossSite, "cross-site request refused")
	}
	return nil
}

// bodyLimit bounds the request body: over n bytes is a 413 problem, at once when the
// Content-Length says so, else when the handler reads past it.
func bodyLimit(n int64) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			if r.ContentLength > n {
				return problemf(http.StatusRequestEntityTooLarge, CodeTooLarge, "the body is over %d bytes", n)
			}
			r.Body = http.MaxBytesReader(c.Response(), r.Body, n)
			return next(c)
		}
	}
}

// requestID gives every request an id (X-Request-Id, the problems' instance): the caller's,
// when it is a sane token, else a random one.
func (s *Server) requestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := c.Request().Header.Get(echo.HeaderXRequestID)
		if !validRequestID(id) {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return fmt.Errorf("request id: %w", err)
			}
			id = hex.EncodeToString(b[:])
		}
		c.Response().Header().Set(echo.HeaderXRequestID, id)
		r := c.Request()
		c.SetRequest(r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		return next(c)
	}
}

func alnum(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' }

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !alnum(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// accessLog logs one line per request: method, route template, path, status, duration and
// request id. Never bodies or query strings (the OAuth callback carries a code).
func (s *Server) accessLog(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := s.clk.Now()
		err := next(c)
		if err != nil {
			c.Error(err) // render now, so the status is known
		}
		r, res := c.Request(), c.Response()
		s.count(res.Status)
		s.log.InfoContext(r.Context(), "http request",
			"method", r.Method, "route", c.Path(), "path", r.URL.Path, "status", res.Status,
			"bytes", res.Size, "duration", s.clk.Now().Sub(start).Round(time.Microsecond),
			"requestId", res.Header().Get(echo.HeaderXRequestID))
		return nil
	}
}

// recover turns a handler panic into a 500 problem, logging the stack.
func (s *Server) recover(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) (err error) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.log.ErrorContext(c.Request().Context(), "handler panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				err = problemf(http.StatusInternalServerError, CodeInternal, "internal error")
			}
		}()
		return next(c)
	}
}

func (s *Server) count(status int) {
	v, _ := s.requests.LoadOrStore(status, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}
