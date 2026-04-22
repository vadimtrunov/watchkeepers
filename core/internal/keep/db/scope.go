// Package db glues the Keep read/write handlers to Postgres with the RLS
// and role-switching discipline the security model requires.
//
// The only exported surface at M2.7.b+c is WithScope, which wraps every
// read request in a short transaction that issues `SET LOCAL ROLE
// wk_<kind>_role` plus `SET LOCAL watchkeeper.scope = '<value>'` before
// running the caller's query function and committing. The
// per-transaction role switch is what activates the RLS policies from
// migration 005; the watchkeeper.scope GUC is what those policies match
// against.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// ErrBadScope is returned when claim.Scope is not "org", "user:<id>", or
// "agent:<id>". WithScope refuses to open a transaction in that case — a
// typed error before any DB work reduces the surface area for role-mapping
// bugs to leak into the policy layer.
var ErrBadScope = auth.ErrBadScope

// Role names are deliberately a fixed closed set rather than a computed
// string. Postgres does not allow parameterising `SET LOCAL ROLE`, so the
// only safe substitution strategy is "pick one of these exact literals";
// the mapping is declared here as the single source of truth.
const (
	roleOrg   = "wk_org_role"
	roleUser  = "wk_user_role"
	roleAgent = "wk_agent_role"
)

// RoleForScope maps a verified claim.Scope to the Postgres role the Keep
// service must SET LOCAL into for that request. The mapping is:
//
//	"org"         -> wk_org_role
//	"user:<id>"   -> wk_user_role
//	"agent:<id>"  -> wk_agent_role
//
// Anything else returns ErrBadScope. Exported so callers (middleware
// assertions, tests) can derive the role name without opening a tx.
func RoleForScope(scope string) (string, error) {
	switch {
	case scope == "org":
		return roleOrg, nil
	case len(scope) > len("user:") && scope[:len("user:")] == "user:":
		return roleUser, nil
	case len(scope) > len("agent:") && scope[:len("agent:")] == "agent:":
		return roleAgent, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrBadScope, scope)
	}
}

// WithScope opens a transaction on pool, issues `SET LOCAL ROLE` and
// `SET LOCAL watchkeeper.scope`, runs fn, and commits. Any error from fn
// triggers a rollback and is returned wrapped. The role name is validated
// against the closed set in RoleForScope before the statement is built, so
// the tiny bit of string interpolation that `SET LOCAL ROLE` requires
// cannot be driven by attacker-controlled input.
func WithScope(ctx context.Context, pool *pgxpool.Pool, claim auth.Claim, fn func(pgx.Tx) error) error {
	role, err := RoleForScope(claim.Scope)
	if err != nil {
		return err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// rollbackOnErr ensures we never leak a session-state-tainted tx back
	// to the pool. Commit() on a successful path makes Rollback() a no-op
	// per pgx semantics, so this is safe to defer unconditionally.
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// SET LOCAL ROLE cannot use a parameter placeholder in Postgres, so we
	// concatenate the role after validating it against the closed set
	// above. scope value flows through a placeholder because SET LOCAL on
	// a GUC *does* accept parameters.
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		return fmt.Errorf("set role %s: %w", role, err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('watchkeeper.scope', $1, true)", claim.Scope); err != nil {
		return fmt.Errorf("set watchkeeper.scope: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
