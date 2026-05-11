package toolregistry

import "errors"

// ErrInvalidSourceName is returned during [DecodeSourcesYAML] / [Validate]
// when a configured source has an empty `name`. Every source identifier is
// load-bearing — it names the on-disk sync directory, scopes auth-secret
// resolution, and labels emitted events — so an empty name would silently
// collapse two unrelated sources onto the same directory.
var ErrInvalidSourceName = errors.New("toolregistry: invalid source name")

// ErrDuplicateSourceName is returned during [Validate] when two
// configured sources share the same `name`. Names are the resolution key
// for [Scheduler.SyncOnce] and the path component under `$DATA_DIR/tools/`;
// a duplicate would create an ambiguous overlay (M9.2 conflict resolution
// resolves SAME-tool-NAME conflicts across sources, not same-source-name).
var ErrDuplicateSourceName = errors.New("toolregistry: duplicate source name")

// ErrInvalidSourceKind is returned during [Validate] when a configured
// source declares a `kind` outside the closed set documented on
// [SourceKind] (`git`, `local`, `hosted`). The set is closed by design —
// adding a new kind requires a new wiring path in [Scheduler].
var ErrInvalidSourceKind = errors.New("toolregistry: invalid source kind")

// ErrInvalidPullPolicy is returned during [Validate] when a configured
// source declares a `pull_policy` outside the closed set documented on
// [PullPolicy] (`on-boot`, `cron`, `on-demand`). For `cron` policies the
// `cron_spec` field is additionally required; absence yields
// [ErrInvalidCronSpec].
var ErrInvalidPullPolicy = errors.New("toolregistry: invalid pull policy")

// ErrInvalidCronSpec is returned during [Validate] when a source declares
// `pull_policy: cron` but supplies an empty `cron_spec`. The cron text is
// passed through to the operator's cron scheduler (M2b.4); empty input
// would land an undefined firing schedule downstream.
var ErrInvalidCronSpec = errors.New("toolregistry: invalid cron spec")

// ErrMissingSourceURL is returned during [Validate] when a `git`-kind
// source omits `url`. `local` and `hosted` kinds do not require `url` and
// passing one is treated as a programmer error (caught by [Validate]).
var ErrMissingSourceURL = errors.New("toolregistry: missing source url")

// ErrSourceURLNotAllowed is returned during [Validate] when a `local` or
// `hosted` source supplies a `url`. The fields are mutually exclusive
// with the source kind to prevent a silent contract drift (e.g. a `local`
// source pointing at a git URL would suggest sync behaviour that does
// not exist).
var ErrSourceURLNotAllowed = errors.New("toolregistry: source url not allowed")

// ErrAuthSecretNotAllowed is returned during [Validate] when a `local`
// or `hosted` source supplies an `auth_secret`. Local sources are
// populated out-of-band (M9.5) and hosted sources route through the
// internal storage adapter (M9.4); neither talks to a remote git
// endpoint so an auth credential is meaningless. Catching this at
// validation prevents a misconfigured source from quietly failing in
// the `auth` phase of [Scheduler.SyncOnce].
var ErrAuthSecretNotAllowed = errors.New("toolregistry: auth secret not allowed")

// ErrUnsafeLocalPath is returned by [Scheduler.SyncOnce] when the
// derived `<DataDir>/tools/<sourceName>` path escapes its parent
// after [filepath.Clean]. Belt-and-braces defence on top of
// [SourceConfig.Validate]'s character allowlist; firing it indicates
// either a successful traversal attempt bypassing validation or a
// programmer error.
var ErrUnsafeLocalPath = errors.New("toolregistry: unsafe local path")

// ErrEmptyResolvedAuth is returned by [Scheduler.SyncOnce] when the
// configured [AuthSecretResolver] returns `("", nil)` for a source
// whose [SourceConfig.AuthSecret] is non-empty. The operator declared
// "this source needs auth" and a silent demote to anonymous would
// either fail later with an opaque 401 (private repo) or succeed
// against an unintended public mirror.
var ErrEmptyResolvedAuth = errors.New("toolregistry: resolver returned empty credential")

// ErrLocalSourceMissing is returned by [Scheduler.SyncOnce] when a
// `local` or `hosted` source's expected directory does not exist on
// disk. Population is the responsibility of out-of-band tooling
// (M9.5 `make tools-local-install` for local; M9.4 hosted-storage
// pipeline for hosted); the scheduler refuses to create the
// directory itself because doing so would emit a spurious
// `source_synced` event for an empty tree.
var ErrLocalSourceMissing = errors.New("toolregistry: local source directory missing")

// ErrInvalidDataDir is returned by [New] when `DataDir` is empty.
// The scheduler clones / pulls into `<DataDir>/tools/<source>/` and an
// empty DataDir would silently create on-disk artifacts in the process
// cwd — fail loudly instead.
var ErrInvalidDataDir = errors.New("toolregistry: invalid data dir")

// ErrUnknownSource is returned by [Scheduler.SyncOnce] when the supplied
// `sourceName` does not match any configured source. The scheduler does
// not auto-create sources at sync time — the configured set is fixed at
// construction.
var ErrUnknownSource = errors.New("toolregistry: unknown source")

// ErrManifestParse wraps a malformed-JSON failure when decoding a
// per-tool `manifest.json`. Callers can `errors.Is(err, ErrManifestParse)`
// without depending on the specific [encoding/json] failure type.
var ErrManifestParse = errors.New("toolregistry: manifest parse")

// ErrManifestUnknownField is returned by [DecodeManifest] when the
// supplied JSON contains a key not declared on [Manifest]. Strict
// decoding refuses unknown fields so a typo in an operator-authored
// manifest does not silently degrade to a manifest with default values.
var ErrManifestUnknownField = errors.New("toolregistry: manifest unknown field")

// ErrManifestMissingRequired is returned by [DecodeManifest] when a
// required field on [Manifest] is empty / absent. The required set is
// `name`, `version`, `capabilities`, `schema`; `source` is auto-filled
// by the loader and `signature` is optional.
var ErrManifestMissingRequired = errors.New("toolregistry: manifest missing required field")

// ErrManifestSourceCollision is returned by [LoadManifestFromFile] when
// the manifest's on-disk `source` field is already populated AND does
// not match the loader's stamping argument. The loader stamps `source`
// from the sync directory unconditionally; an operator-authored
// `source` field would either match (no-op) or conflict (this sentinel)
// — silent overwrite would mask a tampered manifest.
var ErrManifestSourceCollision = errors.New("toolregistry: manifest source collision")

// ErrSignatureVerification wraps a [SignatureVerifier] failure. The
// default [NoopSignatureVerifier] never returns this sentinel; M9.3's
// cosign / minisign integration will surface verification failures
// chained through this wrapper.
var ErrSignatureVerification = errors.New("toolregistry: signature verification failed")

// ErrAuthResolution wraps an [AuthSecretResolver] failure. The
// resolver is called once per [Scheduler.SyncOnce] for sources whose
// `auth_secret` is non-empty; a resolution error aborts the sync and
// emits a [SourceFailed] event carrying the error TYPE only.
var ErrAuthResolution = errors.New("toolregistry: auth resolution failed")

// ErrSyncClone wraps a [GitClient.Clone] failure. The scheduler emits
// a [SourceFailed] event and returns this sentinel; the caller can
// `errors.Is` to distinguish clone-side failures from pull-side.
var ErrSyncClone = errors.New("toolregistry: sync clone failed")

// ErrSyncPull wraps a [GitClient.Pull] failure (same shape as
// [ErrSyncClone] but for the incremental-update path).
var ErrSyncPull = errors.New("toolregistry: sync pull failed")

// ErrFSStat wraps a [FS.Stat] failure that the scheduler cannot
// proceed past (anything other than a not-exist sentinel, which is
// the trigger for a fresh clone).
var ErrFSStat = errors.New("toolregistry: fs stat failed")

// ErrFSMkdir wraps a [FS.MkdirAll] failure when preparing the
// per-source sync directory.
var ErrFSMkdir = errors.New("toolregistry: fs mkdir failed")
