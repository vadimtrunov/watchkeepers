package keepclient

import (
	"context"
	"errors"
)

// errDoStub is a sentinel returned by the stub do() so RED tests fail with
// a clear message instead of a nil-pointer panic. Replaced by the real
// implementation in the matching feat commit.
var errDoStub = errors.New("keepclient: do() not yet implemented (RED stub)")

// do is the internal request helper (stub — real impl lands in the matching
// feat commit).
func (c *Client) do(_ context.Context, _, _ string, _ any, _ any) error {
	return errDoStub
}

// Health calls GET /health (stub — real impl lands in the matching feat
// commit).
func (c *Client) Health(_ context.Context) error {
	return errDoStub
}
