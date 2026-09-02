package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Leaving the managed class, end to end against the bind rig: the disconnect
// verbs, and the door guards that keep every other credential bind from
// leaving a workspace's live installation rows under a class that says it has
// no App.
//
// One assertion runs after every transition here: a workspace has live managed
// rows if and only if its class is managed_app. A verb that moved one without
// the other is the failure this file exists to catch, and a status code alone
// would not catch it.

// bindManaged runs the ceremony for each installation id, on a distinct
// account per id, and returns with every one of them bound.
func (r *bindRig) bindManaged(t *testing.T, ids ...int64) {
	t.Helper()
	r.gh.userInstallations = append([]int64(nil), ids...)
	for i, id := range ids {
		r.gh.installationID = id
		r.gh.accountLogin = fmt.Sprintf("acme-%d", id)
		r.gh.accountID = 700 + int64(i)
		if out := r.callback(t, r.ceremony(t), defaultCallbackQuery(id)); out.Code != http.StatusFound {
			t.Fatalf("bind %d status=%d outcome=%q, want 302", id, out.Code, out.Header().Get("X-TF-Bind-Outcome"))
		}
	}
	r.assertRowsAgreeWithClass(t)
}

// disconnect drives the whole-workspace verb (only == "") or the narrowed one.
func (r *bindRig) disconnect(t *testing.T, only string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/orgs/" + r.orgID.String() + "/github/managed/disconnect"
	if only != "" {
		path = "/api/orgs/" + r.orgID.String() + "/github/managed/installations/" + only + "/disconnect"
	}
	req := httptest.NewRequest("POST", path, nil)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: r.sid})
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// liveInstallations reads the org's live installation ids.
func (r *bindRig) liveInstallations(t *testing.T) []string {
	t.Helper()
	rows, err := r.h.AdminDB.Query(`
		SELECT installation_id FROM org_github_app_installations
		 WHERE org_id = $1 AND removed_at IS NULL
		 ORDER BY installation_id
	`, r.orgID.String())
	if err != nil {
		t.Fatalf("list installations: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan installation: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// assertRowsAgreeWithClass is the invariant: live rows iff managed_app.
func (r *bindRig) assertRowsAgreeWithClass(t *testing.T) {
	t.Helper()
	live := r.liveInstallations(t)
	class := r.credentialClass(t)
	managed := class == string(domain.GitHubCredentialClassManagedApp)
	if managed != (len(live) > 0) {
		t.Errorf("rows and class disagree: class=%q, live rows=%v", class, live)
	}
}

// storedPAT reads the org's GitHub PAT out of the secret store, "" for none.
func (r *bindRig) storedPAT(t *testing.T) string {
	t.Helper()
	var pat string
	if err := r.srv.tx.WithTx(context.Background(), r.orgID.String(), r.userID.String(), func(tx db.TxStores) error {
		loaded, err := integrations.Load(context.Background(), tx.Secrets, r.orgID.String())
		if err != nil {
			return err
		}
		pat = loaded.GitHubPAT
		return nil
	}); err != nil {
		t.Fatalf("load org credentials: %v", err)
	}
	return pat
}

// hasAppRow reports whether the org holds its own App registration.
func (r *bindRig) hasAppRow(t *testing.T) bool {
	t.Helper()
	app, err := r.srv.githubApps.GetForOrgSystem(context.Background(), r.orgID.String())
	if err != nil {
		t.Fatalf("read app row: %v", err)
	}
	return app != nil
}

// accessChanges returns the detail_json of every access-change row with the
// given action, oldest first.
func (r *bindRig) accessChanges(t *testing.T, action string) []string {
	t.Helper()
	rows, err := r.h.AdminDB.Query(`
		SELECT detail_json FROM access_change_log
		 WHERE org_id = $1 AND action = $2
		 ORDER BY created_at, id
	`, r.orgID.String(), action)
	if err != nil {
		t.Fatalf("list access changes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d sql.NullString
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan access change: %v", err)
		}
		out = append(out, d.String)
	}
	return out
}

// seedReach plants a reachable-repo row and a scope marker under the
// installation, the way the reach refresh would, so the cascade has something
// to take with it.
func (r *bindRig) seedReach(t *testing.T, installationID string) {
	t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO reachable_repositories (org_id, credential_class, installation_id, owner, repo)
		VALUES ($1, 'managed_app', $2, 'acme', 'widgets')
	`, r.orgID.String(), installationID); err != nil {
		t.Fatalf("seed reachable_repositories: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO reachable_scopes (org_id, credential_class, scope)
		VALUES ($1, 'managed_app', $2)
	`, r.orgID.String(), installationID); err != nil {
		t.Fatalf("seed reachable_scopes: %v", err)
	}
}

func (r *bindRig) reachRows(t *testing.T, installationID string) (repos, scopes int) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(`
		SELECT count(*) FROM reachable_repositories WHERE org_id = $1 AND installation_id = $2
	`, r.orgID.String(), installationID).Scan(&repos); err != nil {
		t.Fatalf("count reachable_repositories: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		SELECT count(*) FROM reachable_scopes WHERE org_id = $1 AND scope = $2
	`, r.orgID.String(), installationID).Scan(&scopes); err != nil {
		t.Fatalf("count reachable_scopes: %v", err)
	}
	return repos, scopes
}

// errorMessage reads the first error's message off a JSON error body.
func errorMessage(t *testing.T, body io.Reader) string {
	t.Helper()
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if len(out.Errors) == 0 {
		t.Fatal("error body carries no errors")
	}
	return out.Errors[0].Message
}

// --- The disconnect verb ---------------------------------------------------

func TestManagedDisconnect_RowsRemovedClassResetChangeRecorded(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	rig.bindManaged(t, 4242)
	rig.seedReach(t, "4242")

	// A cached installation token is what an installation.deleted delivery
	// also cuts short; the verb owes the same invalidation.
	var invalidated []string
	rig.srv.onInstallationTokensInvalid = func(orgID, installationID string) {
		invalidated = append(invalidated, orgID+"/"+installationID)
	}

	rec := rig.disconnect(t, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if live := rig.liveInstallations(t); len(live) != 0 {
		t.Errorf("live rows after disconnect = %v, want none", live)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassPAT) {
		t.Errorf("class after disconnect = %q, want %q (the rowless default)", class, domain.GitHubCredentialClassPAT)
	}
	rig.assertRowsAgreeWithClass(t)

	// Soft-removed, not deleted: the row survives with removed_at stamped, so
	// a later re-bind revives it exactly as a reinstall would.
	var removed int
	if err := rig.h.AdminDB.QueryRow(`
		SELECT count(*) FROM org_github_app_installations
		 WHERE org_id = $1 AND installation_id = '4242' AND removed_at IS NOT NULL
	`, rig.orgID.String()).Scan(&removed); err != nil {
		t.Fatalf("count removed rows: %v", err)
	}
	if removed != 1 {
		t.Errorf("%d soft-removed rows for 4242, want 1", removed)
	}

	if repos, scopes := rig.reachRows(t, "4242"); repos != 0 || scopes != 0 {
		t.Errorf("reach cascade did not fire: %d reachable_repositories, %d reachable_scopes remain", repos, scopes)
	}
	if want := []string{rig.orgID.String() + "/4242"}; fmt.Sprint(invalidated) != fmt.Sprint(want) {
		t.Errorf("token invalidations = %v, want %v", invalidated, want)
	}

	removals := rig.accessChanges(t, domain.AccessActionCredentialRemoved)
	if len(removals) != 1 {
		t.Fatalf("%d credential_removed rows, want 1: %v", len(removals), removals)
	}
	for _, want := range []string{`"kind":"github_app"`, `"name":"acme-4242"`} {
		if !strings.Contains(removals[0], want) {
			t.Errorf("access change %s lacks %s — the removal must be named by the account it disconnects", removals[0], want)
		}
	}
}

// TestManagedDisconnect_ReceiverStopsRouting is the consequence the hole was
// about: a delivery for the formerly bound installation is acknowledged as
// unbound the moment the row is gone, with nothing changed in the receiver.
func TestManagedDisconnect_ReceiverStopsRouting(t *testing.T) {
	gh := newFakeGitHub()
	rig := newBindRig(t, gh)
	rig.bindManaged(t, 4242)
	got := captureWebhookBus(rig.srv)
	sign := func(body []byte) string { return signWith(rig.srv.deploymentApp.WebhookSecret, body) }

	body := []byte(boundPRBody)
	if rec := postDeploymentWebhookDelivery(rig.srv, "pull_request", sign(body), body, "before-disconnect"); rec.Code != http.StatusNoContent {
		t.Fatalf("bound delivery status=%d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if e := expectPublish(t, got, "webhook:github:pull_request"); e.OrgID != rig.orgID.String() {
		t.Fatalf("bound delivery published under %q, want %q", e.OrgID, rig.orgID)
	}

	if rec := rig.disconnect(t, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}

	if rec := postDeploymentWebhookDelivery(rig.srv, "pull_request", sign(body), body, "after-disconnect"); rec.Code != http.StatusNoContent {
		t.Fatalf("delivery after disconnect status=%d, want 204 (acknowledged as unbound); body=%s", rec.Code, rec.Body.String())
	}
	expectNoPublish(t, got)
}

func TestManagedDisconnect_IsIdempotent(t *testing.T) {
	rig := newBindRig(t, newFakeGitHub())
	rig.bindManaged(t, 4242)

	if rec := rig.disconnect(t, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first disconnect status=%d, want 204", rec.Code)
	}
	if rec := rig.disconnect(t, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("second disconnect status=%d body=%s, want 204 — a no-op, not a 404", rec.Code, rec.Body.String())
	}
	rig.assertRowsAgreeWithClass(t)
	if n := len(rig.accessChanges(t, domain.AccessActionCredentialRemoved)); n != 1 {
		t.Errorf("%d credential_removed rows after two disconnects, want 1 — a removal that removed nothing is not an access change", n)
	}

	// A workspace that never bound anything is the same no-op.
	fresh := rig.sibling(t, "never-bound")
	if rec := fresh.disconnect(t, ""); rec.Code != http.StatusNoContent {
		t.Errorf("disconnect on a never-bound workspace status=%d, want 204", rec.Code)
	}
}

func TestManagedDisconnect_PerInstallation(t *testing.T) {
	rig := newBindRig(t, newFakeGitHub())
	rig.bindManaged(t, 4242, 5150)

	// Dropping one of two accounts keeps the class.
	if rec := rig.disconnect(t, "5150"); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind 5150 status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if live := rig.liveInstallations(t); fmt.Sprint(live) != "[4242]" {
		t.Errorf("live rows after unbinding 5150 = %v, want [4242]", live)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("class after unbinding one of two = %q, want managed_app", class)
	}
	rig.assertRowsAgreeWithClass(t)

	// The row is the resource: one this workspace does not hold live is 404,
	// whether it was just unbound or never bound at all.
	if rec := rig.disconnect(t, "5150"); rec.Code != http.StatusNotFound {
		t.Errorf("unbind of an already-unbound installation status=%d, want 404", rec.Code)
	}
	if rec := rig.disconnect(t, "999999"); rec.Code != http.StatusNotFound {
		t.Errorf("unbind of an installation never bound status=%d, want 404", rec.Code)
	}
	if rec := rig.disconnect(t, "not-a-number"); rec.Code != http.StatusBadRequest {
		t.Errorf("unbind with a malformed id status=%d, want 400", rec.Code)
	}
	if live := rig.liveInstallations(t); fmt.Sprint(live) != "[4242]" {
		t.Errorf("refused unbinds touched the rows: %v", live)
	}

	// Dropping the last one is the full disconnect.
	if rec := rig.disconnect(t, "4242"); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind 4242 status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if live := rig.liveInstallations(t); len(live) != 0 {
		t.Errorf("live rows after unbinding the last = %v, want none", live)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassPAT) {
		t.Errorf("class after unbinding the last = %q, want pat", class)
	}
	rig.assertRowsAgreeWithClass(t)
	if n := len(rig.accessChanges(t, domain.AccessActionCredentialRemoved)); n != 2 {
		t.Errorf("%d credential_removed rows, want 2 — one per account disconnected", n)
	}
}

// TestManagedDisconnect_NeverTouchesAnotherWorkspace: the verb answers about
// this workspace's rows only. A sibling holding its own installation keeps it
// through the caller's disconnect, and the caller cannot name the sibling's
// installation through the narrowed verb.
func TestManagedDisconnect_NeverTouchesAnotherWorkspace(t *testing.T) {
	gh := newFakeGitHub()
	first := newBindRig(t, gh)
	first.bindManaged(t, 4242)
	second := first.sibling(t, "rival-org")
	second.bindManaged(t, 5150)

	if rec := first.disconnect(t, "5150"); rec.Code != http.StatusNotFound {
		t.Errorf("unbinding a sibling's installation status=%d, want 404", rec.Code)
	}
	if rec := first.disconnect(t, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect status=%d, want 204", rec.Code)
	}
	if live := second.liveInstallations(t); fmt.Sprint(live) != "[5150]" {
		t.Errorf("sibling's rows after the caller's disconnect = %v, want [5150]", live)
	}
	if class := second.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
		t.Errorf("sibling's class after the caller's disconnect = %q, want managed_app", class)
	}
	second.assertRowsAgreeWithClass(t)
}

func TestManagedDisconnect_RefusesABYOWorkspace(t *testing.T) {
	// A BYO workspace's installation rows belong to its own App and are torn
	// down by its own switch flow. The managed verb refuses rather than
	// no-ops: a 204 would say the workspace left a class it was never in.
	rig := newBindRig(t, newFakeGitHub())
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, github_credential_class) VALUES ($1, 'byo_app')
		ON CONFLICT (org_id) DO UPDATE SET github_credential_class = 'byo_app'
	`, rig.orgID.String()); err != nil {
		t.Fatalf("seed byo class: %v", err)
	}
	rec := rig.disconnect(t, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("disconnect on a byo_app workspace status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassBYOApp) {
		t.Errorf("class after refused disconnect = %q, want byo_app", class)
	}
}

func TestManagedDisconnect_RequiresWorkspaceAdmin(t *testing.T) {
	rig := newBindRig(t, newFakeGitHub())
	rig.bindManaged(t, 4242)

	member := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'member')
	`, rig.orgID.String(), member.String()); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	resp, _ := rig.driveCallback(member)
	req := httptest.NewRequest("POST", "/api/orgs/"+rig.orgID.String()+"/github/managed/disconnect", nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sidFromResp(resp)})
	req.Header.Set("Origin", rig.srv.deployCfg.publicURL)
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("disconnect by a non-admin member status=%d, want 403", rec.Code)
	}
	if live := rig.liveInstallations(t); fmt.Sprint(live) != "[4242]" {
		t.Errorf("a refused disconnect touched the rows: %v", live)
	}
	rig.assertRowsAgreeWithClass(t)
}

func TestManagedDisconnect_LocalModeIs404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	for _, path := range []string{
		"/api/orgs/" + runmode.LocalDefaultOrgID + "/github/managed/disconnect",
		"/api/orgs/" + runmode.LocalDefaultOrgID + "/github/managed/installations/4242/disconnect",
	} {
		rec := doJSON(t, s, "POST", path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s in local mode = %d, want 404", path, rec.Code)
		}
	}
}

// --- The door guards -------------------------------------------------------

// TestManagedClass_DoorsRefuseAManagedWorkspace is the test set that catches
// the hole: before the guards, a managed workspace — known class, no App row —
// walked through every other credential bind, acquired the credential, and
// kept its bound installation rows live under a class that said it had no App.
// Each door refuses now, names the disconnect as the way out, and writes
// nothing: no PAT stored, no App row, class and rows untouched.
func TestManagedClass_DoorsRefuseAManagedWorkspace(t *testing.T) {
	assertUntouched := func(t *testing.T, rig *bindRig) {
		t.Helper()
		if live := rig.liveInstallations(t); fmt.Sprint(live) != "[4242]" {
			t.Errorf("live rows after a refused bind = %v, want [4242]", live)
		}
		if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassManagedApp) {
			t.Errorf("class after a refused bind = %q, want managed_app", class)
		}
		if pat := rig.storedPAT(t); pat != "" {
			t.Errorf("a refused bind stored a PAT")
		}
		if rig.hasAppRow(t) {
			t.Errorf("a refused bind wrote an App row")
		}
		rig.assertRowsAgreeWithClass(t)
	}

	t.Run("pat_bind", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		rig.bindManaged(t, 4242)
		calls := rig.gh.callCount()

		resp := rig.postJSONWithSid("PUT", "/api/orgs/"+rig.orgID.String()+"/github/pat", rig.sid, map[string]string{"pat": "ghp_new"})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("PAT bind on a managed workspace status=%d, want 409", resp.StatusCode)
		}
		if msg := errorMessage(t, resp.Body); msg != managedInTheWayMessage {
			t.Errorf("refusal message = %q, want %q", msg, managedInTheWayMessage)
		}
		if rig.gh.callCount() != calls {
			t.Error("the PAT was validated against GitHub before the guard refused; the advisory guard must fire first")
		}
		assertUntouched(t, rig)
	})

	t.Run("byo_register_start", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		rig.bindManaged(t, 4242)

		req := httptest.NewRequest("GET", "/api/orgs/"+rig.orgID.String()+"/github/app/register/launch?owner_type=org&owner_login=acme", nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sid})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("register launch on a managed workspace status=%d, want 409", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Disconnect it") {
			t.Errorf("launch refusal page does not name the disconnect as the way out: %s", rec.Body.String())
		}
		assertUntouched(t, rig)
	})

	t.Run("byo_register_callback", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		rig.bindManaged(t, 4242)
		calls := rig.gh.callCount()

		state := appRegisterState{OrgID: rig.orgID.String(), OwnerType: "org", ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
		signed, err := state.sign(rig.srv.deployCfg.hmacKey)
		if err != nil {
			t.Fatalf("sign state: %v", err)
		}
		req := httptest.NewRequest("GET", "/api/orgs/"+rig.orgID.String()+"/github/app/register/callback?code=test_code&state="+signed, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: rig.sid})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("register callback on a managed workspace status=%d body=%s, want 409", rec.Code, rec.Body.String())
		}
		if msg := errorMessage(t, rec.Body); msg != managedInTheWayMessage {
			t.Errorf("refusal message = %q, want %q", msg, managedInTheWayMessage)
		}
		if rig.gh.callCount() != calls {
			t.Error("the manifest code was exchanged with GitHub before the guard refused")
		}
		assertUntouched(t, rig)
	})

	t.Run("byo_import", func(t *testing.T) {
		rig := newBindRig(t, newFakeGitHub())
		rig.bindManaged(t, 4242)
		calls := rig.gh.callCount()

		resp := rig.postJSONWithSid("POST", "/api/orgs/"+rig.orgID.String()+"/github/app/import", rig.sid,
			map[string]string{"app_id": "777", "pem": "-----BEGIN RSA PRIVATE KEY-----\nnot-a-key\n-----END RSA PRIVATE KEY-----"})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("import on a managed workspace status=%d, want 409", resp.StatusCode)
		}
		if msg := errorMessage(t, resp.Body); msg != managedInTheWayMessage {
			t.Errorf("refusal message = %q, want %q", msg, managedInTheWayMessage)
		}
		if rig.gh.callCount() != calls {
			t.Error("the App was validated against GitHub before the guard refused; the advisory guard must fire first")
		}
		assertUntouched(t, rig)
	})

	t.Run("a_managed_workspace_with_no_live_row_is_not_in_the_way", func(t *testing.T) {
		// The guard is about live rows, not the class alone: once every row is
		// gone (an uninstall reported by webhook, say) the workspace is free
		// to become the plain PAT workspace it already effectively is.
		rig := newBindRig(t, newFakeGitHub())
		rig.bindManaged(t, 4242)
		if _, err := rig.srv.githubApps.MarkInstallationRemoved(context.Background(), rig.orgID.String(), "4242"); err != nil {
			t.Fatalf("remove installation: %v", err)
		}
		resp := rig.postJSONWithSid("PUT", "/api/orgs/"+rig.orgID.String()+"/github/pat", rig.sid, map[string]string{"pat": "ghp_new"})
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PAT bind on a managed workspace with no live row status=%d body=%s, want 200", resp.StatusCode, body)
		}
		if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassPAT) {
			t.Errorf("class after the PAT bind = %q, want pat", class)
		}
		rig.assertRowsAgreeWithClass(t)
	})
}

// TestManagedDisconnect_RacesAPATBind: a disconnect and a PAT bind on one
// workspace serialize under the App-registration lock, and the loser sees the
// winner's state. Both are released onto the lock at once; whichever runs
// second reads the other's commit — a bind after the disconnect lands on an
// empty workspace and succeeds, a bind before it meets the guard and refuses,
// and the disconnect after a refused bind still finds the rows to remove. In
// every ordering the workspace ends with no live rows and the pat class, and
// holds a PAT exactly when the bind reported success.
func TestManagedDisconnect_RacesAPATBind(t *testing.T) {
	rig := newBindRig(t, newFakeGitHub())
	rig.bindManaged(t, 4242)

	// Hold the lock so both requests queue on it, then release them together.
	// The PAT bind validates against GitHub before it reaches the lock, so it
	// is given a head start to get there.
	release, err := rig.srv.acquireKeyedLock(context.Background(), &rig.srv.githubAppRegMu, githubAppRegRMWLockSalt, rig.orgID.String())
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	var (
		done       sync.WaitGroup
		bindStatus int
		discStatus int
	)
	done.Add(2)
	go func() {
		defer done.Done()
		resp := rig.postJSONWithSid("PUT", "/api/orgs/"+rig.orgID.String()+"/github/pat", rig.sid, map[string]string{"pat": "ghp_new"})
		bindStatus = resp.StatusCode
	}()
	go func() {
		defer done.Done()
		time.Sleep(100 * time.Millisecond)
		discStatus = rig.disconnect(t, "").Code
	}()
	time.Sleep(300 * time.Millisecond)
	release()
	done.Wait()

	if discStatus != http.StatusNoContent {
		t.Errorf("disconnect status=%d, want 204 whichever order the lock chose", discStatus)
	}
	if live := rig.liveInstallations(t); len(live) != 0 {
		t.Errorf("live rows after the race = %v, want none", live)
	}
	if class := rig.credentialClass(t); class != string(domain.GitHubCredentialClassPAT) {
		t.Errorf("class after the race = %q, want pat", class)
	}
	rig.assertRowsAgreeWithClass(t)

	pat := rig.storedPAT(t)
	switch bindStatus {
	case http.StatusOK:
		if pat == "" {
			t.Error("the PAT bind reported success and stored nothing")
		}
	case http.StatusConflict:
		if pat != "" {
			t.Error("the PAT bind was refused and stored a PAT anyway")
		}
	default:
		t.Errorf("PAT bind status=%d, want 200 (ran after the disconnect) or 409 (ran before it)", bindStatus)
	}
}
