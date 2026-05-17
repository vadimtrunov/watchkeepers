package server_test

// M3.3 — manifest_version metadata columns (reason, previous_version_id,
// proposer). The tests in this file exercise the round-trip wiring + the
// stable 400 reasons + the FK / CHECK error translations the
// `parsePutManifestVersionRequest` precheck and the
// `handlePutManifestVersion` Postgres-error switch own.
//
// Sibling of handlers_write_test.go on purpose — keeping the M3.3 tests
// in their own file makes the family-wise reading order clear and
// matches the M3.1 lessons recommendation: "every column extension under
// `watchkeeper.manifest_version` ... shares a load-bearing chain ... a
// preflight on the keepclient `PutManifestVersion` mirroring the SQL
// CHECK ... and a typed projection through `LoadManifest`." This file
// is the server-side share of that chain.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// Postgres SQLSTATE codes used in this test file. Mirrors the raw
// string-literal style of handlers_human_test.go which uses "23505" /
// "23503" inline rather than depending on the `pgerrcode` constants
// package (the project's repo has no transitive dependency on it).
const (
	pgCodeForeignKeyViolation = "23503" // class 23, constraint violation
	pgCodeCheckViolation      = "23514" // class 23, check constraint
)

// -----------------------------------------------------------------------
// PUT — happy-path round-trip with all three M3.3 metadata fields set.
// -----------------------------------------------------------------------

// TestPutManifestVersion_WithMetadata_201_RoundTrip — a PUT body carrying
// non-empty reason / previous_version_id / proposer is accepted; the
// runner sees each field bound at its INSERT slot verbatim (no
// re-shaping); the companion GET returns the fields verbatim on the wire.
// The previous_version_id slot binds via `stringOrNil` → SQL NULL on the
// empty case; non-empty values are cast to `uuid` in the SQL itself
// (see `$15::uuid` in the INSERT statement).
func TestPutManifestVersion_WithMetadata_201_RoundTrip(t *testing.T) {
	const (
		wantReason            = "lead requested rollback to last Friday's version"
		wantPreviousVersionID = "22222222-2222-4222-8222-222222222222"
		wantProposer          = "watchmaster"
	)

	var capturedReason any
	var capturedPreviousVersionID any
	var capturedProposer any
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		// After Phase 2 §M3.3 the INSERT placeholders run $1..$17:
		// reason is $14 → args[13], previous_version_id is $15 →
		// args[14], proposer is $16 → args[15]. The trailing
		// claim.OrganizationID is args[16]. Capture each bind verbatim
		// so a regression that reorders the SQL surfaces immediately.
		const (
			reasonArgIdx            = 13
			previousVersionIDArgIdx = 14
			proposerArgIdx          = 15
		)
		if len(args) > proposerArgIdx {
			capturedReason = args[reasonArgIdx]
			capturedPreviousVersionID = args[previousVersionIDArgIdx]
			capturedProposer = args[proposerArgIdx]
		}
		return server.NewFakeRow(func(dest ...any) error {
			if sp, ok := dest[0].(*string); ok {
				*sp = fakeUUID
			}
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	body := `{` +
		`"version_no":1,` +
		`"system_prompt":"ok",` +
		`"reason":` + jsonString(wantReason) + `,` +
		`"previous_version_id":"` + wantPreviousVersionID + `",` +
		`"proposer":` + jsonString(wantProposer) +
		`}`
	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		nil, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if capturedReason != wantReason {
		t.Errorf("INSERT reason = %v, want %q (stringOrNil must forward verbatim)", capturedReason, wantReason)
	}
	if capturedPreviousVersionID != wantPreviousVersionID {
		t.Errorf("INSERT previous_version_id = %v, want %q (stringOrNil + SQL ::uuid cast)", capturedPreviousVersionID, wantPreviousVersionID)
	}
	if capturedProposer != wantProposer {
		t.Errorf("INSERT proposer = %v, want %q (stringOrNil must forward verbatim)", capturedProposer, wantProposer)
	}

	// GET round-trip: the SELECT returns the same metadata fields
	// through the scan path. The response JSON must carry them verbatim.
	getQueryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			*dest[0].(*string) = fakeUUID
			*dest[1].(*string) = putManifestID
			*dest[2].(*int) = 1
			*dest[3].(*string) = "ok"
			*dest[4].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[5].(*json.RawMessage) = json.RawMessage(`{}`)
			*dest[6].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[7].(*string) = ""
			*dest[8].(*string) = ""
			*dest[9].(*string) = ""
			*dest[10].(*string) = ""
			*dest[11].(*int) = 0
			*dest[12].(*float64) = 0
			// dest[13] (**json.RawMessage) untouched: immutable_core NULL.
			*dest[14].(*string) = wantReason
			// previous_version_id scans into **string; allocate the
			// string and promote the pointer so the response field is
			// populated non-nil.
			prev := wantPreviousVersionID
			*dest[15].(**string) = &prev
			*dest[16].(*string) = wantProposer
			*dest[17].(*time.Time) = time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
			return nil
		})
	}
	getRunner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: getQueryRow})}
	gh, gti := writeRouterForTest(t, mustFixedNow(), getRunner)
	gtok := mustMintToken(t, gti, "org")

	greq := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/manifests/"+putManifestID, nil)
	greq.Header.Set("Authorization", "Bearer "+gtok)
	grec := httptest.NewRecorder()
	gh.ServeHTTP(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", grec.Code, grec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET decode: %v", err)
	}
	if got["reason"] != wantReason {
		t.Errorf("GET reason = %v, want %q; body=%s", got["reason"], wantReason, grec.Body.String())
	}
	if got["previous_version_id"] != wantPreviousVersionID {
		t.Errorf("GET previous_version_id = %v, want %q; body=%s", got["previous_version_id"], wantPreviousVersionID, grec.Body.String())
	}
	if got["proposer"] != wantProposer {
		t.Errorf("GET proposer = %v, want %q; body=%s", got["proposer"], wantProposer, grec.Body.String())
	}
}

// -----------------------------------------------------------------------
// PUT — fields omitted → SQL NULL → response omits keys (omitempty).
// -----------------------------------------------------------------------

// TestPutManifestVersion_MetadataOmitted_GetHasNoMetadataKeys — a PUT
// body that omits the three M3.3 metadata fields binds SQL NULL for each
// (via `stringOrNil`) and the companion GET drops the keys from the
// wire response (`omitempty` for reason/proposer; pointer + `omitempty`
// for previous_version_id). Mirrors the wire-omit posture of
// `personality` / `language` / `model` / `autonomy` / `immutable_core`.
func TestPutManifestVersion_MetadataOmitted_GetHasNoMetadataKeys(t *testing.T) {
	// Step 1: PUT without metadata. Capture each M3.3 bind slot and
	// assert it is the untyped nil that `stringOrNil("")` returns. The
	// nilArg counting in TestPutManifestVersion_*_GetHasNo*Key (with
	// wantNilArgs=13) already pins the aggregate count; this test pins
	// the three specific slot positions so a regression that swaps
	// reason ↔ previous_version_id ↔ proposer at INSERT time still
	// catches.
	const (
		reasonArgIdx            = 13
		previousVersionIDArgIdx = 14
		proposerArgIdx          = 15
	)
	var capturedReason any
	var capturedPreviousVersionID any
	var capturedProposer any
	captured := false
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		if len(args) > proposerArgIdx {
			capturedReason = args[reasonArgIdx]
			capturedPreviousVersionID = args[previousVersionIDArgIdx]
			capturedProposer = args[proposerArgIdx]
			captured = true
		}
		return server.NewFakeRow(func(dest ...any) error {
			if sp, ok := dest[0].(*string); ok {
				*sp = fakeUUID
			}
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
		}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !captured {
		t.Fatalf("queryRow not invoked — runner wiring broken")
	}
	if capturedReason != nil {
		t.Errorf("reason bind = %v, want nil (omitted → stringOrNil)", capturedReason)
	}
	if capturedPreviousVersionID != nil {
		t.Errorf("previous_version_id bind = %v, want nil (omitted → stringOrNil)", capturedPreviousVersionID)
	}
	if capturedProposer != nil {
		t.Errorf("proposer bind = %v, want nil (omitted → stringOrNil)", capturedProposer)
	}

	// Step 2: GET — SELECT returns coalesce(reason, '') / NULL
	// previous_version_id / coalesce(proposer, '') so the response
	// JSON must NOT carry any of the three keys.
	getQueryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			*dest[0].(*string) = fakeUUID
			*dest[1].(*string) = putManifestID
			*dest[2].(*int) = 1
			*dest[3].(*string) = "ok"
			*dest[4].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[5].(*json.RawMessage) = json.RawMessage(`{}`)
			*dest[6].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[7].(*string) = ""
			*dest[8].(*string) = ""
			*dest[9].(*string) = ""
			*dest[10].(*string) = ""
			*dest[11].(*int) = 0
			*dest[12].(*float64) = 0
			// dest[13] (immutable_core, **json.RawMessage) untouched.
			*dest[14].(*string) = ""
			// dest[15] (previous_version_id, **string) untouched — SQL NULL.
			*dest[16].(*string) = ""
			*dest[17].(*time.Time) = time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
			return nil
		})
	}
	getRunner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: getQueryRow})}
	gh, gti := writeRouterForTest(t, mustFixedNow(), getRunner)
	gtok := mustMintToken(t, gti, "org")

	greq := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/manifests/"+putManifestID, nil)
	greq.Header.Set("Authorization", "Bearer "+gtok)
	grec := httptest.NewRecorder()
	gh.ServeHTTP(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", grec.Code, grec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET decode: %v", err)
	}
	for _, k := range []string{"reason", "previous_version_id", "proposer"} {
		if _, present := got[k]; present {
			t.Errorf("GET response contains %q key; expected omitted via omitempty", k)
		}
	}
}

// -----------------------------------------------------------------------
// PUT — `reason` cap (1024 codepoints).
// -----------------------------------------------------------------------

// TestPutManifestVersion_ReasonTooLong_400 — the server CHECK constraint
// caps `reason` at 1024 Unicode codepoints; the precheck surfaces
// `reason_too_long` BEFORE the row hits Postgres so the caller sees a
// stable 400 reason rather than a 23514 check_violation. Both an
// ASCII-1025 case and a 1025-rune CJK case must be rejected — a
// byte-count cap (`len(s)`) would let the CJK case through but
// `utf8.RuneCountInString` does not.
func TestPutManifestVersion_ReasonTooLong_400(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"ascii_1025", strings.Repeat("a", 1025)},
		{"cjk_1025", strings.Repeat("漢", 1025)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/"+putManifestID+"/versions", tok,
				map[string]any{
					"version_no":    1,
					"system_prompt": "ok",
					"reason":        tc.reason,
				}, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner was invoked; expected rejection before tx")
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "reason_too_long" {
				t.Errorf("error = %q, want reason_too_long", env.Error)
			}
		})
	}
}

// TestPutManifestVersion_ReasonAt1024_201 — boundary: exactly
// 1024 Unicode codepoints is accepted (the CHECK uses `<= 1024`).
// Pins the off-by-one between `< 1024` and `<= 1024`. Both ASCII +
// CJK boundary cases are exercised so the rune-not-byte semantics
// stay pinned.
func TestPutManifestVersion_ReasonAt1024_201(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"ascii_1024", strings.Repeat("a", 1024)},
		{"cjk_1024", strings.Repeat("漢", 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{FakeID: fakeUUID}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/"+putManifestID+"/versions", tok,
				map[string]any{
					"version_no":    1,
					"system_prompt": "ok",
					"reason":        tc.reason,
				}, "")
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// -----------------------------------------------------------------------
// PUT — `proposer` cap (256 codepoints).
// -----------------------------------------------------------------------

// TestPutManifestVersion_ProposerTooLong_400 — the server CHECK
// constraint caps `proposer` at 256 Unicode codepoints; the precheck
// surfaces `proposer_too_long` before the row hits Postgres.
func TestPutManifestVersion_ProposerTooLong_400(t *testing.T) {
	cases := []struct {
		name     string
		proposer string
	}{
		{"ascii_257", strings.Repeat("p", 257)},
		{"cjk_257", strings.Repeat("漢", 257)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/"+putManifestID+"/versions", tok,
				map[string]any{
					"version_no":    1,
					"system_prompt": "ok",
					"proposer":      tc.proposer,
				}, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner was invoked; expected rejection before tx")
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "proposer_too_long" {
				t.Errorf("error = %q, want proposer_too_long", env.Error)
			}
		})
	}
}

// TestPutManifestVersion_ProposerAt256_201 — boundary: exactly 256
// Unicode codepoints is accepted.
func TestPutManifestVersion_ProposerAt256_201(t *testing.T) {
	cases := []struct {
		name     string
		proposer string
	}{
		{"ascii_256", strings.Repeat("p", 256)},
		{"cjk_256", strings.Repeat("漢", 256)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{FakeID: fakeUUID}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/"+putManifestID+"/versions", tok,
				map[string]any{
					"version_no":    1,
					"system_prompt": "ok",
					"proposer":      tc.proposer,
				}, "")
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// -----------------------------------------------------------------------
// PUT — `previous_version_id` shape rejection.
// -----------------------------------------------------------------------

// TestPutManifestVersion_PreviousVersionIDMalformed_400 — a non-UUID
// `previous_version_id` value surfaces the stable 400 reason
// `invalid_previous_version_id` BEFORE Postgres sees the row so the
// caller does not get an opaque `22P02 invalid_text_representation`
// cast error. Mirrors the M5.5.b.* `invalid_<field>` precedents.
func TestPutManifestVersion_PreviousVersionIDMalformed_400(t *testing.T) {
	cases := []struct {
		name string
		v    string
	}{
		{"not_uuid", "not-a-uuid"},
		{"short_hex", "11111111-1111"},
		{"trailing_garbage", "11111111-1111-4111-8111-111111111111-extra"},
		{"empty_braces", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/"+putManifestID+"/versions", tok,
				map[string]any{
					"version_no":          1,
					"system_prompt":       "ok",
					"previous_version_id": tc.v,
				}, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner was invoked; expected rejection before tx")
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "invalid_previous_version_id" {
				t.Errorf("error = %q, want invalid_previous_version_id", env.Error)
			}
		})
	}
}

// -----------------------------------------------------------------------
// PUT — `previous_version_id` FK target row missing → stable 400.
// -----------------------------------------------------------------------

// TestPutManifestVersion_PreviousVersionIDUnknown_400 — defense-in-depth
// path: if the INSERT's own NOT EXISTS gate for same-manifest is
// somehow bypassed (e.g. a race between the gate's SELECT and the FK
// check), the FK violation `manifest_version_previous_version_id_fkey`
// surfaces as a Postgres `23503 foreign_key_violation`. The handler
// translates that to a stable 400 `unknown_previous_version_id` so the
// M3.4 `manifest.rollback` tool can distinguish "no such version" from
// a generic 500. In production the same-manifest WHERE EXISTS gate
// catches the missing-row case first (returning pgx.ErrNoRows → 404
// not_found, exercised in TestPutManifestVersion_PreviousVersionIDCrossManifest_404).
func TestPutManifestVersion_PreviousVersionIDUnknown_400(t *testing.T) {
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRowErr(&pgconn.PgError{
			Code:           pgCodeForeignKeyViolation,
			ConstraintName: "manifest_version_previous_version_id_fkey",
			Message:        "insert violates foreign key constraint",
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":          1,
			"system_prompt":       "ok",
			"previous_version_id": "22222222-2222-4222-8222-222222222222",
		}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "unknown_previous_version_id" {
		t.Errorf("error = %q, want unknown_previous_version_id", env.Error)
	}
}

// TestPutManifestVersion_PreviousVersionIDCrossManifest_404 — the M3.3
// same-manifest invariant for previous_version_id. The INSERT's WHERE
// clause includes a NOT-NULL-implies-EXISTS-same-manifest subquery
// (`$15::uuid IS NULL OR EXISTS (... AND manifest_id = $1)`) so a row
// that names a previous_version_id whose target row lives under a
// DIFFERENT manifest_id never lands in the table. Postgres returns
// no row through RETURNING → pgx.ErrNoRows → 404 not_found, mirroring
// the cross-tenant rejection contract documented on
// `handleInsertWatchkeeper`. This is the schema-write-path enforcement
// of the audit-chain invariant the M3.4 `manifest.rollback` tool
// relies on; without it, a caller in org A could persist a
// previous_version_id pointing at org B's row (the FK is unscoped) —
// codex iter-1 P1.
func TestPutManifestVersion_PreviousVersionIDCrossManifest_404(t *testing.T) {
	// The fake returns pgx.ErrNoRows: in production the WHERE clause
	// filters out the row (either the manifest does not belong to the
	// claim's tenant, OR the previous_version_id row's manifest_id
	// doesn't match $1). Either branch lands the same surface — the
	// handler maps both to 404 not_found by design.
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":          1,
			"system_prompt":       "ok",
			"previous_version_id": "22222222-2222-4222-8222-222222222222",
		}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "not_found" {
		t.Errorf("error = %q, want not_found", env.Error)
	}
}

// TestPutManifestVersion_PreviousVersionSelfRef_400 — the
// `manifest_version_previous_version_self_ref` CHECK constraint forbids
// a row that points its `previous_version_id` at its own `id`. Since the
// insert generates the row's id via `gen_random_uuid()` the precheck
// cannot catch this client-side; the handler maps the 23514 check
// violation to a stable 400 `invalid_previous_version_id` so the
// caller's contract stays narrow. (In practice the FK + the M3.4 tools'
// own logic make this case improbable, but the handler must still
// translate the error cleanly rather than 500.)
func TestPutManifestVersion_PreviousVersionSelfRef_400(t *testing.T) {
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRowErr(&pgconn.PgError{
			Code:           pgCodeCheckViolation,
			ConstraintName: "manifest_version_previous_version_self_ref",
			Message:        "check violation",
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":          1,
			"system_prompt":       "ok",
			"previous_version_id": "33333333-3333-4333-8333-333333333333",
		}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_previous_version_id" {
		t.Errorf("error = %q, want invalid_previous_version_id", env.Error)
	}
}

// jsonString returns s as a JSON string literal so callers can embed it
// in a raw JSON-body fixture without worrying about quoting / escape
// rules. Used by the tests in this file that build a raw body
// (`"{...}"`) rather than passing a `map[string]any` through
// json.Marshal.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
