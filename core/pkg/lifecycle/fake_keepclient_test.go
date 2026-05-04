package lifecycle

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// fakeCallKind names a method on [LocalKeepClient] for the call-sequence
// recorder embedded in [fakeKeepClient]. Tests assert the kind+order to
// pin Spawn's two-step Insert→Update sequence (and that bail-outs do
// NOT touch the unreached method).
type fakeCallKind string

const (
	fakeCallInsert fakeCallKind = "insert"
	fakeCallUpdate fakeCallKind = "update"
	fakeCallGet    fakeCallKind = "get"
	fakeCallList   fakeCallKind = "list"
)

// fakeCall is one entry in [fakeKeepClient]'s call-sequence log. Holds
// just enough to assert the call shape without dragging the whole
// keepclient request struct through every assertion.
type fakeCall struct {
	Kind fakeCallKind
	// InsertReq populated when Kind == fakeCallInsert.
	InsertReq keepclient.InsertWatchkeeperRequest
	// UpdateID / UpdateStatus populated when Kind == fakeCallUpdate.
	UpdateID, UpdateStatus string
	// GetID populated when Kind == fakeCallGet.
	GetID string
	// ListReq populated when Kind == fakeCallList.
	ListReq keepclient.ListWatchkeepersRequest
}

// fakeKeepClient is the hand-rolled [LocalKeepClient] stand-in used by
// the lifecycle test suite. It mirrors the M2b.5 `flakyStore` /
// archive_on_retire_test `fakeStore` patterns — injectable error fields,
// recorded call sequence, no mocking lib.
//
// The fake assigns monotonically increasing ids on Insert (`fake-id-1`,
// `fake-id-2`, …) so the concurrency test can assert uniqueness
// without depending on UUIDs. `nextID` is an atomic.Uint64 so 50
// goroutines can call Spawn in parallel without holding `mu` for the
// whole call duration; the calls slice is still guarded by `mu` so the
// recorder is race-free under the same workload.
type fakeKeepClient struct {
	// insertResp is the response Insert returns when insertErr is nil.
	// Tests that need a specific id set it explicitly; tests that just
	// want "any non-empty id" rely on the monotonic-id fallback below.
	insertResp *keepclient.InsertWatchkeeperResponse
	// insertErr, when non-nil, is returned from every Insert call;
	// insertResp is ignored.
	insertErr error
	// updateErr, when non-nil, is returned from every Update call.
	updateErr error
	// getResp is returned from every successful Get call.
	getResp *keepclient.Watchkeeper
	// getErr, when non-nil, is returned from every Get call; getResp
	// is ignored.
	getErr error
	// listResp is returned from every successful List call.
	listResp *keepclient.ListWatchkeepersResponse
	// listErr, when non-nil, is returned from every List call;
	// listResp is ignored.
	listErr error

	mu     sync.Mutex
	calls  []fakeCall
	nextID atomic.Uint64
}

// recordedCalls returns a defensive copy of the recorded call log.
// Tests inspect the copy so a later assertion does not race a still-
// running goroutine appending to the underlying slice.
func (f *fakeKeepClient) recordedCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// callCount returns the total number of recorded calls. Convenience
// for the "fake records ZERO calls" assertions.
func (f *fakeKeepClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// InsertWatchkeeper records the call and returns the configured
// response or error. When insertErr is nil and insertResp is nil the
// fake assigns a monotonic id ("fake-id-1", "fake-id-2", …) so
// concurrent Spawn callers each receive a unique id without
// per-test setup.
func (f *fakeKeepClient) InsertWatchkeeper(_ context.Context, req keepclient.InsertWatchkeeperRequest) (*keepclient.InsertWatchkeeperResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Kind: fakeCallInsert, InsertReq: req})
	f.mu.Unlock()
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	if f.insertResp != nil {
		return f.insertResp, nil
	}
	n := f.nextID.Add(1)
	return &keepclient.InsertWatchkeeperResponse{ID: "fake-id-" + strconv.FormatUint(n, 10)}, nil
}

// UpdateWatchkeeperStatus records the call and returns the configured
// error.
func (f *fakeKeepClient) UpdateWatchkeeperStatus(_ context.Context, id, status string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Kind: fakeCallUpdate, UpdateID: id, UpdateStatus: status})
	f.mu.Unlock()
	return f.updateErr
}

// GetWatchkeeper records the call and returns the configured response
// or error.
func (f *fakeKeepClient) GetWatchkeeper(_ context.Context, id string) (*keepclient.Watchkeeper, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Kind: fakeCallGet, GetID: id})
	f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResp, nil
}

// ListWatchkeepers records the call and returns the configured
// response or error.
func (f *fakeKeepClient) ListWatchkeepers(_ context.Context, req keepclient.ListWatchkeepersRequest) (*keepclient.ListWatchkeepersResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Kind: fakeCallList, ListReq: req})
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResp, nil
}
