package toolregistry

import (
	"fmt"
	"regexp"
	"strings"
)

// validSourceName is the character allowlist applied by
// [SourceConfig.Validate]. Names are used as on-disk path components
// under `$DATA_DIR/tools/<source>/` and routed through
// [filepath.Join]; an unconstrained name would let `..` / `/` /
// `\` escape the tools directory.
//
// The allowlist matches `lower_kebab_or_snake_case` identifiers
// (letters, digits, `_`, `-`). It deliberately forbids `.` so
// `..` cannot appear; it forbids `/` and `\` so a name cannot
// embed a path separator on either Unix or Windows; it forbids
// `:` so a name cannot impersonate a URL scheme on stringification.
var validSourceName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// SourceKind is the closed-set kind discriminator for a [SourceConfig].
// New kinds need a corresponding wiring path in [Scheduler.SyncOnce];
// the set is intentionally small.
type SourceKind string

const (
	// SourceKindGit is a git repository pulled via [GitClient.Clone] /
	// [GitClient.Pull]. Requires a non-empty `url`.
	SourceKindGit SourceKind = "git"

	// SourceKindLocal is an operator-supplied local directory under
	// `$DATA_DIR/tools/<source>/` populated out-of-band (e.g. by
	// `make tools-local-install`, M9.5). The scheduler treats local
	// sources as already-synced: [Scheduler.SyncOnce] verifies the
	// directory exists and emits [SourceSynced] without git work.
	SourceKindLocal SourceKind = "local"

	// SourceKindHosted is the Watchkeeper-hosted source storing
	// AI-authored tools (M9.4 slack-native approval path). M9.1.a
	// shapes the seam only — actual hosted-storage I/O lands later.
	// The scheduler currently treats hosted sources the same as
	// local: exists-check + emit. Behaviour can diverge in M9.4
	// without changing this enum.
	SourceKindHosted SourceKind = "hosted"
)

// PullPolicy is the closed-set discriminator for when a source is
// re-synced. New policies need a corresponding wiring path in the
// caller's scheduler integration; M9.1.a defines the closed set and
// leaves the wiring to M9.1.b / the operator boot graph.
//
// The bootstrap behaviour and the periodic-refresh behaviour are
// independent: a source can be "synced once at boot AND refreshed on
// cron" by setting [SourceConfig.BootstrapOnBoot] true alongside
// `pull_policy: cron`. The policy here describes only the periodic
// refresh contract; the boot-time sync is the operator's call.
type PullPolicy string

const (
	// PullPolicyOnBoot syncs the source exactly once at process boot
	// (the operator calls [Scheduler.SyncOnce] from its bootstrap
	// path). No periodic refresh. Equivalent to
	// `pull_policy: on-demand` plus `bootstrap_on_boot: true`; kept
	// as a distinct value because it is the most common shape and
	// reads naturally in operator configs.
	PullPolicyOnBoot PullPolicy = "on-boot"

	// PullPolicyCron syncs the source on the configured `cron_spec`.
	// The cron-scheduler wiring (calling [Scheduler.SyncOnce] from a
	// cron tick) lives outside this package; the policy here just
	// declares the intent + the spec. Combine with
	// [SourceConfig.BootstrapOnBoot] true to ALSO sync at boot
	// without waiting for the first cron fire.
	PullPolicyCron PullPolicy = "cron"

	// PullPolicyOnDemand syncs the source only when explicitly
	// triggered (e.g. an operator CLI command or a webhook handler).
	// No automatic refresh. Combine with
	// [SourceConfig.BootstrapOnBoot] true to ALSO sync at boot.
	PullPolicyOnDemand PullPolicy = "on-demand"
)

// SourceConfig describes one entry in the operator's `tool_sources`
// list. Decoded from YAML; the YAML key conventions match the rest of
// `core/pkg/config` (lower_snake_case keys, lower_kebab-case enum
// values).
type SourceConfig struct {
	// Name is the unique identifier — used as the path component
	// under `$DATA_DIR/tools/<source>/` and as the resolution key for
	// [Scheduler.SyncOnce]. Required, non-empty, unique within a
	// config.
	Name string `yaml:"name" json:"name"`

	// Kind is the [SourceKind] discriminator. Required.
	Kind SourceKind `yaml:"kind" json:"kind"`

	// URL is the upstream clone URL for `kind: git`. Required for
	// git, forbidden for `local` / `hosted` (see [ErrMissingSourceURL]
	// and [ErrSourceURLNotAllowed]).
	URL string `yaml:"url,omitempty" json:"url,omitempty"`

	// Branch is the upstream branch tracked for `kind: git`. Empty
	// branch defaults to `main` at sync time — kept explicit on the
	// struct so a `git: log` of the operator config records the
	// intent.
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`

	// PullPolicy is the [PullPolicy] discriminator. Required.
	PullPolicy PullPolicy `yaml:"pull_policy" json:"pull_policy"`

	// CronSpec is the robfig/cron v3 6-field spec for `pull_policy:
	// cron`. Required for cron policy, ignored for other policies.
	CronSpec string `yaml:"cron_spec,omitempty" json:"cron_spec,omitempty"`

	// AuthSecret is the reference name of the auth credential used by
	// the [GitClient] when cloning / pulling a private source. Empty
	// AuthSecret means "no auth"; non-empty AuthSecret triggers a
	// per-call [AuthSecretResolver] invocation on every sync. The
	// resolved value is passed through to [GitClient]; this struct
	// never holds the plaintext credential. Valid only for
	// `kind: git`; setting it on `local` / `hosted` is a config
	// error caught by [SourceConfig.Validate]
	// ([ErrAuthSecretNotAllowed]).
	//
	// Per-call resolution (vs a process-global pinned value) keeps
	// per-tenant tokens safe — same lesson as M8.2.a's
	// `jira.BasicAuthSource` shape.
	AuthSecret string `yaml:"auth_secret,omitempty" json:"auth_secret,omitempty"`

	// BootstrapOnBoot, when true, indicates the operator boot graph
	// should call [Scheduler.SyncOnce] for this source during
	// process startup IN ADDITION to whatever the [PullPolicy]
	// schedules. The flag is orthogonal to [PullPolicy] so the
	// common "sync at boot AND refresh on cron" shape is expressible
	// without overloading the policy enum. Default false; the field
	// is operator-facing config, not a runtime signal — the
	// scheduler itself does not consult it.
	BootstrapOnBoot bool `yaml:"bootstrap_on_boot,omitempty" json:"bootstrap_on_boot,omitempty"`
}

// Validate runs the closed-set + cross-field validation for one
// [SourceConfig]. Called by [ValidateSources] and by [New]; callers
// can also invoke it directly when assembling sources programmatically
// (e.g. tests).
func (sc SourceConfig) Validate() error {
	if strings.TrimSpace(sc.Name) == "" {
		return ErrInvalidSourceName
	}
	if !validSourceName.MatchString(sc.Name) {
		return fmt.Errorf("%w: %q does not match %s", ErrInvalidSourceName, sc.Name, validSourceName.String())
	}
	switch sc.Kind {
	case SourceKindGit:
		if strings.TrimSpace(sc.URL) == "" {
			return fmt.Errorf("%w: source %q", ErrMissingSourceURL, sc.Name)
		}
	case SourceKindLocal, SourceKindHosted:
		if strings.TrimSpace(sc.URL) != "" {
			return fmt.Errorf("%w: source %q (kind %q)", ErrSourceURLNotAllowed, sc.Name, sc.Kind)
		}
		if strings.TrimSpace(sc.AuthSecret) != "" {
			return fmt.Errorf("%w: source %q (kind %q)", ErrAuthSecretNotAllowed, sc.Name, sc.Kind)
		}
	default:
		return fmt.Errorf("%w: source %q kind %q", ErrInvalidSourceKind, sc.Name, sc.Kind)
	}
	switch sc.PullPolicy {
	case PullPolicyOnBoot, PullPolicyOnDemand:
		// no extra fields
	case PullPolicyCron:
		if strings.TrimSpace(sc.CronSpec) == "" {
			return fmt.Errorf("%w: source %q", ErrInvalidCronSpec, sc.Name)
		}
	default:
		return fmt.Errorf("%w: source %q policy %q", ErrInvalidPullPolicy, sc.Name, sc.PullPolicy)
	}
	return nil
}

// EffectiveBranch returns the branch the scheduler should pass to
// [GitClient] for this source. An empty `Branch` field defaults to
// `main`; callers MUST NOT consult `sc.Branch` directly.
func (sc SourceConfig) EffectiveBranch() string {
	if strings.TrimSpace(sc.Branch) == "" {
		return "main"
	}
	return sc.Branch
}

// ValidateSources runs [SourceConfig.Validate] across `sources` and
// additionally checks for duplicate names. The return is the first
// failure encountered; callers that want to surface all failures at
// once can iterate themselves.
func ValidateSources(sources []SourceConfig) error {
	seen := make(map[string]struct{}, len(sources))
	for i, s := range sources {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("toolregistry: sources[%d]: %w", i, err)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateSourceName, s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

// CloneSources returns a deep-copy of `in` so callers cannot mutate
// the scheduler's internal slice post-construction. Mirrors the
// defensive-copy discipline established for saga-step configs in
// M7.1.c.c.
func CloneSources(in []SourceConfig) []SourceConfig {
	if in == nil {
		return nil
	}
	out := make([]SourceConfig, len(in))
	copy(out, in)
	return out
}
