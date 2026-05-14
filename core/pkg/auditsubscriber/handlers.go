package auditsubscriber

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// extractor adapts a bus payload to the `(payload, correlation_id)` pair
// the dispatcher ships to [Writer.Append]. Returns
// [errUnexpectedPayload] (wrapped with the expected vs got Go type
// names) when the bus payload is neither the expected concrete type
// nor a non-nil pointer to it. The wrapped form keeps
// [errors.Is]-matching against the sentinel intact for test
// assertions; the dispatcher itself does NOT return the error to the
// outside world.
type extractor func(event any) (payload any, correlationID string, err error)

// makeExtractor returns the canonical extractor for payload type `T`.
// The generic adapter accepts BOTH a value publish (`Publish(..., ev)`)
// AND a pointer publish (`Publish(..., &ev)`) so a future emitter
// refactor from one to the other does not silently turn into
// log+drop for every audit row on that topic — iter-1 codex M2 lesson:
// the bus surface is untyped (`any`), so the bridge accepts both
// shapes by design rather than relying on every emitter author to
// know the contract.
//
// A nil `*T` is treated as a type-mismatch (no field values to
// surface; no `CorrelationID` to extract).
func makeExtractor[T any](getCorrelationID func(T) string) extractor {
	return func(event any) (any, string, error) {
		switch p := event.(type) {
		case T:
			return p, getCorrelationID(p), nil
		case *T:
			if p == nil {
				var zero T
				return nil, "", typeMismatch(zero, event)
			}
			return *p, getCorrelationID(*p), nil
		default:
			var zero T
			return nil, "", typeMismatch(zero, event)
		}
	}
}

// extractSourceSynced is the [toolregistry.SourceSynced] adapter.
var extractSourceSynced = makeExtractor(func(p toolregistry.SourceSynced) string { return p.CorrelationID })

// extractSourceFailed is the [toolregistry.SourceFailed] adapter.
var extractSourceFailed = makeExtractor(func(p toolregistry.SourceFailed) string { return p.CorrelationID })

// extractToolShadowed is the [toolregistry.ToolShadowed] adapter. The
// payload's `Message()` method is NOT invoked here — the audit row
// preserves the structured fields; downstream rendering is a UI
// concern.
var extractToolShadowed = makeExtractor(func(p toolregistry.ToolShadowed) string { return p.CorrelationID })

// extractToolProposed is the [approval.ToolProposed] adapter.
var extractToolProposed = makeExtractor(func(p approval.ToolProposed) string { return p.CorrelationID })

// extractToolApproved is the [approval.ToolApproved] adapter.
var extractToolApproved = makeExtractor(func(p approval.ToolApproved) string { return p.CorrelationID })

// extractToolRejected is the [approval.ToolRejected] adapter.
var extractToolRejected = makeExtractor(func(p approval.ToolRejected) string { return p.CorrelationID })

// extractDryRunExecuted is the [approval.DryRunExecuted] adapter.
var extractDryRunExecuted = makeExtractor(func(p approval.DryRunExecuted) string { return p.CorrelationID })

// extractLocalPatchApplied is the [localpatch.LocalPatchApplied]
// adapter. The payload's operator-supplied `Reason` field IS carried
// through (M9.5 audit-purpose departure from metadata-only — see
// localpatch events.go doc-block).
var extractLocalPatchApplied = makeExtractor(func(p localpatch.LocalPatchApplied) string { return p.CorrelationID })

// extractHostedToolExported is the [hostedexport.HostedToolExported]
// adapter.
var extractHostedToolExported = makeExtractor(func(p hostedexport.HostedToolExported) string { return p.CorrelationID })

// extractToolShareProposed is the [toolshare.ToolShareProposed]
// adapter. The agent-supplied `Reason` field IS carried (M9.6
// audit-purpose departure).
var extractToolShareProposed = makeExtractor(func(p toolshare.ToolShareProposed) string { return p.CorrelationID })

// extractToolSharePROpened is the [toolshare.ToolSharePROpened]
// adapter.
var extractToolSharePROpened = makeExtractor(func(p toolshare.ToolSharePROpened) string { return p.CorrelationID })

// typeMismatch wraps [errUnexpectedPayload] with metadata-only
// diagnostics: the expected struct's Go type name and the offending
// payload's `%T`. Never embeds the payload VALUE — a verbose emitter
// might publish credentials inside a struct field a future reader
// would expect to be safe. The dispatcher does NOT log the err
// VALUE either (only the binding's `ExpectedType` + `%T` of the
// offending event); the wrapped err exists purely so a test
// `errors.Is(err, errUnexpectedPayload)` assertion has something to
// match against.
func typeMismatch(expected, got any) error {
	return fmt.Errorf("%w: expected %T, got %T", errUnexpectedPayload, expected, got)
}
