package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/db"
)

func TestRoleForScope(t *testing.T) {
	cases := []struct {
		scope   string
		want    string
		wantErr bool
	}{
		{"org", "wk_org_role", false},
		{"user:abc", "wk_user_role", false},
		{"agent:abc", "wk_agent_role", false},
		{"user:", "", true},
		{"agent:", "", true},
		{"weird", "", true},
		{"", "", true},
		{"ORG", "", true}, // case-sensitive; we enforce lowercase.
	}
	for _, tc := range cases {
		t.Run(tc.scope, func(t *testing.T) {
			got, err := db.RoleForScope(tc.scope)
			if tc.wantErr {
				if err == nil {
					t.Errorf("RoleForScope(%q) = %q, want error", tc.scope, got)
				}
				if !errors.Is(err, auth.ErrBadScope) {
					t.Errorf("err = %v, want errors.Is auth.ErrBadScope", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RoleForScope(%q) error = %v", tc.scope, err)
			}
			if got != tc.want {
				t.Errorf("RoleForScope(%q) = %q, want %q", tc.scope, got, tc.want)
			}
		})
	}
}

// TestWithScope_BadScopeBeforeBegin asserts AC3's "Invalid scope format →
// typed error before tx open": we pass a nil pool which would panic if
// WithScope tried to open a transaction. The bad-scope branch must return
// before reaching the pool.
func TestWithScope_BadScopeBeforeBegin(t *testing.T) {
	// A nil *pgxpool.Pool dereferenced inside BeginTx would panic; the
	// test passes only because the scope check short-circuits first.
	err := db.WithScope(context.Background(), nil, auth.Claim{Scope: "weird"}, func(_ pgx.Tx) error {
		t.Fatal("fn must not run on bad scope")
		return nil
	})
	if err == nil {
		t.Fatal("WithScope with bad scope returned nil error")
	}
	if !errors.Is(err, auth.ErrBadScope) {
		t.Errorf("err = %v, want errors.Is auth.ErrBadScope", err)
	}
}
