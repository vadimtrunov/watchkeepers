package manifest_test

import (
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/manifest"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

func TestK2KTokenBudget_NoImmutableCore(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{}
	got, ok := manifest.K2KTokenBudget(mf)
	if ok {
		t.Errorf("ok = true, want false (no ImmutableCore)")
	}
	if got != 0 {
		t.Errorf("got = %d, want 0", got)
	}
}

func TestK2KTokenBudget_NoCostLimits(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{}}
	got, ok := manifest.K2KTokenBudget(mf)
	if ok {
		t.Errorf("ok = true, want false (no CostLimits map)")
	}
	if got != 0 {
		t.Errorf("got = %d, want 0", got)
	}
}

func TestK2KTokenBudget_MissingKey(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{"per_task_tokens": float64(5000)},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if ok {
		t.Errorf("ok = true, want false (missing k2k_token_budget key)")
	}
	if got != 0 {
		t.Errorf("got = %d, want 0", got)
	}
}

func TestK2KTokenBudget_CoerceFloat64(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: float64(12345),
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if !ok {
		t.Errorf("ok = false, want true")
	}
	if got != 12345 {
		t.Errorf("got = %d, want 12345", got)
	}
}

func TestK2KTokenBudget_CoerceFloat64TruncatesFraction(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: float64(1000.7),
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if !ok {
		t.Errorf("ok = false, want true")
	}
	if got != 1000 {
		t.Errorf("got = %d, want 1000 (truncated)", got)
	}
}

func TestK2KTokenBudget_CoerceInt(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: int(42),
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if !ok || got != 42 {
		t.Errorf("(got, ok) = (%d, %v), want (42, true)", got, ok)
	}
}

func TestK2KTokenBudget_CoerceInt64(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: int64(99),
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if !ok || got != 99 {
		t.Errorf("(got, ok) = (%d, %v), want (99, true)", got, ok)
	}
}

func TestK2KTokenBudget_CoerceInt32(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: int32(7),
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if !ok || got != 7 {
		t.Errorf("(got, ok) = (%d, %v), want (7, true)", got, ok)
	}
}

func TestK2KTokenBudget_UnknownTypeReturnsFalse(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: "not-a-number",
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if ok {
		t.Errorf("ok = true, want false for string value")
	}
	if got != 0 {
		t.Errorf("got = %d, want 0", got)
	}
}

func TestK2KTokenBudget_NegativePropagatesVerbatim(t *testing.T) {
	t.Parallel()
	mf := runtime.Manifest{ImmutableCore: &runtime.ImmutableCore{
		CostLimits: map[string]any{
			manifest.K2KTokenBudgetCostLimitsKey: float64(-5),
		},
	}}
	got, ok := manifest.K2KTokenBudget(mf)
	if !ok {
		t.Errorf("ok = false, want true (negative value still decoded)")
	}
	if got != -5 {
		t.Errorf("got = %d, want -5 (helper returns raw value; budget.ResolveBudget clamps to 0)", got)
	}
}

func TestK2KTokenBudgetCostLimitsKey_PinnedValue(t *testing.T) {
	t.Parallel()
	if manifest.K2KTokenBudgetCostLimitsKey != "k2k_token_budget" {
		t.Errorf("K2KTokenBudgetCostLimitsKey = %q, want %q",
			manifest.K2KTokenBudgetCostLimitsKey, "k2k_token_budget")
	}
}
