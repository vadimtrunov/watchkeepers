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
