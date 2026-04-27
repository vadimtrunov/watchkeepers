package keepclient

import "errors"

// Sentinel errors (stubs — real docs land in the matching feat commit).
var (
	// ErrUnauthorized maps to 401 (stub).
	ErrUnauthorized = errors.New("keepclient: unauthorized")
	// ErrForbidden maps to 403 (stub).
	ErrForbidden = errors.New("keepclient: forbidden")
	// ErrNotFound maps to 404 (stub).
	ErrNotFound = errors.New("keepclient: not found")
	// ErrConflict maps to 409 (stub).
	ErrConflict = errors.New("keepclient: conflict")
	// ErrInvalidRequest maps to 400 (stub).
	ErrInvalidRequest = errors.New("keepclient: invalid request")
	// ErrInternal maps to 5xx (stub).
	ErrInternal = errors.New("keepclient: internal server error")
	// ErrNoTokenSource is returned when /v1/* is called without a token (stub).
	ErrNoTokenSource = errors.New("keepclient: no token source configured")
)

// ServerError is a stub for the typed error envelope.
type ServerError struct {
	Status int
	Code   string
	Reason string
}

// Error is the stub implementation.
func (e *ServerError) Error() string { return "" }

// Unwrap is the stub implementation (always nil — real mapping lands in the
// matching feat commit).
func (e *ServerError) Unwrap() error { return nil }
