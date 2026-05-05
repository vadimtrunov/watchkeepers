//go:build integration

// Integration tests for the M4.4 human-identity Keep endpoints
// (POST /v1/humans, GET /v1/humans/by-slack/{slack_user_id}, and
// PATCH /v1/watchkeepers/{id}/lead). Requires a Postgres 16 reachable via
// KEEP_INTEGRATION_DB_URL with migrations 001..012 applied (CI wires this
// through the Keep Integration CI job). Reuses the helpers from
// read_integration_test.go (newTestEnv, bootKeep, doJSON, issuerForTest,
// mintToken, newUUID).
//
// Run locally with:
//
//	KEEP_INTEGRATION_DB_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
//	  go test -tags=integration -v -run 'TestHumanAPI_' ./core/cmd/keep/...
package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// humanIDResponse matches the `{"id":"<uuid>"}` envelope POST /v1/humans
// returns on 201.
type humanIDResponse struct {
	ID string `json:"id"`
}

// humanRowResponse matches the JSON shape returned by GET
// /v1/humans/by-slack/{slack_user_id}. Pointer fields mirror the
// nullable columns on the wire.
type humanRowResponse struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	DisplayName    string    `json:"display_name"`
	Email          *string   `json:"email"`
	SlackUserID    *string   `json:"slack_user_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// insertHumanBody is the wire-shape body POST /v1/humans accepts.
type insertHumanBody struct {
	OrganizationID string `json:"organization_id"`
	DisplayName    string `json:"display_name"`
	Email          string `json:"email,omitempty"`
	SlackUserID    string `json:"slack_user_id,omitempty"`
}

// setLeadBody is the wire-shape body PATCH /v1/watchkeepers/{id}/lead
// accepts.
type setLeadBody struct {
	LeadHumanID string `json:"lead_human_id"`
}

// TestHumanAPI_Insert_RoundTrip — POST /v1/humans creates a row whose
// slack_user_id is then resolvable via GET /v1/humans/by-slack/{id}.
func TestHumanAPI_Insert_RoundTrip(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	slackID := "U-" + env.subjectTag + "-rt"
	displayName := "Round Trip Human " + env.subjectTag

	// Insert.
	status, body := doJSON(t, http.MethodPost, "http://"+addr+"/v1/humans", tok,
		insertHumanBody{
			OrganizationID: env.orgID,
			DisplayName:    displayName,
			SlackUserID:    slackID,
		})
	if status != http.StatusCreated {
		t.Fatalf("insert status = %d; body = %s", status, body)
	}
	var created humanIDResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode insert: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("empty id; body=%s", body)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := env.pool.Exec(ctx,
			`DELETE FROM watchkeeper.human WHERE id = $1::uuid`, created.ID); err != nil {
			t.Logf("cleanup human %s: %v", created.ID, err)
		}
	})

	// Lookup by slack_user_id.
	status, body = doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/humans/by-slack/"+slackID, tok, nil)
	if status != http.StatusOK {
		t.Fatalf("lookup status = %d; body = %s", status, body)
	}
	var got humanRowResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("id = %q, want %q", got.ID, created.ID)
	}
	if got.SlackUserID == nil || *got.SlackUserID != slackID {
		t.Errorf("slack_user_id = %v, want %q", got.SlackUserID, slackID)
	}
	if got.DisplayName != displayName {
		t.Errorf("display_name = %q, want %q", got.DisplayName, displayName)
	}
	// Email was omitted from the insert body, so the column round-trips
	// as SQL NULL — the wire surfaces this as a JSON null, which decodes
	// to a nil *string.
	if got.Email != nil {
		t.Errorf("email = %v, want nil (SQL NULL)", got.Email)
	}
}

// TestHumanAPI_Insert_DuplicateSlackID_Conflict — a second insert with the
// same slack_user_id surfaces 409 slack_user_id_conflict from the unique
// constraint added in migration 012.
func TestHumanAPI_Insert_DuplicateSlackID_Conflict(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	slackID := "U-" + env.subjectTag + "-dup"

	// First insert succeeds.
	status, body := doJSON(t, http.MethodPost, "http://"+addr+"/v1/humans", tok,
		insertHumanBody{
			OrganizationID: env.orgID,
			DisplayName:    "First Human",
			SlackUserID:    slackID,
		})
	if status != http.StatusCreated {
		t.Fatalf("first insert status = %d; body = %s", status, body)
	}
	var created humanIDResponse
	_ = json.Unmarshal(body, &created)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := env.pool.Exec(ctx,
			`DELETE FROM watchkeeper.human WHERE id = $1::uuid`, created.ID); err != nil {
			t.Logf("cleanup human %s: %v", created.ID, err)
		}
	})

	// Second insert with the same slack_user_id must conflict.
	status, body = doJSON(t, http.MethodPost, "http://"+addr+"/v1/humans", tok,
		insertHumanBody{
			OrganizationID: env.orgID,
			DisplayName:    "Second Human",
			SlackUserID:    slackID,
		})
	if status != http.StatusConflict {
		t.Fatalf("second insert status = %d, want 409; body = %s", status, body)
	}
	var envErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Error != "slack_user_id_conflict" {
		t.Errorf("error = %q, want slack_user_id_conflict", envErr.Error)
	}
}

// TestHumanAPI_LookupBySlackID_NotFound — an unknown slack_user_id
// returns 404 not_found.
func TestHumanAPI_LookupBySlackID_NotFound(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	status, body := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/humans/by-slack/U-NOT-PRESENT-"+env.subjectTag, tok, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", status, body)
	}
}

// TestHumanAPI_SetLead_HappyPath — PATCH /v1/watchkeepers/{id}/lead
// rebinds the lead_human_id column; a follow-up GET reflects the new
// value.
func TestHumanAPI_SetLead_HappyPath(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	// Insert a fresh human row to act as the new lead. The seed already
	// has env.humanID; we deliberately rebind to a brand new row to
	// observe the column changing.
	newSlackID := "U-" + env.subjectTag + "-lead"
	insertStatus, insertBody := doJSON(t, http.MethodPost, "http://"+addr+"/v1/humans", tok,
		insertHumanBody{
			OrganizationID: env.orgID,
			DisplayName:    "New Lead " + env.subjectTag,
			SlackUserID:    newSlackID,
		})
	if insertStatus != http.StatusCreated {
		t.Fatalf("insert lead status = %d; body = %s", insertStatus, insertBody)
	}
	var created humanIDResponse
	_ = json.Unmarshal(insertBody, &created)
	if created.ID == "" {
		t.Fatalf("empty new lead id; body=%s", insertBody)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Re-bind back to the seed lead_human_id so the FK does not
		// block the human delete; this also exercises the rebind path
		// in cleanup.
		_, _ = env.pool.Exec(ctx, `
            UPDATE watchkeeper.watchkeeper SET lead_human_id = $2
            WHERE id = $1::uuid
        `, env.watchkeeperID, env.humanID)
		if _, err := env.pool.Exec(ctx,
			`DELETE FROM watchkeeper.human WHERE id = $1::uuid`, created.ID); err != nil {
			t.Logf("cleanup human %s: %v", created.ID, err)
		}
	})

	// PATCH the watchkeeper's lead.
	status, body := doJSON(t, http.MethodPatch,
		"http://"+addr+"/v1/watchkeepers/"+env.watchkeeperID+"/lead", tok,
		setLeadBody{LeadHumanID: created.ID})
	if status != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204; body = %s", status, body)
	}

	// Confirm the new lead is observed via the watchkeeper GET endpoint.
	getStatus, getBody := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/watchkeepers/"+env.watchkeeperID, tok, nil)
	if getStatus != http.StatusOK {
		t.Fatalf("get status = %d; body = %s", getStatus, getBody)
	}
	var got struct {
		LeadHumanID string `json:"lead_human_id"`
	}
	if err := json.Unmarshal(getBody, &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.LeadHumanID != created.ID {
		t.Errorf("lead_human_id = %q, want %q", got.LeadHumanID, created.ID)
	}
}

// TestHumanAPI_SetLead_UnknownHuman_400 — a lead_human_id that does not
// exist in watchkeeper.human surfaces as 400 invalid_lead_human_id (the
// FK violation translation) rather than a 500.
func TestHumanAPI_SetLead_UnknownHuman_400(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	bogusHumanID := newUUID(t)
	status, body := doJSON(t, http.MethodPatch,
		"http://"+addr+"/v1/watchkeepers/"+env.watchkeeperID+"/lead", tok,
		setLeadBody{LeadHumanID: bogusHumanID})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", status, body)
	}
	var envErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Error != "invalid_lead_human_id" {
		t.Errorf("error = %q, want invalid_lead_human_id", envErr.Error)
	}
}

// TestHumanAPI_SetLead_UnknownWatchkeeper_404 — patching a non-existent
// watchkeeper id returns 404 not_found.
func TestHumanAPI_SetLead_UnknownWatchkeeper_404(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	bogusWKID := newUUID(t)
	status, body := doJSON(t, http.MethodPatch,
		"http://"+addr+"/v1/watchkeepers/"+bogusWKID+"/lead", tok,
		setLeadBody{LeadHumanID: env.humanID})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", status, body)
	}
}
