package keepclient

import (
	"errors"
	"fmt"
)

// Sentinel errors that map to specific server response shapes. Callers
// match with [errors.Is] (e.g. `errors.Is(err, keepclient.ErrUnauthorized)`)
// rather than comparing error text — error strings are documentation, not
// API.
var (
	// ErrUnauthorized — server returned 401 (missing/invalid/expired token).
	ErrUnauthorized = errors.New("keepclient: unauthorized")
	// ErrForbidden — server returned 403 (token valid but lacks required scope).
	ErrForbidden = errors.New("keepclient: forbidden")
	// ErrNotFound — server returned 404 (resource missing).
	ErrNotFound = errors.New("keepclient: not found")
	// ErrConflict — server returned 409 (e.g. duplicate version_no).
	ErrConflict = errors.New("keepclient: conflict")
	// ErrInvalidRequest — server returned 400 (request shape rejected).
	ErrInvalidRequest = errors.New("keepclient: invalid request")
	// ErrInternal — server returned a 5xx (treat as transient or retryable).
	ErrInternal = errors.New("keepclient: internal server error")
	// ErrNoTokenSource — caller invoked a /v1/* path without configuring
	// [WithTokenSource]. Returned synchronously, before any network round-trip,
	// so a missing token never becomes a stale-token request.
	ErrNoTokenSource = errors.New("keepclient: no token source configured")
)

// ServerError carries the parsed envelope from a non-2xx response. Status is
// always populated; Code and Reason are populated when the body matched the
// `{"error":"<code>","reason":"<reason>"}` shape, else Code is "" and Reason
// holds the raw (truncated) body.
//
// Match with [errors.Is] against the Err* sentinels — Unwrap returns the
// matching sentinel for the table documented on [ServerError.Unwrap].
type ServerError struct {
	// Status is the HTTP status code returned by the server.
	Status int
	// Code is the parsed `error` field from the server envelope (empty
	// when the body was not JSON or the field was absent).
	Code string
	// Reason is the parsed `reason` field, OR the raw response body
	// (truncated to 1 KiB) when the JSON envelope could not be decoded.
	Reason string
}

// Error implements the error interface with a self-describing format that
// includes the status, the parsed code (when present), and the reason. Logs
// of failed requests are useful without additional caller context.
func (e *ServerError) Error() string {
	if e.Code == "" && e.Reason == "" {
		return fmt.Sprintf("keepclient: server error: status=%d", e.Status)
	}
	if e.Code == "" {
		return fmt.Sprintf("keepclient: server error: status=%d reason=%q", e.Status, e.Reason)
	}
	if e.Reason == "" {
		return fmt.Sprintf("keepclient: server error: status=%d code=%q", e.Status, e.Code)
	}
	return fmt.Sprintf("keepclient: server error: status=%d code=%q reason=%q", e.Status, e.Code, e.Reason)
}

// Unwrap maps the response status to one of the package sentinels per the
// AC3 table:
//
//	400        -> ErrInvalidRequest
//	401        -> ErrUnauthorized
//	403        -> ErrForbidden
//	404        -> ErrNotFound
//	409        -> ErrConflict
//	5xx        -> ErrInternal
//	other 4xx  -> nil (generic *ServerError, no sentinel match)
//
// A nil Unwrap means callers can still match the type with [errors.As] but
// `errors.Is(err, ErrSomething)` will return false — a deliberate signal
// that the response did not fit a documented sentinel.
func (e *ServerError) Unwrap() error {
	switch e.Status {
	case 400:
		return ErrInvalidRequest
	case 401:
		return ErrUnauthorized
	case 403:
		return ErrForbidden
	case 404:
		return ErrNotFound
	case 409:
		return ErrConflict
	}
	if e.Status >= 500 && e.Status < 600 {
		return ErrInternal
	}
	return nil
}
