package provider

import (
	"errors"
	"fmt"
	"time"
)

// Errors returned by adapters and sinks; match them with errors.Is.
var (
	// ErrDuplicateDelivery is returned by an IngressSink for a delivery it already stored.
	ErrDuplicateDelivery = errors.New("provider: duplicate delivery")
	// ErrNotFound is an object, team or group that doesn't exist on the provider.
	ErrNotFound = errors.New("provider: not found")
	// ErrInvalid is a request the adapter rejects: a missing idempotency key, an unscoped
	// credential request, a malformed delivery, …
	ErrInvalid = errors.New("provider: invalid request")
	// ErrUnsupported is an op or call this provider type can't do (a git-side effect op, a
	// PR op on Slack, …).
	ErrUnsupported = errors.New("provider: unsupported")
	// ErrRetryable is a transient failure (rate limit, 5xx): the Effector API answers 429 with
	// Retry-After.
	// Adapters return it as a *RetryableError.
	ErrRetryable = errors.New("provider: retryable")
	// ErrUnknownType is a Provider type no registered adapter connects.
	ErrUnknownType = errors.New("provider: unknown provider type")
	// ErrAlreadyRegistered is a second adapter for the same type.
	ErrAlreadyRegistered = errors.New("provider: adapter type already registered")
)

// RetryableError is a transient failure; errors.Is(err, ErrRetryable) is true.
type RetryableError struct {
	// RetryAfter is the provider's hint (Retry-After), or zero.
	RetryAfter time.Duration
	Err        error
}

func (e *RetryableError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("provider: retryable (after %s): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("provider: retryable: %v", e.Err)
}

// Unwrap returns the underlying error.
func (e *RetryableError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrRetryable) true.
func (e *RetryableError) Is(target error) bool { return target == ErrRetryable }
