package secrets

import (
	"context"
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

// containsString reports whether any KV value in any log entry contains
// the needle as a substring. Used to assert redaction: no log payload
// must contain a secret value.
func containsString(entries []fakeLogEntry, needle string) bool {
	for _, e := range entries {
		if contains(e.Msg, needle) {
			return true
		}
		for _, v := range e.KV {
			if s, ok := v.(string); ok && contains(s, needle) {
				return true
			}
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(substr) > 0 && len(s) >= len(substr) &&
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}()
}
