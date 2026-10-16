package store

import (
	"errors"
	"fmt"
)

// Errors. Every error a store returns wraps one of these (test with errors.Is); most come as
// an *Error carrying the operation and key.
var (
	// ErrNotFound: no object (or transcript) with that key.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists: Create on a key that is taken.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict: Update with a stale resourceVersion; re-read and retry.
	ErrConflict = errors.New("resourceVersion conflict")
	// ErrInvalid: a malformed request (missing kind or name, config kind, bad selector, cursor,
	// field or version, wrong output type).
	ErrInvalid = errors.New("invalid")
	// ErrWatchExpired: the watch fell behind (buffer full) or SinceVersion is older than the
	// history; re-list and watch again.
	ErrWatchExpired = errors.New("watch expired")
	// ErrLockLost: Renew on a lease that expired or was released.
	ErrLockLost = errors.New("lock lost")
	// ErrTooLarge: a transcript over the per-transcript cap.
	ErrTooLarge = errors.New("too large")
)

// Error is a store error on one operation.
type Error struct {
	// Op is the operation: get, list, create, update, delete, watch, lock, renew, transcript.
	Op string
	// Key is the object, when there is one (Kind only for list and watch; Name only for locks
	// and transcripts).
	Key Key
	// Detail says more, when there is more to say.
	Detail string
	Err    error
}

func (e *Error) Error() string {
	s := "store: " + e.Op
	switch {
	case e.Key.Kind != "" && e.Key.Name != "":
		s += " " + e.Key.String()
	case e.Key.Kind != "":
		s += " " + e.Key.Kind
	case e.Key.Name != "":
		s += " " + e.Key.Name
	}
	s += ": " + e.Err.Error()
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf returns an *Error for op on k wrapping err, with a formatted detail.
func Errorf(op string, k Key, err error, format string, args ...any) *Error {
	d := ""
	if format != "" {
		d = fmt.Sprintf(format, args...)
	}
	return &Error{Op: op, Key: k, Detail: d, Err: err}
}
