package budget

import "errors"

// ErrInvalidChargeParams is returned by [Enforcer.Charge] when the
// supplied [ChargeParams] are degenerate (zero conversation id, zero
// organization id, non-positive delta, negative budget). Hoisted to a
// single sentinel so the caller branches on errors.Is rather than parsing
// the formatted error chain. Mirrors the M1.3.d `ErrInvalidBroadcastConcurrency`
// + M1.4 `ErrInvalidEvent` per-package validator-sentinel discipline.
var ErrInvalidChargeParams = errors.New("budget: invalid charge params")
