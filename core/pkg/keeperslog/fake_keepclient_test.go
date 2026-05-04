package keeperslog

import (
	"context"
	"sync"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// fakeKeepClient is the hand-rolled [LocalKeepClient] stand-in used by
// the keeperslog test suite. It records every LogAppend call (request +
// optional context-derived snapshot for assertions) and returns either a
// canned response or an injected error. Pattern mirrors the lifecycle
// fakeKeepClient (M3.2.b) and notebook fakeStore (M2b) — no mocking lib.
type fakeKeepClient struct {
	// resp is the response returned on success. When nil and respErr is
	// also nil, a default {ID: "fake-log-id"} is returned.
	resp *keepclient.LogAppendResponse
	// respErr, when non-nil, is returned from every LogAppend call;
	// resp is ignored.
	respErr error

	mu    sync.Mutex
	calls []keepclient.LogAppendRequest
}

// LogAppend records the call and returns the configured response or
// error. Captures the request value (not pointer) so subsequent
// assertions cannot race a still-mutating caller.
func (f *fakeKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.respErr != nil {
		return nil, f.respErr
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &keepclient.LogAppendResponse{ID: "fake-log-id"}, nil
}

// recordedCalls returns a defensive copy of the recorded request log.
func (f *fakeKeepClient) recordedCalls() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// callCount returns the total number of recorded calls.
func (f *fakeKeepClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
