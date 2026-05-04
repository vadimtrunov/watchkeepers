package lifecycle

import "errors"

// ErrInvalidParams is returned synchronously by [Manager.Spawn],
// [Manager.Retire], [Manager.Health], and [Manager.List] when the caller
// hands in an obviously malformed argument (empty required id, empty
// required ManifestID/LeadHumanID, out-of-range list limit). Match with
// [errors.Is]; no network round-trip is made when this sentinel is
// returned. Callers that want the more specific keepclient sentinels
// (`keepclient.ErrInvalidStatusTransition`, `keepclient.ErrNotFound`,
// `keepclient.ErrConflict`, …) should `errors.Is` against those
// directly — lifecycle wraps them so the chain match works.
var ErrInvalidParams = errors.New("lifecycle: invalid params")
