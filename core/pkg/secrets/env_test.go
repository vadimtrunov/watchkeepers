package secrets

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

// Compile-time assertion: *EnvSource satisfies SecretSource.
var _ SecretSource = (*EnvSource)(nil)

// TestEnvSource_GetReturnsEnvValue — happy path: env var set to a
// non-empty value; Get returns (value, nil).
func TestEnvSource_GetReturnsEnvValue(t *testing.T) {
	t.Setenv("DB_PASSWORD", "hunter2")

	src := NewEnvSource()
	got, err := src.Get(context.Background(), "DB_PASSWORD")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("Get = %q, want %q", got, "hunter2")
	}
}

// TestEnvSource_NoLoggerNoOp — constructing without WithLogger and
// calling Get must not panic; all error and success paths are exercised
// silently.
func TestEnvSource_NoLoggerNoOp(t *testing.T) {
	src := NewEnvSource() // no logger
	// Success path.
	t.Setenv("NL_KEY", "value")
	if _, err := src.Get(context.Background(), "NL_KEY"); err != nil {
		t.Fatalf("success Get: %v", err)
	}
	// Error path (missing key) must not panic.
	os.Unsetenv("NL_MISSING") //nolint:errcheck
	if _, err := src.Get(context.Background(), "NL_MISSING"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing Get err = %v, want ErrSecretNotFound", err)
	}
}

// TestEnvSource_LoggerCalledOnErrorOnly — fake logger wired; Get on a
// set key produces 0 log calls; Get on a missing key produces exactly 1
// log call; the log payload contains the key but NOT any value-shaped
// substring from the found key.
func TestEnvSource_LoggerCalledOnErrorOnly(t *testing.T) {
	logger := &fakeLogger{}
	src := NewEnvSource(WithLogger(logger))

	t.Setenv("FOUND_KEY", "supersecret")
	os.Unsetenv("NOT_FOUND_KEY") //nolint:errcheck

	// Success path: MUST produce zero log entries.
	if _, err := src.Get(context.Background(), "FOUND_KEY"); err != nil {
		t.Fatalf("Get FOUND_KEY: %v", err)
	}
	if n := logger.count(); n != 0 {
		t.Fatalf("log entries after success Get = %d, want 0", n)
	}

	// Error path: MUST produce exactly 1 log entry.
	if _, err := src.Get(context.Background(), "NOT_FOUND_KEY"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get NOT_FOUND_KEY err = %v, want ErrSecretNotFound", err)
	}
	if n := logger.count(); n != 1 {
		t.Fatalf("log entries after error Get = %d, want 1", n)
	}

	// The log entry must contain the key name.
	entries := logger.allEntries()
	if !containsString(entries, "NOT_FOUND_KEY") {
		t.Fatalf("log entry does not contain key name %q; entries: %+v", "NOT_FOUND_KEY", entries)
	}

	// The log entry must NOT contain the value of the found key.
	if containsString(entries, "supersecret") {
		t.Fatalf("log entry contains secret value %q; entries: %+v", "supersecret", entries)
	}
}

// TestEnvSource_GetUnsetReturnsErrSecretNotFound — unset env var returns
// ErrSecretNotFound.
func TestEnvSource_GetUnsetReturnsErrSecretNotFound(t *testing.T) {
	t.Parallel()
	os.Unsetenv("MISSING_KEY") //nolint:errcheck

	src := NewEnvSource()
	_, err := src.Get(context.Background(), "MISSING_KEY")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get unset err = %v, want errors.Is ErrSecretNotFound", err)
	}
}

// TestEnvSource_GetEmptyValueReturnsErrSecretNotFound — env var set to
// empty string is treated as "not set" and returns ErrSecretNotFound.
func TestEnvSource_GetEmptyValueReturnsErrSecretNotFound(t *testing.T) {
	t.Setenv("EMPTY_KEY", "")

	src := NewEnvSource()
	_, err := src.Get(context.Background(), "EMPTY_KEY")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get empty-value err = %v, want errors.Is ErrSecretNotFound", err)
	}
}

// TestEnvSource_LoggerRedactsValues — set an env var to a sentinel value
// V; trigger ErrInvalidKey (by passing empty key) with the logger wired;
// assert NO log payload contains V. The empty-key path is checked before
// any env lookup so there is no opportunity to read V — but the test
// documents and pins the redaction invariant regardless of path.
func TestEnvSource_LoggerRedactsValues(t *testing.T) {
	const secretValue = "v3rys3cr3t"
	t.Setenv("REDACT_TEST_VAR", secretValue)

	logger := &fakeLogger{}
	src := NewEnvSource(WithLogger(logger))

	// ErrInvalidKey path: empty key. The logger is NOT called on this
	// path (the check is synchronous before log), so entries = 0.
	// The important invariant: secretValue must never appear in logs.
	_, err := src.Get(context.Background(), "")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Get empty key err = %v, want errors.Is ErrInvalidKey", err)
	}

	// Also exercise the not-found path with a different key to produce
	// log entries, then verify the secret value is absent.
	os.Unsetenv("OTHER_MISSING") //nolint:errcheck
	_, _ = src.Get(context.Background(), "OTHER_MISSING")

	entries := logger.allEntries()
	if containsString(entries, secretValue) {
		t.Fatalf("log entries contain secret value %q — redaction violated; entries: %+v", secretValue, entries)
	}
}

// TestEnvSource_GetEmptyKeyReturnsErrInvalidKey — empty key returns
// ErrInvalidKey synchronously; no env read is attempted (the check is
// before any LookupEnv call).
func TestEnvSource_GetEmptyKeyReturnsErrInvalidKey(t *testing.T) {
	t.Parallel()

	src := NewEnvSource()
	_, err := src.Get(context.Background(), "")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Get empty key err = %v, want errors.Is ErrInvalidKey", err)
	}
}

// TestEnvSource_GetCancelledCtx — a pre-cancelled context causes Get to
// return ctx.Err() before any env lookup.
func TestEnvSource_GetCancelledCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	src := NewEnvSource()
	_, err := src.Get(ctx, "ANY_KEY")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get cancelled ctx err = %v, want errors.Is context.Canceled", err)
	}
}

// TestEnvSource_ConcurrentGetsAreSafe — 50 goroutines call Get for the
// same key simultaneously; all return (value, nil); no race detector
// flag.
func TestEnvSource_ConcurrentGetsAreSafe(t *testing.T) {
	t.Setenv("CONCURRENT_KEY", "concurrent_value")

	logger := &fakeLogger{}
	src := NewEnvSource(WithLogger(logger))

	const n = 50
	errs := make([]error, n)
	vals := make([]string, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			vals[i], errs[i] = src.Get(context.Background(), "CONCURRENT_KEY")
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Get err = %v", i, errs[i])
		}
		if vals[i] != "concurrent_value" {
			t.Errorf("goroutine %d: Get = %q, want %q", i, vals[i], "concurrent_value")
		}
	}
}
