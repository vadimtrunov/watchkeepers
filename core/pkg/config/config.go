package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vadimtrunov/watchkeepers/core/pkg/secrets"
)

// Config is the strongly-typed root of the operator-supplied
// configuration. Sub-structs group fields by service so callers thread
// only what they need (e.g. `cfg.Keep.BaseURL` rather than a flat bag).
//
// Field-tag conventions:
//
//   - `yaml:"<key>"` — the YAML key the value is decoded from / encoded to.
//   - `env:"<NAME>"` — the env-var name that overrides the YAML value.
//     [WithEnvPrefix] lets multi-tenant deployments namespace these.
//   - `yaml:"-"` — exclude from YAML serialisation. Used on resolved
//     secret-value fields so the literal cannot leak via a config dump.
//
// The `*Secret` convention: a string field whose Go name ends in
// `Secret` holds a secret-reference name (e.g. `DB_PASSWORD`). By
// convention the YAML key SHOULD also end in `_secret` (e.g.
// `token_secret`) for human clarity, but detection is name-based —
// the Go field name is what matters, not the YAML tag. The loader
// resolves the reference through the configured [secrets.SecretSource]
// and stores the resolved value in the sibling field (the field WITHOUT
// the `Secret` suffix). The reference name itself is preserved so
// operators can inspect / rotate it; only the resolved value is
// considered sensitive.
type Config struct {
	Keep     KeepConfig     `yaml:"keep"`
	Notebook NotebookConfig `yaml:"notebook"`
}

// KeepConfig configures the connection to the Keep service. The token
// itself is never stored in YAML — operators set [KeepConfig.TokenSecret]
// to a secret-reference name (e.g. `KEEP_TOKEN`) and the loader resolves
// it through the configured [secrets.SecretSource], landing the
// resolved value in [KeepConfig.Token].
type KeepConfig struct {
	// BaseURL is the Keep service endpoint (e.g. `https://keep.example.com`).
	// Required; an empty value after all layers run yields
	// [ErrMissingRequired]. Override via env var
	// `WATCHKEEPER_KEEP_BASE_URL` or via the YAML key `keep.base_url`.
	BaseURL string `yaml:"base_url" env:"WATCHKEEPER_KEEP_BASE_URL"`

	// TokenSecret is the secret-reference name (NOT the token value
	// itself). The loader passes this name to the configured
	// [secrets.SecretSource] and stores the resolved value in
	// [KeepConfig.Token]. Empty TokenSecret skips resolution; a non-empty
	// TokenSecret without a configured SecretSource yields
	// [ErrNoSecretSource].
	TokenSecret string `yaml:"token_secret"`

	// Token holds the resolved secret value. Populated by the loader
	// after [KeepConfig.TokenSecret] is resolved through the configured
	// [secrets.SecretSource]; never written by callers. Excluded from
	// YAML to prevent accidental round-trip serialisation of the secret.
	Token string `yaml:"-"`
}

// NotebookConfig configures the per-agent notebook storage substrate.
type NotebookConfig struct {
	// DataDir is the on-disk directory under which per-agent SQLite
	// notebook files are created. Defaults to
	// `$WATCHKEEPER_DATA/notebook` when `WATCHKEEPER_DATA` is set in the
	// environment; otherwise the field is empty unless overridden by
	// YAML or the env-var below. Override via env var
	// `WATCHKEEPER_NOTEBOOK_DATA_DIR` or via YAML key
	// `notebook.data_dir`.
	DataDir string `yaml:"data_dir" env:"WATCHKEEPER_NOTEBOOK_DATA_DIR"`
}

// Logger is the diagnostic sink wired in via [WithLogger]. The shape
// mirrors the cron / notebook / secrets Logger interface: a single
// `Log(ctx, msg, kv...)` method so callers can substitute structured
// loggers (e.g. an slog wrapper) without losing type compatibility.
//
// The variadic `kv` slice carries flat key,value pairs
// (`"field", "Keep.TokenSecret", "err", err`). A nil logger silently
// drops the message — the package never panics on a nil logger.
//
// Redaction discipline: implementations MUST NEVER log secret values.
// Only field paths and error descriptions are acceptable log fields.
// The package itself only invokes Log with non-secret arguments;
// custom implementations wired via [WithLogger] must follow the same
// rule.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Option configures the loader. Pass options to [Load]; later options
// override earlier ones for the same field.
type Option func(*loaderConfig)

// loaderConfig is the internal mutable bag the [Option] callbacks
// populate. Held in a separate type so [Config] itself stays as the
// caller-facing payload.
type loaderConfig struct {
	filePath     string
	envPrefix    string
	secretSource secrets.SecretSource
	logger       Logger
}

// WithFile sets the path to a YAML config file. An empty path is
// treated as "no file" and the file layer is skipped silently. A
// non-empty path that cannot be opened yields a wrapped
// [ErrParseYAML].
//
// The disambiguation: empty-string path means the caller deliberately
// omitted a file (env-vars + defaults are sufficient); a non-empty
// path means the caller asked for THIS file and the loader fails
// loudly if it is missing.
func WithFile(path string) Option {
	return func(c *loaderConfig) {
		c.filePath = path
	}
}

// WithEnvPrefix prepends `prefix` to every env-var name declared in
// struct tags. An empty prefix is the default (no prepending). Useful
// for multi-tenant deployments that share an environment but want
// per-tenant config:
//
//	cfg, _ := config.Load(ctx, config.WithEnvPrefix("ACME_"))
//	// reads ACME_WATCHKEEPER_KEEP_BASE_URL, etc.
func WithEnvPrefix(prefix string) Option {
	return func(c *loaderConfig) {
		c.envPrefix = prefix
	}
}

// WithSecretSource wires a [secrets.SecretSource] for `*_secret`
// resolution. A nil source is a silent no-op so callers can always
// pass through whatever they have without a nil-guard at the call
// site. When unset (or set to nil) AND any `*_secret` field is
// non-empty, [Load] returns [ErrNoSecretSource].
func WithSecretSource(src secrets.SecretSource) Option {
	return func(c *loaderConfig) {
		if src != nil {
			c.secretSource = src
		}
	}
}

// WithLogger wires a diagnostic [Logger] onto the loader. A nil logger
// is a silent no-op so callers can always pass through whatever they
// have. Currently only secret-resolution errors emit a log entry;
// success paths and routine layer transitions are silent to avoid
// chatty boot output.
func WithLogger(l Logger) Option {
	return func(c *loaderConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// envTagName is the struct-tag key used to declare an env-var override
// for a field. See the [Config] godoc for the convention.
const envTagName = "env"

// secretFieldSuffix is the suffix that marks a [Config] field as a
// secret-reference. The loader walks struct fields with names ending
// in this suffix and resolves them via the configured
// [secrets.SecretSource], landing the resolved value in the sibling
// field whose name is the same minus the suffix.
const secretFieldSuffix = "Secret"

// Load resolves the operator configuration from the layered source
// stack and returns a populated [*Config], or `(nil, err)` on the
// first layer failure.
//
// Layer order (from lowest to highest precedence):
//
//  1. Built-in defaults — see [applyDefaults].
//  2. YAML file via [WithFile] — strict mode
//     (`yaml.Decoder.KnownFields(true)`) rejects unknown keys.
//  3. Env-var overrides for fields with `env:"NAME"` tags. Empty
//     env-var values do NOT override.
//  4. `*_secret` resolution via the configured [secrets.SecretSource].
//
// After the four layers run, [Load] validates that required fields
// are non-empty.
//
// `ctx` is honoured during secret resolution (each
// [secrets.SecretSource.Get] call receives it). A nil ctx is replaced
// by [context.Background] so [Load] never panics — callers can pass
// whatever they have. A pre-cancelled ctx short-circuits in the secret
// layer with `ctx.Err()` (the YAML, env, and validation layers run
// purely on local data and do not consult ctx).
//
//nolint:contextcheck // intentional: nil-ctx safety fallback to context.Background; the resulting ctx is threaded into SecretSource.Get below.
func Load(ctx context.Context, opts ...Option) (*Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	loaderCfg := &loaderConfig{}
	for _, opt := range opts {
		opt(loaderCfg)
	}

	cfg := &Config{}

	// Phase A — defaults.
	applyDefaults(cfg)

	// Phase B — YAML file (optional).
	if loaderCfg.filePath != "" {
		if err := decodeYAMLFile(loaderCfg.filePath, cfg); err != nil {
			return nil, err
		}
	}

	// Phase C — env-var overrides.
	applyEnvOverrides(reflect.ValueOf(cfg).Elem(), loaderCfg.envPrefix)

	// Phase D — secret resolution.
	if err := resolveSecrets(ctx, reflect.ValueOf(cfg).Elem(), loaderCfg.secretSource, loaderCfg.logger); err != nil {
		return nil, err
	}

	// Phase E — validation.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyDefaults populates fields whose default values cannot be
// expressed at struct-literal time (because they consult the process
// environment). Statically-known defaults (e.g. zero values) are
// already in place via Go's zero-value rule and need no explicit
// assignment.
func applyDefaults(cfg *Config) {
	if dataRoot := os.Getenv("WATCHKEEPER_DATA"); dataRoot != "" {
		cfg.Notebook.DataDir = filepath.Join(dataRoot, "notebook")
	}
}

// decodeYAMLFile opens `path` and decodes its contents into `cfg`
// using strict mode (`KnownFields(true)`) so unknown keys yield
// [ErrUnknownField]. Open failures (missing file, permission denied)
// and decode failures both wrap [ErrParseYAML]; strict-mode unknown
// keys additionally chain through [ErrUnknownField].
func decodeYAMLFile(path string, cfg *Config) error {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied at boot, not a user input
	if err != nil {
		return fmt.Errorf("%w: %w", ErrParseYAML, err)
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		// yaml.v3 reports unknown keys with a message that contains
		// "field ... not found in type"; we surface that as
		// ErrUnknownField (chained through ErrParseYAML so callers
		// matching either sentinel succeed).
		if isUnknownFieldErr(err) {
			return fmt.Errorf("%w: %w: %w", ErrUnknownField, ErrParseYAML, err)
		}
		return fmt.Errorf("%w: %w", ErrParseYAML, err)
	}
	return nil
}

// isUnknownFieldErr reports whether `err` is a yaml.v3 strict-mode
// "unknown field" diagnostic. yaml.v3 does not export a typed sentinel
// for this case, so we sniff the message text. The substring checked
// here is the exact phrasing produced by yaml.v3 (`v3.0.1`) when
// `Decoder.KnownFields(true)` is set and an unknown key is encountered:
// `line N: field X not found in type Y`. Any error containing the
// `not found in type` fragment is treated as the unknown-key case.
func isUnknownFieldErr(err error) bool {
	return strings.Contains(err.Error(), "not found in type")
}

// applyEnvOverrides walks `v` (expected: a settable struct value) and,
// for every string field whose `env:"NAME"` tag yields a non-empty
// process env-var, overwrites the field with the env-var value. Empty
// env-var values are no-ops — they do NOT clear an earlier layer.
//
// The walk recurses into nested struct fields so [Config]'s
// per-service sub-structs are visited transparently. Non-struct,
// non-string fields are skipped silently.
func applyEnvOverrides(v reflect.Value, prefix string) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		structField := t.Field(i)

		switch field.Kind() {
		case reflect.Struct:
			applyEnvOverrides(field, prefix)
			continue
		case reflect.String:
			tag := structField.Tag.Get(envTagName)
			if tag == "" {
				continue
			}
			envName := prefix + tag
			if val, ok := os.LookupEnv(envName); ok && val != "" {
				field.SetString(val)
			}
		default:
			// Other kinds (int, bool, slice, etc.) are not yet
			// supported. M3.4.b ships only string fields; future
			// additions can extend this switch.
			continue
		}
	}
}

// resolveSecrets walks `v` and, for every string field whose name ends
// in [secretFieldSuffix] (e.g. `TokenSecret`), treats the value as a
// secret-reference name and resolves it through `src`. The resolved
// value is written to the sibling field whose name is the same minus
// the suffix (`Token`). Empty secret-reference values are skipped (the
// operator did not request a secret on that field).
//
// Returns:
//
//   - [ErrNoSecretSource] if any `*Secret` field is non-empty and `src`
//     is nil — a non-empty `*Secret` is a request for resolution, and
//     silently leaving the sibling empty would land empty credentials
//     downstream.
//   - [ErrSecretResolutionFailed] (chained with the underlying
//     SecretSource error) if `src.Get` returns an error.
//
// The `logger` (if non-nil) is called only on the resolution-failed
// path with the field path + the wrapped error — never with the
// secret value or the resolved sibling value.
func resolveSecrets(ctx context.Context, v reflect.Value, src secrets.SecretSource, logger Logger) error {
	return walkSecrets(ctx, v, "", src, logger)
}

// walkSecrets is the recursive helper for [resolveSecrets]. `pathPrefix`
// accumulates the dotted field path (e.g. `Keep.TokenSecret`) so error
// messages and log entries name the offending field unambiguously.
func walkSecrets(ctx context.Context, v reflect.Value, pathPrefix string, src secrets.SecretSource, logger Logger) error {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		structField := t.Field(i)
		fieldPath := joinPath(pathPrefix, structField.Name)

		if field.Kind() == reflect.Struct {
			if err := walkSecrets(ctx, field, fieldPath, src, logger); err != nil {
				return err
			}
			continue
		}

		if field.Kind() != reflect.String {
			continue
		}
		// Detection is by Go field-name suffix ("Secret"); YAML-key
		// alignment (e.g. `yaml:"token_secret"`) is by convention only.
		if !strings.HasSuffix(structField.Name, secretFieldSuffix) {
			continue
		}
		ref := field.String()
		if ref == "" {
			// Operator did not request a secret on this field —
			// nothing to resolve, and the sibling stays as-is.
			continue
		}
		if src == nil {
			return fmt.Errorf("%w: %s", ErrNoSecretSource, fieldPath)
		}

		val, err := src.Get(ctx, ref)
		if err != nil {
			logSecretError(ctx, logger, fieldPath, err)
			return fmt.Errorf("%w: %s: %w", ErrSecretResolutionFailed, fieldPath, err)
		}

		// Populate the sibling field (name without the "Secret" suffix).
		siblingName := strings.TrimSuffix(structField.Name, secretFieldSuffix)
		sibling := v.FieldByName(siblingName)
		if !sibling.IsValid() {
			// Programmer error: a *Secret field with no sibling — fail
			// loudly rather than silently dropping the resolved value.
			return fmt.Errorf("%w: %s: missing sibling field %q", ErrSecretResolutionFailed, fieldPath, siblingName)
		}
		if !sibling.CanSet() || sibling.Kind() != reflect.String {
			return fmt.Errorf("%w: %s: sibling field %q is not a settable string", ErrSecretResolutionFailed, fieldPath, siblingName)
		}
		sibling.SetString(val)
	}
	return nil
}

// joinPath joins a parent dotted path with a leaf field name, handling
// the empty-prefix case so the root struct's fields render as "Keep"
// rather than ".Keep".
func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// logSecretError forwards a secret-resolution error to the optional
// [Logger]. Nil-logger safe. Logs only the field path and the error
// type — never the secret-reference value, never the resolved value
// (the resolved value does not exist on the error path anyway).
func logSecretError(ctx context.Context, logger Logger, fieldPath string, err error) {
	if logger == nil {
		return
	}
	logger.Log(
		ctx, "config: secret resolution failed",
		"field", fieldPath,
		"err", err,
	)
}

// validate runs the post-layer required-field check. The current set of
// required fields is small (just [KeepConfig.BaseURL]); future fields
// should be added here with a clear error path so the operator sees
// exactly which key is missing.
//
// Validation runs AFTER secret resolution so a `Token` populated via
// the `*_secret` indirection passes the check. Validating before
// resolution would falsely flag every fresh boot.
func (c *Config) validate() error {
	if c.Keep.BaseURL == "" {
		return fmt.Errorf("%w: Keep.BaseURL", ErrMissingRequired)
	}
	return nil
}
