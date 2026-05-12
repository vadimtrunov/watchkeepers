package approval

import "errors"

// ErrInvalidMode is returned by [Mode.Validate] /
// [DecodeModeYAML] when the configured value is outside the
// closed set documented on [Mode] (`git-pr`, `slack-native`,
// `both`). The set is closed by design — adding a new mode requires
// a new wiring path in M9.4.b — so a typo in the operator config
// surfaces here rather than silently degrading to a default.
var ErrInvalidMode = errors.New("approval: invalid approval mode")

// ErrInvalidTargetSource is returned by [TargetSource.Validate] when
// the [ProposalInput.TargetSource] is outside the closed set
// (`platform`, `private`). Note that `local` is deliberately absent
// from the [TargetSource] enum: the M9.4 roadmap text states "local
// source never offered to the agent" — the agent-facing
// [Proposer.Submit] therefore cannot resolve to a local source. A
// lead may later override `target_source` at approval time (M9.4.b);
// that override path uses a distinct lead-only enum and does not
// flow through this validator.
var ErrInvalidTargetSource = errors.New("approval: invalid target source")

// ErrModeMismatchHosted is returned by
// [ValidateModeForSources] when the configured
// [Mode] is [ModeGitPR] AND at least one configured
// [toolregistry.SourceConfig] has
// [toolregistry.SourceKindHosted]. The roadmap text mandates
// `slack-native` (or `both`) when any source is hosted because the
// hosted authoring flow exclusively uses the Slack approval card —
// a `git-pr`-only deployment with hosted sources would orphan every
// proposal targeting the hosted route.
var ErrModeMismatchHosted = errors.New("approval: slack-native (or both) required when any source is hosted")

// ErrModeYAMLParse wraps a yaml.v3 decode failure for the
// `approval_mode:` document. Strict mode (`KnownFields(true)`)
// rejects any extra key.
var ErrModeYAMLParse = errors.New("approval: approval_mode yaml parse")

// ErrModeYAMLMultiDoc is returned by
// [DecodeModeYAML] when the input contains a `---` separator
// followed by additional YAML documents. The approval_mode is a
// single-document setting; an accidental merge-conflict artefact or
// a trailing snippet would otherwise silently be ignored.
var ErrModeYAMLMultiDoc = errors.New("approval: approval_mode yaml contains multiple documents")

// ErrMissingProposalName is returned by [ProposalInput.Validate] when
// `Name` is empty / whitespace. Tool names are load-bearing for the
// runtime ACL gate (see [toolregistry.Manifest.Name]); an empty name
// would silently route every invocation to the default-deny branch.
var ErrMissingProposalName = errors.New("approval: proposal name required")

// ErrInvalidProposalName is returned by [ProposalInput.Validate] when
// `Name` does not match the lower_snake_case allowlist
// (`^[a-z][a-z0-9_]*$`). The roadmap text describes tools as
// `count_open_prs` / `find_overdue_tickets` — this validator pins
// the convention at the authoring boundary so the per-tool
// manifest.json's `name` field cannot drift downstream (M9.1.a
// deferred the regex check to a linter; M9.4.a enforces it at the
// `propose_tool` agent capability).
var ErrInvalidProposalName = errors.New("approval: invalid proposal name (lower_snake_case required)")

// ErrMissingProposalPurpose is returned by [ProposalInput.Validate]
// when `Purpose` is empty / whitespace. The purpose flows into the
// audit log and the lead-facing approval card; an empty value would
// produce an opaque card with no rationale.
var ErrMissingProposalPurpose = errors.New("approval: proposal purpose required")

// ErrMissingPlainLanguageDescription is returned by
// [ProposalInput.Validate] when `PlainLanguageDescription` is empty
// / whitespace. The roadmap text marks this field MANDATORY because
// the lead-facing approval card (M9.4.b) renders it verbatim and a
// non-engineer lead cannot review a proposal without it. Distinct
// from `Purpose`: purpose is the agent's reason for needing the
// tool; plain_language_description is the lead-facing summary in
// natural language.
var ErrMissingPlainLanguageDescription = errors.New("approval: plain_language_description required")

// ErrMissingCodeDraft is returned by [ProposalInput.Validate] when
// `CodeDraft` is empty / whitespace. The CI gates (M9.4.d) and the
// in-process AI reviewer (M9.4.b) BOTH consume the draft source; an
// empty draft would produce a proposal that no gate can evaluate.
var ErrMissingCodeDraft = errors.New("approval: code_draft required")

// ErrMissingProposalCapabilities is returned by
// [ProposalInput.Validate] when `Capabilities` is empty. A tool
// declaring no capabilities is a non-sensical state per
// [toolregistry.Manifest.Capabilities]'s godoc — the deny-by-default
// runtime ACL would refuse every call.
var ErrMissingProposalCapabilities = errors.New("approval: capabilities required")

// ErrInvalidProposalCapability is returned by
// [ProposalInput.Validate] when an individual entry in `Capabilities`
// is empty / whitespace. The validator does NOT check membership
// against M9.3's `dict/capabilities.yaml` — that lives in a future
// milestone (the M9.3 dictionary loader); at M9.4.a we only enforce
// shape.
var ErrInvalidProposalCapability = errors.New("approval: invalid proposal capability")

// ErrEmptyResolvedIdentity is returned by [Proposer.Submit] when the
// configured [IdentityResolver] returns `("", nil)`. Same fail-loud
// discipline as [toolregistry]'s [toolregistry.ErrEmptyResolvedAuth]:
// the resolver said "no error" while supplying an empty value —
// either a bug in the resolver or a misconfiguration silently
// erasing tenant identity.
var ErrEmptyResolvedIdentity = errors.New("approval: identity resolver returned empty value")

// ErrInvalidProposerID is returned by [Proposer.Submit] when the
// configured [IdentityResolver] returns a non-empty identifier
// exceeding [MaxProposerIDLength]. The bound prevents a buggy
// resolver from silently flowing an unbounded bearer-token-shaped
// string onto the [TopicToolProposed] event payload or the
// publish-failure logger entry — defensive shape check at the
// authoring boundary, same discipline as the per-field
// [ProposalInput] bounds.
var ErrInvalidProposerID = errors.New("approval: identity resolver returned invalid proposer id")

// ErrDuplicateProposalCapability is returned by [ProposalInput.Validate]
// when two entries in `Capabilities` carry the same id. Duplicates
// would render twice on the M9.4.b approval card, cause redundant
// dictionary lookups in M9.3, and inflate the audit-log capability
// count. Mirrors `toolregistry.ErrDuplicateSourceName`'s discipline
// (intra-set dedupe at the validator boundary, not at the
// downstream consumer).
var ErrDuplicateProposalCapability = errors.New("approval: duplicate proposal capability")

// ErrIdentityResolution wraps a non-nil [IdentityResolver] error.
// Mirrors [toolregistry.ErrAuthResolution]'s shape: callers can
// `errors.Is(err, ErrIdentityResolution)` for kind-of-failure and
// `errors.Is(err, underlyingErr)` for the cause.
var ErrIdentityResolution = errors.New("approval: identity resolution failed")

// ErrPublishToolProposed wraps a [Publisher.Publish] failure for
// [TopicToolProposed]. Asymmetric to
// [toolregistry.ErrPublishAfterSwap]: at the [Proposer.Submit]
// boundary there is no atomic state swap before publish — the
// proposal is built in-memory and the event IS the durable record at
// this layer. A publish failure therefore means the proposal is NOT
// observable downstream; the caller MUST treat the returned error
// as "proposal not landed" and retry or surface to the operator.
var ErrPublishToolProposed = errors.New("approval: publish tool_proposed failed")

// ErrInvalidRoute is returned by [Route.Validate] when
// the value is outside the closed set (`git-pr`, `slack-native`). The
// validator surfaces the typo loudly rather than silently degrading the
// downstream M9.7 audit-row's `route` field.
var ErrInvalidRoute = errors.New("approval: invalid approval route")

// ErrProposalNotFound is returned by [ProposalLookup.Lookup] when no
// proposal with the supplied id is in the store. Mirrors the
// [toolregistry] "missing referent" discipline; webhook receivers and
// callback dispatchers wrap this sentinel so an out-of-band caller
// (replay of an old webhook after a process restart, a forged button
// click referencing an unknown id) is logged with the proposal id but
// does not panic.
var ErrProposalNotFound = errors.New("approval: proposal not found")

// ErrWebhookMissingHeader is returned by the webhook receiver when the
// `X-Watchkeeper-Signature-256` or `X-Watchkeeper-Request-Timestamp`
// header is absent. Mirrors [slack/inbound.ErrMissingHeader]. The
// handler maps this to HTTP 401 with reason `missing_header` on the
// downstream M9.7 audit row.
var ErrWebhookMissingHeader = errors.New("approval: webhook missing signing header")

// ErrWebhookStaleTimestamp is returned by the webhook receiver when the
// request timestamp is outside the configured replay-attack window
// (default 5 minutes — same as Slack's guidance) OR when the timestamp
// header is not a parseable integer. Mirrors
// [slack/inbound.ErrStaleTimestamp]. Maps to HTTP 401 with reason
// `stale_timestamp`.
var ErrWebhookStaleTimestamp = errors.New("approval: webhook stale or unparseable timestamp")

// ErrWebhookBadSignature is returned by the webhook receiver when the
// computed HMAC does not match the supplied header (constant-time
// compare via [hmac.Equal]). Also covers the case where the header is
// missing the `sha256=` version prefix or contains non-hex bytes.
// Mirrors [slack/inbound.ErrBadSignature]. Maps to HTTP 401 with reason
// `bad_signature`.
var ErrWebhookBadSignature = errors.New("approval: webhook bad signature")

// ErrWebhookOversizeBody is returned by the webhook receiver when the
// inbound body exceeds the configured cap (default 1 MiB, same as the
// slack-inbound default). The cap is in place so an adversarial poster
// cannot DOS the process by streaming an unbounded body before the
// HMAC compare runs. Maps to HTTP 413 with reason `oversize_body`.
var ErrWebhookOversizeBody = errors.New("approval: webhook body exceeds size cap")

// ErrWebhookMalformedPayload is returned by the webhook receiver when
// the body decodes but the resulting JSON does not carry the expected
// fields (proposal_id / action / timestamp) or carries values outside
// their closed sets (action ∈ {approved, rejected}). Maps to HTTP 400
// with reason `malformed_payload`.
var ErrWebhookMalformedPayload = errors.New("approval: webhook malformed payload")

// ErrWebhookEmptySecret is returned by the webhook receiver when the
// configured [WebhookSecretResolver] returns an empty byte slice (or a
// nil slice). Mirrors [ErrEmptyResolvedIdentity]'s fail-loud discipline:
// the resolver said "no error" while supplying an empty value — either
// a bug in the resolver or a misconfiguration silently disabling the
// HMAC compare. Maps to HTTP 500 (the per-deployment secret rotation
// failed) with reason `secret_resolution`.
var ErrWebhookEmptySecret = errors.New("approval: webhook secret resolver returned empty value")

// ErrWebhookSecretResolution wraps a non-nil
// [WebhookSecretResolver] error. Mirrors [ErrIdentityResolution]'s
// shape: callers `errors.Is(err, ErrWebhookSecretResolution)` for
// kind-of-failure and `errors.Is(err, underlyingErr)` for the cause.
// Maps to HTTP 500 with reason `secret_resolution`.
var ErrWebhookSecretResolution = errors.New("approval: webhook secret resolution failed")

// ErrSchedulerSync wraps a non-nil [SchedulerSyncer.SyncOnce] failure
// surfaced by the webhook receiver. A sync failure means the
// now-approved tool is NOT yet observable to running runtimes; the
// caller MUST treat the returned error as "approval landed on the event
// stream BUT the registry has not yet re-scanned the source — re-drive
// the sync manually". Asymmetric to [ErrPublishToolApproved]: the
// publish failure means the approval event itself was lost; this
// sentinel means the event landed but the consequent sync did not.
var ErrSchedulerSync = errors.New("approval: tool source sync failed")

// ErrSourceMappingFailed is returned by the webhook receiver when the
// configured [SourceForTarget] resolver returns a non-nil error or an
// empty source name. Maps to HTTP 422 with reason
// `source_mapping_failed`: the proposal carries a valid
// [TargetSource] but no [toolregistry.SourceConfig] in this deployment
// matches it (e.g. the operator removed the `private` source between
// proposal time and approval time).
var ErrSourceMappingFailed = errors.New("approval: source mapping failed")

// ErrPublishToolApproved wraps a [Publisher.Publish] failure for
// [TopicToolApproved]. Same "event is the durable record" contract as
// [ErrPublishToolProposed]: a publish failure means the approval is
// NOT observable downstream and the caller MUST retry or surface to
// the operator.
var ErrPublishToolApproved = errors.New("approval: publish tool_approved failed")

// ErrPublishToolRejected wraps a [Publisher.Publish] failure for
// [TopicToolRejected]. Same contract as [ErrPublishToolApproved].
var ErrPublishToolRejected = errors.New("approval: publish tool_rejected failed")

// ErrPublishDryRunRequested wraps a [Publisher.Publish] failure for
// [TopicDryRunRequested]. Same contract as [ErrPublishToolApproved];
// subscribers (M9.4.c dry-run executor) cannot react to a dropped
// event.
var ErrPublishDryRunRequested = errors.New("approval: publish tool_dry_run_requested failed")

// ErrPublishQuestionAsked wraps a [Publisher.Publish] failure for
// [TopicQuestionAsked]. Same contract as [ErrPublishToolApproved].
var ErrPublishQuestionAsked = errors.New("approval: publish tool_question_asked failed")

// ErrReviewerNilProposal is returned by [Reviewer.Review] when the
// supplied [Proposal] has a zero-valued [Proposal.ID] OR
// [Proposal.Input.Name] empty. The reviewer pre-condition is that the
// proposal already passed [ProposalInput.Validate]; a zero-id /
// empty-name proposal indicates the caller bypassed [Proposer.Submit].
var ErrReviewerNilProposal = errors.New("approval: reviewer received zero-valued proposal")

// ErrInvalidActionID is returned by [DecodeActionID] when the input
// does not parse as `tool_approval:<proposal_id>:<button>`, when the
// prefix mismatches, when the proposal id does not parse as a UUID, or
// when the button value is outside the closed [ButtonAction] set.
// Distinct from the [cards.ErrInvalidActionID] in
// `messenger/slack/cards`: the M9.4.b approval card carries its OWN
// action_id namespace so the M6.3 dispatcher cannot accidentally route
// an M9.4.b click into the spawn approval saga.
var ErrInvalidActionID = errors.New("approval: invalid action_id")

// ErrInvalidButtonValue is returned by the callback dispatcher when the
// decoded [ButtonAction] is outside the closed set
// (`approve`, `reject`, `test_in_my_dm`, `ask_questions`). Distinct
// from [ErrInvalidActionID] so an audit subscriber can group the two
// failure modes independently.
var ErrInvalidButtonValue = errors.New("approval: invalid button value")

// ErrCardMissingInput is returned by [RenderApprovalCard] when any of
// the required fields on [CardInput] are zero-valued (proposal
// id, tool name, plain-language description, capabilities, review
// result). The renderer is pure — it does not silently produce an
// empty card; callers receive a sentinel so the wiring path can
// distinguish "no proposal" from "render failed".
var ErrCardMissingInput = errors.New("approval: approval card missing required input")

// ErrCardMissingLeadDM is returned by the callback dispatcher when a
// `[Test in my DM]` click is dispatched without a resolved lead DM
// channel id. The dry-run executor cannot run without a forced DM
// destination; the click is rejected at the dispatcher boundary so an
// audit row records the failure rather than M9.4.c silently dropping
// it.
var ErrCardMissingLeadDM = errors.New("approval: callback missing lead DM channel")

// ErrCardProposalMismatch is returned by [RenderApprovalCard] when the
// supplied [CardInput.Review.ProposalID] does not equal
// [CardInput.ProposalID]. The renderer fails closed on this
// orchestration bug so a card never ships with the proposal-identity
// of A and the gate-result body of B (a class of mismatched-card
// confusion the M9.4.b iter-1 review flagged on `card.go`).
var ErrCardProposalMismatch = errors.New("approval: card review.proposal_id does not match card.proposal_id")

// ErrInvalidDecisionKind is returned by [DecisionKind.Validate] when
// the value is outside the closed approve/reject set. Mirrors
// [ErrInvalidRoute]'s discipline; the decision-recorder seam
// refuses to claim a decision for an unknown kind so an audit row never
// records a typo.
var ErrInvalidDecisionKind = errors.New("approval: invalid decision kind")

// ErrDecisionConflict is returned by [DecisionRecorder.MarkDecided]
// when a different decision kind was already claimed for the supplied
// proposal id (e.g. a `[Reject]` click arrives after an `[Approve]`
// click already landed). The first decision is final by construction —
// the dispatcher surfaces this sentinel so the operator's audit row
// records the conflicting attempt rather than silently overwriting the
// recorded outcome.
var ErrDecisionConflict = errors.New("approval: proposal already decided with a different kind")
