package secrets

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// fakeLogEntry records one call to [fakeLogger.Log].
type fakeLogEntry struct {
	Msg string
	KV  []any
}

// fakeLogger is the hand-rolled [Logger] stand-in used by the secrets
// test suite. Mirrors the cron package's fakeLogger pattern: mutex-guarded
// entries slice, no mocking library.
type fakeLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

func (l *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, fakeLogEntry{Msg: msg, KV: cp})
}

// count returns the number of recorded log entries.
func (l *fakeLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// allEntries returns a defensive copy of all recorded entries.
func (l *fakeLogger) allEntries() []fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// containsString reports whether any log entry contains needle as a
// substring, checking both Msg and KV values. Used to assert redaction:
// no log payload must contain a secret value.
//
// String-typed KV values are checked directly. As a defense-in-depth
// measure the entire entry is also serialized via fmt.Sprintf so that
// future log calls passing the value as a non-string type ([]byte, error,
// concrete struct, etc.) are caught regardless of kv-value type.
func containsString(entries []fakeLogEntry, needle string) bool {
	for _, e := range entries {
		if strings.Contains(e.Msg, needle) {
			return true
		}
		for _, v := range e.KV {
			if s, ok := v.(string); ok && strings.Contains(s, needle) {
				return true
			}
		}
		// Defense-in-depth: catch leaks via non-string kv-value types.
		if strings.Contains(fmt.Sprintf("%+v", e), needle) {
			return true
		}
	}
	return false
}
