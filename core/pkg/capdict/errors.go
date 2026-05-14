package capdict

import "errors"

// ErrInvalidDictionary is the umbrella sentinel for every dictionary
// loader failure: strict-decode errors, structural violations, AND
// per-row invariant violations. The leaf sentinels below
// ([ErrEmptyDescription], [ErrInvalidCapabilityID], etc.) are
// CHAINED under this one via Go 1.20+ multi-wrap (`fmt.Errorf("%w:
// %w: …", ErrInvalidDictionary, leaf, …)`) so callers can match
// either:
//
//   - `errors.Is(err, ErrInvalidDictionary)` — "the dictionary did
//     not load cleanly" (operator-level triage).
//   - `errors.Is(err, ErrEmptyDescription)` etc. — specific failure
//     class for per-row diagnostics.
//
// I/O failures (`os.ReadFile` errors on `LoadFromFile`) surface
// under the distinct [ErrDictionaryRead] sentinel — they are
// operationally different from structural violations and operators
// triaging "file missing" vs "yaml malformed" need the distinction.
var ErrInvalidDictionary = errors.New("capdict: invalid dictionary")

// ErrDictionaryRead is returned by [LoadFromFile] when the supplied
// [FS] cannot deliver the bytes (file missing, permission denied,
// underlying os.ReadFile error). Distinct from [ErrInvalidDictionary]
// so operator triage code can branch on "load failed because the
// path is wrong" vs "load failed because the content is malformed".
// Wraps the underlying read error via `%w` so `errors.As(err,
// &pathError)` recovers the os-level detail.
var ErrDictionaryRead = errors.New("capdict: dictionary read failed")

// ErrUnknownCapability is returned by [Dictionary.TranslateOrError]
// when the supplied capability id is not in the dictionary. The
// translator surface ([Translator]) treats this case as a miss and
// returns the empty string instead.
var ErrUnknownCapability = errors.New("capdict: unknown capability")

// ErrEmptyDescription is chained under [ErrInvalidDictionary] when a
// capability entry omits its description or supplies a whitespace-
// only value. The dictionary's purpose is to render a human-readable
// line — an empty description defeats the purpose AND would render
// as a blank bullet on the approval card.
var ErrEmptyDescription = errors.New("capdict: capability description must not be empty")

// ErrDescriptionTooLong is chained under [ErrInvalidDictionary] when
// a description exceeds [MaxDescriptionLength] bytes. Pinned with
// its own sentinel rather than the umbrella ErrInvalidDictionary so
// per-row diagnostics surface the specific cause (mirrors the
// id-too-long path which has lived under [ErrInvalidCapabilityID]).
var ErrDescriptionTooLong = errors.New("capdict: capability description exceeds the byte cap")

// ErrInvalidCapabilityID is chained under [ErrInvalidDictionary]
// when a capability id fails the lower-snake-case + colon-namespace
// grammar enforced by [validateCapabilityID]. Capdict's grammar is
// STRICTER than `approval.proposal.go`'s capability-id validator —
// the proposer enforces only non-blank + duplicate + length, while
// capdict additionally rejects uppercase, hyphens, dots, spaces, and
// empty namespace segments. Every id capdict accepts is a legal
// proposer id, NOT vice-versa.
var ErrInvalidCapabilityID = errors.New("capdict: invalid capability id")

// ErrEmbeddedControlByte is chained under [ErrInvalidDictionary]
// when a description carries an ASCII control byte (NUL / TAB / NL
// / CR inside a single line / DEL). Multi-line descriptions are
// rejected at the same boundary — the card renderer treats every
// description as one bullet of mrkdwn. Invalid UTF-8 is rejected at
// the same loader pass via [utf8.ValidString] and surfaces under
// the umbrella sentinel without its own leaf.
var ErrEmbeddedControlByte = errors.New("capdict: description carries an embedded control byte")

// ErrUnsupportedVersion is chained under [ErrInvalidDictionary]
// when the YAML carries a `version:` value outside the supported
// set ([SupportedSchemaVersion]). v1 is the only supported version
// in Phase 1; future versions land via additive yaml fields (new
// locale keys, etc.) on the same major.
var ErrUnsupportedVersion = errors.New("capdict: unsupported schema version")
