package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func linearCredentialPath() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/linear/access/credential"
}

func linearAccessPath() string { return "/api/orgs/" + runmode.LocalDefaultOrgID + "/linear/access" }

// linearWhoamiFake answers the one document the bind sends — Whoami — as the
// person, the app user, the refusal or the outage a test sets it to be.
type linearWhoamiFake struct {
	URL string

	mu        sync.Mutex
	mode      string // "person" (default), "app", "rejected", "down"
	workspace linear.Organization
	calls     int
}

func newLinearWhoamiFake(t *testing.T) *linearWhoamiFake {
	t.Helper()
	f := &linearWhoamiFake{mode: "person", workspace: linear.Organization{ID: "org-acme", Name: "Acme", URLKey: "acme"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		mode, ws := f.mode, f.workspace
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		email := "ada@example.com"
		switch mode {
		case "rejected":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`)
			return
		case "down":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case "app":
			email = "4b1b3139@oauthapp.linear.app"
		}
		writeGraphQLData(w, map[string]any{
			"viewer":       map[string]any{"id": "lu-ada", "name": "ada", "displayName": "Ada", "email": email},
			"organization": ws,
		})
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *linearWhoamiFake) set(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

func (f *linearWhoamiFake) setWorkspace(ws linear.Organization) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspace = ws
}

func (f *linearWhoamiFake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// linearAccessRig is a local test server with no Linear credential bound,
// validating against a fake Linear, and recording every Linear re-due the
// handlers ask for.
type linearAccessRig struct {
	s      *Server
	fake   *linearWhoamiFake
	stores db.Stores
	kicks  chan string
}

func newLinearAccessRig(t *testing.T) *linearAccessRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	linear.SetRetryBackoffForTest(t, time.Millisecond)
	s := newTestServer(t)
	unconfigureEventSources(t)
	fake := newLinearWhoamiFake(t)
	s.validateLinear = func(ctx context.Context, cfg linear.Config) (*auth.LinearUser, *auth.LinearOrganization, error) {
		cfg.Endpoint = fake.URL
		return auth.ValidateLinear(ctx, cfg)
	}
	kicks := make(chan string, 8)
	s.SetOnLinearChanged(func(orgID string) { kicks <- orgID })
	return &linearAccessRig{s: s, fake: fake, stores: sqlitestore.New(s.db), kicks: kicks}
}

func (r *linearAccessRig) secret(t *testing.T, key string) string {
	t.Helper()
	v, err := r.stores.Secrets.Get(t.Context(), runmode.LocalDefaultOrgID, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return v
}

func (r *linearAccessRig) putSecret(t *testing.T, key, value string) {
	t.Helper()
	if err := r.stores.Secrets.Put(t.Context(), runmode.LocalDefaultOrgID, key, value, ""); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func (r *linearAccessRig) orgSettings(t *testing.T) domain.OrgSettings {
	t.Helper()
	set, err := r.stores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("org settings: %v", err)
	}
	return set
}

// credentialRows returns the org's Linear credential change-log rows, newest
// first.
func (r *linearAccessRig) credentialRows(t *testing.T) []domain.AccessChange {
	t.Helper()
	rows, _, err := r.stores.AccessChangeLog.ListByOrg(t.Context(), runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list access log: %v", err)
	}
	var out []domain.AccessChange
	for _, row := range rows {
		var d struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal([]byte(row.DetailJSON), &d)
		if d.Kind == domain.CredentialKindLinearOrg {
			out = append(out, row)
		}
	}
	return out
}

// expectKick waits for the re-due the handler fires on its own goroutine.
func (r *linearAccessRig) expectKick(t *testing.T) {
	t.Helper()
	select {
	case org := <-r.kicks:
		if org != runmode.LocalDefaultOrgID {
			t.Errorf("re-dued org %q, want %q", org, runmode.LocalDefaultOrgID)
		}
	case <-time.After(2 * time.Second):
		t.Error("the handler did not re-due the org's Linear poll")
	}
}

func (r *linearAccessRig) expectNoKick(t *testing.T) {
	t.Helper()
	select {
	case org := <-r.kicks:
		t.Errorf("re-dued org %q's Linear poll on a request that changed nothing", org)
	case <-time.After(50 * time.Millisecond):
	}
}

func (r *linearAccessRig) bind(t *testing.T, key string) linearAccessStatus {
	t.Helper()
	rec := doJSON(t, r.s, http.MethodPut, linearCredentialPath(), map[string]any{"api_key": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT linear credential: %d: %s", rec.Code, rec.Body.String())
	}
	r.expectKick(t)
	return decodeLinearAccess(t, rec)
}

func (r *linearAccessRig) access(t *testing.T) linearAccessStatus {
	t.Helper()
	rec := doJSON(t, r.s, http.MethodGet, linearAccessPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET linear access: %d: %s", rec.Code, rec.Body.String())
	}
	return decodeLinearAccess(t, rec)
}

func decodeLinearAccess(t *testing.T, rec *httptest.ResponseRecorder) linearAccessStatus {
	t.Helper()
	var got linearAccessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode linear access: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

// TestLinearCredentialPut_Binds walks a bind end to end: what it stores, the
// workspace it learns, the audit row, the re-due, and every read that reflects
// it — the status route, the integrations status, and the org's sources.
func TestLinearCredentialPut_Binds(t *testing.T) {
	r := newLinearAccessRig(t)
	// A stale install envelope with no marker naming it: the bind must not
	// leave the other shape's secret beside the api_key marker.
	r.putSecret(t, integrations.KeyLinearAppInstall, `{"refresh_token":"stale"}`)

	got := r.bind(t, "  lin_api_ada  ")

	want := linearAccessStatus{
		Connected: true, AuthMethod: "api_key", WorkspaceURLKey: "acme", WorkspaceName: "Acme",
		BoundAs: &linearBoundAsJSON{Name: "ada", DisplayName: "Ada"},
	}
	if got.Connected != want.Connected || got.AuthMethod != want.AuthMethod || got.WorkspaceURLKey != want.WorkspaceURLKey ||
		got.WorkspaceName != want.WorkspaceName || got.BoundAs == nil || *got.BoundAs != *want.BoundAs ||
		got.ConnectAvailable || got.UsingDeploymentDefault {
		t.Errorf("PUT answered %+v, want %+v", got, want)
	}
	if read := r.access(t); read.Connected != got.Connected || read.WorkspaceURLKey != got.WorkspaceURLKey || read.BoundAs == nil {
		t.Errorf("GET answered %+v, want the PUT's %+v", read, got)
	}

	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "lin_api_ada" {
		t.Errorf("stored key = %q, want the trimmed key", v)
	}
	if v := r.secret(t, integrations.KeyLinearAuthMethod); v != "api_key" {
		t.Errorf("stored marker = %q, want api_key", v)
	}
	if v := r.secret(t, integrations.KeyLinearAppInstall); v != "" {
		t.Errorf("app install envelope = %q, want it cleared by the bind", v)
	}
	if set := r.orgSettings(t); set.LinearWorkspaceID != "org-acme" || set.LinearWorkspaceURLKey != "acme" {
		t.Errorf("workspace columns = (%q, %q), want (org-acme, acme)", set.LinearWorkspaceID, set.LinearWorkspaceURLKey)
	}

	rows := r.credentialRows(t)
	if len(rows) != 1 || rows[0].Action != domain.AccessActionCredentialSet ||
		rows[0].DetailJSON != domain.AccessDetailCredential(domain.CredentialKindLinearOrg, "", "acme") {
		t.Errorf("linear credential rows = %+v, want one credential_set naming the workspace", rows)
	}

	if st := readOrgSources(t, r.s)[eventsource.KindLinear]; st != string(eventsource.StateAvailable) {
		t.Errorf("linear source = %q after a bind, want available", st)
	}
	status := doJSON(t, r.s, http.MethodGet, "/api/integrations/status", nil)
	var integ map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &integ); err != nil {
		t.Fatalf("decode integrations status: %v", err)
	}
	if integ["linear"] != true || integ["linear_workspace_url_key"] != "acme" {
		t.Errorf("integrations status linear = %v, url key = %v; want true, acme", integ["linear"], integ["linear_workspace_url_key"])
	}
}

// TestLinearCredentialPut_Rotates: binding a second key replaces the first,
// re-learns the workspace, and records a second row.
func TestLinearCredentialPut_Rotates(t *testing.T) {
	r := newLinearAccessRig(t)
	r.bind(t, "lin_api_one")
	r.fake.setWorkspace(linear.Organization{ID: "org-beta", Name: "Beta", URLKey: "beta"})
	got := r.bind(t, "lin_api_two")

	if got.WorkspaceURLKey != "beta" || got.WorkspaceName != "Beta" {
		t.Errorf("rotation answered %+v, want the new workspace", got)
	}
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "lin_api_two" {
		t.Errorf("stored key = %q, want the rotated key", v)
	}
	if set := r.orgSettings(t); set.LinearWorkspaceID != "org-beta" {
		t.Errorf("workspace id = %q, want org-beta", set.LinearWorkspaceID)
	}
	if rows := r.credentialRows(t); len(rows) != 2 {
		t.Errorf("linear credential rows = %d, want 2", len(rows))
	}
}

// TestLinearCredentialPut_RefusesBadBodies: a body that names no key is the
// caller's fault, answered before Linear is asked anything.
func TestLinearCredentialPut_RefusesBadBodies(t *testing.T) {
	r := newLinearAccessRig(t)
	cases := []struct {
		name       string
		body       any
		wantReason string
	}{
		{"absent key", map[string]any{}, "MISSING_FIELD"},
		{"empty key", map[string]any{"api_key": ""}, "MISSING_FIELD"},
		{"whitespace key", map[string]any{"api_key": "   "}, "MISSING_FIELD"},
		{"unknown field", map[string]any{"api_key": "lin_api_x", "url": "https://linear.app"}, "UNKNOWN_FIELD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, r.s, http.MethodPut, linearCredentialPath(), tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := firstError(t, rec); got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
	if n := r.fake.Calls(); n != 0 {
		t.Errorf("Linear was asked %d times, want 0", n)
	}
}

// TestLinearCredentialPut_UpstreamOutcomes pins the three ways Linear can
// decline: a refused key and an app user's key are the caller's to fix (422),
// an outage is not (502). None of them stores, audits or re-dues anything.
func TestLinearCredentialPut_UpstreamOutcomes(t *testing.T) {
	cases := []struct {
		mode       string
		wantStatus int
		wantReason string
		wantField  string
	}{
		{"rejected", http.StatusUnprocessableEntity, "UPSTREAM_REJECTED", "api_key"},
		{"app", http.StatusUnprocessableEntity, "UPSTREAM_REJECTED", "api_key"},
		{"down", http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			r := newLinearAccessRig(t)
			r.fake.set(tc.mode)

			rec := doJSON(t, r.s, http.MethodPut, linearCredentialPath(), map[string]any{"api_key": "lin_api_x"})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			got := firstError(t, rec)
			if got.Reason != tc.wantReason || got.Field != tc.wantField {
				t.Errorf("error = %+v, want reason %s field %q", got, tc.wantReason, tc.wantField)
			}
			if v := r.secret(t, integrations.KeyLinearAPIKey); v != "" {
				t.Errorf("stored key = %q after a refused bind, want none", v)
			}
			if set := r.orgSettings(t); set.LinearWorkspaceID != "" {
				t.Errorf("workspace id = %q after a refused bind, want none", set.LinearWorkspaceID)
			}
			if rows := r.credentialRows(t); len(rows) != 0 {
				t.Errorf("linear credential rows = %+v, want none", rows)
			}
			r.expectNoKick(t)
		})
	}
}

// TestLinearCredentialPut_RefusesOverInstalledApp: an installed app has
// tokens to revoke, so a key is not bound over it.
func TestLinearCredentialPut_RefusesOverInstalledApp(t *testing.T) {
	r := newLinearAccessRig(t)
	r.putSecret(t, integrations.KeyLinearAuthMethod, string(linear.AuthMethodAppInstall))
	r.putSecret(t, integrations.KeyLinearAppInstall, `{"refresh_token":"live"}`)

	rec := doJSON(t, r.s, http.MethodPut, linearCredentialPath(), map[string]any{"api_key": "lin_api_x"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if got := firstError(t, rec); got.Reason != "CONFLICT" {
		t.Errorf("reason = %q, want CONFLICT", got.Reason)
	}
	if v := r.secret(t, integrations.KeyLinearAuthMethod); v != string(linear.AuthMethodAppInstall) {
		t.Errorf("marker = %q, want app_install left alone", v)
	}
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "" {
		t.Errorf("stored key = %q, want none", v)
	}
	if v := r.secret(t, integrations.KeyLinearAppInstall); v == "" {
		t.Error("the install envelope was cleared by a refused bind")
	}
	if rows := r.credentialRows(t); len(rows) != 0 {
		t.Errorf("linear credential rows = %+v, want none", rows)
	}
	r.expectNoKick(t)
}

// TestLinearCredentialDelete_Unbinds: the disconnect clears the credential,
// its bound-as record and the workspace columns, audits once, and is a
// no-audit 200 when there is nothing left to remove.
func TestLinearCredentialDelete_Unbinds(t *testing.T) {
	r := newLinearAccessRig(t)
	r.bind(t, "lin_api_ada")

	rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	r.expectKick(t)

	for _, k := range []string{integrations.KeyLinearAPIKey, integrations.KeyLinearAuthMethod, integrations.KeyLinearAppInstall, integrations.KeyLinearBoundAs} {
		if v := r.secret(t, k); v != "" {
			t.Errorf("%s = %q after the unbind, want cleared", k, v)
		}
	}
	if set := r.orgSettings(t); set.LinearWorkspaceID != "" || set.LinearWorkspaceURLKey != "" {
		t.Errorf("workspace columns = (%q, %q), want both cleared", set.LinearWorkspaceID, set.LinearWorkspaceURLKey)
	}
	// The bind's row and the unbind's can share a timestamp, so they are
	// matched by action rather than by position.
	var removed []domain.AccessChange
	for _, row := range r.credentialRows(t) {
		if row.Action == domain.AccessActionCredentialRemoved {
			removed = append(removed, row)
		}
	}
	if len(removed) != 1 || removed[0].DetailJSON != domain.AccessDetailCredential(domain.CredentialKindLinearOrg, "", "acme") {
		t.Errorf("credential_removed rows = %+v, want one naming the workspace", removed)
	}
	if got := r.access(t); got.Connected || got.AuthMethod != "" || got.BoundAs != nil || got.WorkspaceURLKey != "" {
		t.Errorf("GET after the unbind = %+v, want disconnected", got)
	}
	if st := readOrgSources(t, r.s)[eventsource.KindLinear]; st != string(eventsource.StateUnconfigured) {
		t.Errorf("linear source = %q after the unbind, want unconfigured", st)
	}

	again := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil)
	if again.Code != http.StatusOK {
		t.Fatalf("second DELETE: %d: %s", again.Code, again.Body.String())
	}
	r.expectKick(t)
	if rows := r.credentialRows(t); len(rows) != 2 {
		t.Errorf("linear credential rows = %d after an idempotent unbind, want still 2", len(rows))
	}
}

// TestLinearCredentialDelete_LeavesUserCredentials: a person's own Linear
// credential is theirs and is cleared only by its own surface.
func TestLinearCredentialDelete_LeavesUserCredentials(t *testing.T) {
	r := newLinearAccessRig(t)
	r.bind(t, "lin_api_ada")
	ctx := t.Context()
	userKey := linear.UserTokenKey("org-acme")
	if err := r.stores.Secrets.PutUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, userKey, `{"method":"api_key","token":"lin_api_mine"}`, ""); err != nil {
		t.Fatalf("put user credential: %v", err)
	}

	if rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	r.expectKick(t)
	v, err := r.stores.Secrets.GetUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, userKey)
	if err != nil || v == "" {
		t.Errorf("user credential = %q (err %v) after the org unbind, want it kept", v, err)
	}
}

// failingWorkspaceTx fails the Linear workspace write, the last write the
// bind and the unbind make before their audit row, so every keychain write
// before it has already landed when the transaction rolls back.
type failingWorkspaceTx struct{ db.TxRunner }

func (w failingWorkspaceTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.TxRunner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Orgs = failingWorkspaceOrgs{OrgsStore: tx.Orgs}
		return fn(tx)
	})
}

type failingWorkspaceOrgs struct{ db.OrgsStore }

func (failingWorkspaceOrgs) SetLinearWorkspace(context.Context, string, string, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("workspace write failed")
}

// TestLinearCredential_FailedWriteRestoresKeys: a bind or unbind whose
// transaction fails after its keychain writes answers 500 and leaves the org
// exactly as it was — the keys are put back, and no audit row or re-due lands.
func TestLinearCredential_FailedWriteRestoresKeys(t *testing.T) {
	r := newLinearAccessRig(t)
	r.bind(t, "lin_api_ada")
	keys := []string{integrations.KeyLinearAPIKey, integrations.KeyLinearAuthMethod, integrations.KeyLinearAppInstall, integrations.KeyLinearBoundAs}
	before := map[string]string{}
	for _, k := range keys {
		before[k] = r.secret(t, k)
	}
	rows := len(r.credentialRows(t))
	r.s.tx = failingWorkspaceTx{TxRunner: r.s.tx}
	r.fake.setWorkspace(linear.Organization{ID: "org-beta", Name: "Beta", URLKey: "beta"})

	for _, tc := range []struct {
		method string
		body   any
	}{
		{http.MethodPut, map[string]any{"api_key": "lin_api_bea"}},
		{http.MethodDelete, nil},
	} {
		rec := doJSON(t, r.s, tc.method, linearCredentialPath(), tc.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s with a failing write: %d %s, want 500", tc.method, rec.Code, rec.Body.String())
		}
		r.expectNoKick(t)
		for _, k := range keys {
			if v := r.secret(t, k); v != before[k] {
				t.Errorf("%s: %s = %q after the failed write, want %q", tc.method, k, v, before[k])
			}
		}
		if set := r.orgSettings(t); set.LinearWorkspaceURLKey != "acme" {
			t.Errorf("%s: workspace = %q after the failed write, want acme", tc.method, set.LinearWorkspaceURLKey)
		}
	}
	if got := len(r.credentialRows(t)); got != rows {
		t.Errorf("linear credential rows = %d after two failed writes, want still %d", got, rows)
	}
}

// TestLinearCredential_LeavesSettingsVersion: the workspace columns are the
// credential's, not the settings page's, so a bind or unbind leaves the
// settings version alone. A settings save loaded before the bind still lands,
// and it cannot put the old workspace back.
func TestLinearCredential_LeavesSettingsVersion(t *testing.T) {
	r := newLinearAccessRig(t)
	loaded := orgSettingsVersion(t, r.s)

	r.bind(t, "lin_api_ada")
	if v := orgSettingsVersion(t, r.s); v != loaded {
		t.Fatalf("settings version = %d after the bind, want %d unchanged", v, loaded)
	}

	rec := doJSON(t, r.s, http.MethodPatch, orgSettingsPath(), map[string]any{"version": loaded, "linear_poll_interval": "20m"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH loaded before the bind: %d: %s", rec.Code, rec.Body.String())
	}
	if set := r.orgSettings(t); set.LinearWorkspaceID != "org-acme" || set.LinearWorkspaceURLKey != "acme" {
		t.Errorf("workspace = (%q, %q) after the settings save, want the bound one kept", set.LinearWorkspaceID, set.LinearWorkspaceURLKey)
	}

	loaded = orgSettingsVersion(t, r.s)
	if rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	r.expectKick(t)
	if v := orgSettingsVersion(t, r.s); v != loaded {
		t.Errorf("settings version = %d after the unbind, want %d unchanged", v, loaded)
	}
}

// TestLinearCredential_UnreadableSnapshotRefuses: a key the local snapshot
// cannot read is one a failed write could not put back, so both the bind and
// the unbind refuse before touching anything.
func TestLinearCredential_UnreadableSnapshotRefuses(t *testing.T) {
	r := newLinearAccessRig(t)
	r.bind(t, "lin_api_ada")
	r.s.secrets = getFailingSecrets{SecretStore: r.s.secrets}

	for _, tc := range []struct {
		method string
		body   any
	}{
		{http.MethodPut, map[string]any{"api_key": "lin_api_bea"}},
		{http.MethodDelete, nil},
	} {
		rec := doJSON(t, r.s, tc.method, linearCredentialPath(), tc.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s with an unreadable snapshot: %d %s, want 500", tc.method, rec.Code, rec.Body.String())
		}
		r.expectNoKick(t)
		if v := r.secret(t, integrations.KeyLinearAPIKey); v != "lin_api_ada" {
			t.Errorf("%s: key = %q, want the bound key untouched", tc.method, v)
		}
	}
}

// TestSnapshotLinearSecrets_Restores pins the local-mode rollback the bind
// relies on: keys put back as they were, and keys that were absent removed.
func TestSnapshotLinearSecrets_Restores(t *testing.T) {
	r := newLinearAccessRig(t)
	r.putSecret(t, integrations.KeyLinearAPIKey, "lin_api_old")
	r.putSecret(t, integrations.KeyLinearAuthMethod, "api_key")

	restore, err := r.s.snapshotLinearSecrets(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	r.putSecret(t, integrations.KeyLinearAPIKey, "lin_api_new")
	r.putSecret(t, integrations.KeyLinearBoundAs, `{"name":"ada"}`)
	restore()

	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "lin_api_old" {
		t.Errorf("key = %q, want the prior key restored", v)
	}
	if v := r.secret(t, integrations.KeyLinearAuthMethod); v != "api_key" {
		t.Errorf("marker = %q, want it kept", v)
	}
	if v := r.secret(t, integrations.KeyLinearBoundAs); v != "" {
		t.Errorf("bound-as = %q, want it removed — it was absent before", v)
	}
}

// TestLinearSource_PauseAndCreateGate pins what the source flip buys for free
// once Linear can be configured: the off switch applies to it and clears its
// tracker snapshots, and a linear:* handler is authorable only while the
// source is available.
func TestLinearSource_PauseAndCreateGate(t *testing.T) {
	r := newLinearAccessRig(t)
	createRule := func() int {
		return doJSON(t, r.s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
			"event_type":       domain.EventLinearIssueAssigned,
			"name":             "linear rule",
			"default_priority": 0.5,
		}).Code
	}

	if code := createRule(); code != http.StatusUnprocessableEntity {
		t.Errorf("create a linear rule with no credential = %d, want 422", code)
	}

	r.bind(t, "lin_api_ada")
	ctx := t.Context()
	ent, _, err := r.stores.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "linear", "ENG-1", "issue", "An issue", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if ok, err := r.stores.Entities.UpdateSnapshotCASSystem(ctx, runmode.LocalDefaultOrgID, ent.ID, `{"identifier":"ENG-1"}`, ent.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}

	rec := doJSON(t, r.s, http.MethodPatch, orgSourcePath(eventsource.KindLinear), map[string]any{"disabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH linear disabled: %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeSource(t, rec.Body.Bytes()); got.State != eventsource.StateDisabled {
		t.Errorf("PATCH answered %q, want disabled", got.State)
	}
	got, err := r.stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "linear", "ENG-1")
	if err != nil || got == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", got, err)
	}
	if got.SnapshotJSON != "" {
		t.Errorf("linear snapshot = %q after disable, want cleared", got.SnapshotJSON)
	}
	if code := createRule(); code != http.StatusUnprocessableEntity {
		t.Errorf("create a linear rule while paused = %d, want 422", code)
	}

	if rec := doJSON(t, r.s, http.MethodPatch, orgSourcePath(eventsource.KindLinear), map[string]any{"disabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH linear enabled: %d: %s", rec.Code, rec.Body.String())
	}
	if code := createRule(); code != http.StatusCreated {
		t.Errorf("create a linear rule with the source available = %d, want 201", code)
	}
}

// TestLinearAccessRoutes_Registration pins how the three routes mount: the
// two credential writes behind the CSRF wrap, the read without it.
func TestLinearAccessRoutes_Registration(t *testing.T) {
	want := map[string]string{
		"GET /api/orgs/{org_id}/linear/access":               "api",
		"PUT /api/orgs/{org_id}/linear/access/credential":    "apiMutating",
		"DELETE /api/orgs/{org_id}/linear/access/credential": "apiMutating",
	}
	got := map[string]string{}
	for _, route := range discoverProtectedRoutes(t) {
		if _, ok := want[route.pattern]; ok {
			got[route.pattern] = route.via
		}
	}
	for pattern, via := range want {
		if got[pattern] != via {
			t.Errorf("%s mounted via %q, want %q", pattern, got[pattern], via)
		}
	}
}
