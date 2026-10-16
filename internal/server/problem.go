package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/auth"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/scheduler"
	"github.com/compliance-framework/factory/internal/store"
)

// ProblemContentType is the media type of every error body (RFC 9457).
const ProblemContentType = "application/problem+json"

// Problem codes (Problem.code), stable for clients.
const (
	CodeUnauthenticated        = "unauthenticated"
	CodeForbidden              = "forbidden"
	CodeNotMember              = "not-a-member"
	CodeNotAdmin               = "not-admin"
	CodeCrossSite              = "cross-site-request"
	CodeNotLeaseHolder         = "not-lease-holder"
	CodeNotYourAgent           = "not-your-agent"
	CodeLeaseGone              = "lease-gone"
	CodeInvalid                = "invalid-request"
	CodeNotFound               = "not-found"
	CodeConflict               = "conflict"
	CodeTooLarge               = "payload-too-large"
	CodeIdempotencyKeyRequired = "idempotency-key-required"
	CodeEffectNotAllowed       = "effect-not-allowed"
	CodeLabelNotAllowed        = "label-not-allowed"
	CodeTargetNotAllowed       = "target-not-allowed"
	CodeRateLimited            = "rate-limited"
	CodeProviderRetryable      = "provider-retryable"
	CodeProviderError          = "provider-error"
	CodeStateMismatch          = "oauth-state-mismatch"
	CodeBadReturnTo            = "bad-return-to"
	CodeOAuth                  = "oauth-failed"
	CodeCatalogNotLoaded       = "catalog-not-loaded"
	CodeUnavailable            = "unavailable"
	CodeTooManySubscribers     = "too-many-subscribers"
	CodeTaskNotActive          = "task-not-active"
	CodeInternal               = "internal"
)

// apiError is an error with its problem: what a handler returns for a known failure.
type apiError struct {
	status int
	code   string
	detail string
	fields []gen.FieldError
	// retryAfter sets Retry-After (429, 503).
	retryAfter time.Duration
	err        error
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("%d %s", e.status, e.code)
	if e.detail != "" {
		s += ": " + e.detail
	}
	if e.err != nil {
		s += ": " + e.err.Error()
	}
	return s
}

func (e *apiError) Unwrap() error { return e.err }

func problemf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, detail: fmt.Sprintf(format, args...)}
}

// toProblem maps any error a handler, a middleware or Echo returns to its problem. Details of
// 5xx errors are not shown to the client (they are logged).
func toProblem(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return &apiError{status: http.StatusRequestEntityTooLarge, code: CodeTooLarge, detail: fmt.Sprintf("the body is over %d bytes", mbe.Limit), err: err}
	}
	var he *echo.HTTPError
	if errors.As(err, &he) {
		code := CodeInvalid
		switch he.Code {
		case http.StatusNotFound:
			code = CodeNotFound
		case http.StatusRequestEntityTooLarge:
			code = CodeTooLarge
		case http.StatusMethodNotAllowed:
			code = "method-not-allowed"
		case http.StatusUnauthorized:
			code = CodeUnauthenticated
		case http.StatusUnsupportedMediaType:
			code = "unsupported-media-type"
		}
		if he.Code >= 500 {
			code = CodeInternal
		}
		return &apiError{status: he.Code, code: code, detail: fmt.Sprint(he.Message), err: err}
	}
	var ve *scheduler.ValidationError
	if errors.As(err, &ve) {
		return &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: ve.Error(),
			fields: []gen.FieldError{{Field: ve.Field, Message: ve.Msg}}, err: err}
	}
	var re *provider.RetryableError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &apiError{status: http.StatusServiceUnavailable, code: CodeUnavailable, err: err}
	case errors.Is(err, auth.ErrStateMismatch):
		return &apiError{status: http.StatusBadRequest, code: CodeStateMismatch, detail: "the login state doesn't match; sign in again", err: err}
	case errors.Is(err, auth.ErrBadReturnTo):
		return &apiError{status: http.StatusBadRequest, code: CodeBadReturnTo, detail: auth.ErrBadReturnTo.Error(), err: err}
	case errors.Is(err, auth.ErrNotMember):
		return &apiError{status: http.StatusForbidden, code: CodeNotMember, detail: "not a member of the factory's Groups", err: err}
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, scheduler.ErrUnauthorized):
		return &apiError{status: http.StatusUnauthorized, code: CodeUnauthenticated, detail: "missing or invalid credentials", err: err}
	case errors.Is(err, auth.ErrOAuth):
		return &apiError{status: http.StatusBadGateway, code: CodeOAuth, err: err}
	case errors.Is(err, scheduler.ErrNotLeaseHolder):
		return &apiError{status: http.StatusForbidden, code: CodeNotLeaseHolder, detail: "the lease belongs to another agent", err: err}
	case errors.Is(err, scheduler.ErrForbidden), errors.Is(err, auth.ErrForbidden):
		return &apiError{status: http.StatusForbidden, code: CodeForbidden, detail: err.Error(), err: err}
	case errors.Is(err, scheduler.ErrLeaseGone):
		return &apiError{status: http.StatusConflict, code: CodeLeaseGone, detail: "the lease has ended: stop the run", err: err}
	case errors.Is(err, controllers.ErrNotLeased):
		return &apiError{status: http.StatusConflict, code: CodeLeaseGone, detail: "the task is not leased", err: err}
	case errors.Is(err, scheduler.ErrInvalid), errors.Is(err, store.ErrInvalid):
		return &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: err.Error(), err: err}
	case errors.Is(err, scheduler.ErrNotFound), errors.Is(err, store.ErrNotFound), errors.Is(err, controllers.ErrUnknownTask):
		return &apiError{status: http.StatusNotFound, code: CodeNotFound, detail: err.Error(), err: err}
	case errors.Is(err, store.ErrAlreadyExists), errors.Is(err, store.ErrConflict):
		return &apiError{status: http.StatusConflict, code: CodeConflict, detail: err.Error(), err: err}
	case errors.Is(err, store.ErrTooLarge):
		return &apiError{status: http.StatusRequestEntityTooLarge, code: CodeTooLarge, detail: err.Error(), err: err}
	case errors.As(err, &re):
		return &apiError{status: http.StatusServiceUnavailable, code: CodeProviderRetryable, retryAfter: re.RetryAfter, err: err}
	}
	return &apiError{status: http.StatusInternalServerError, code: CodeInternal, err: err}
}

// errorHandler is Echo's HTTPErrorHandler: every error becomes application/problem+json.
func (s *Server) errorHandler(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}
	p := toProblem(err)
	r := c.Request()
	if p.status >= 500 {
		s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "route", c.Path(),
			"status", p.status, "code", p.code, "err", err)
	} else {
		s.log.DebugContext(r.Context(), "request refused", "method", r.Method, "route", c.Path(),
			"status", p.status, "code", p.code, "err", err)
	}
	body := gen.Problem{Status: p.status, Title: http.StatusText(p.status), Errors: p.fields}
	if body.Title == "" {
		body.Title = "Error"
	}
	if p.code != "" {
		body.Code = &p.code
	}
	if p.detail != "" && p.status < 500 {
		body.Detail = &p.detail
	}
	if id := c.Response().Header().Get(echo.HeaderXRequestID); id != "" {
		body.Instance = &id
	}
	h := c.Response().Header()
	if p.retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int((p.retryAfter+time.Second-1)/time.Second)))
	}
	h.Set(echo.HeaderContentType, ProblemContentType)
	c.Response().WriteHeader(p.status)
	if r.Method == http.MethodHead {
		return
	}
	if werr := json.NewEncoder(c.Response()).Encode(body); werr != nil {
		s.log.DebugContext(r.Context(), "write problem", "err", werr)
	}
}
