package jira

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// --- fakes (embed the interface so unexercised methods compile-satisfy and
// panic only if the resolver unexpectedly reaches for them) ---

type fakeSecrets struct {
	db.SecretStore
	sys     map[string]string // key -> value for GetSystem
	user    map[string]string // key -> value for GetUserSystem
	sysErr  error
	userErr error

	// recorded args from the last GetUserSystem call — pins that ForUser
	// routes by (orgID, userID), the per-user provenance the resolver exists
	// to enforce.
	gotUserOrg, gotUserID, gotUserKey string
}

func (f *fakeSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	if f.sysErr != nil {
		return "", f.sysErr
	}
	return f.sys[key], nil
}

func (f *fakeSecrets) GetUserSystem(_ context.Context, orgID, userID, key string) (string, error) {
	f.gotUserOrg, f.gotUserID, f.gotUserKey = orgID, userID, key
	if f.userErr != nil {
		return "", f.userErr
	}
	return f.user[key], nil
}

type fakeOrgs struct {
	db.OrgsStore
	jiraBase string
	err      error
}

func (f *fakeOrgs) GetSettingsSystem(_ context.Context, _ string) (domain.OrgSettings, error) {
	if f.err != nil {
		return domain.OrgSettings{}, f.err
	}
	return domain.OrgSettings{JiraBaseURL: f.jiraBase}, nil
}

const (
	testOrgID  = "org-1"
	testUserID = "user-1"
)

// TestForSystem_BuildsOrgClient pins the system path: the org's jira_url +
// jira_pat secrets become a Bearer client that talks to the org host.
func TestForSystem_BuildsOrgClient(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	secrets := &fakeSecrets{sys: map[string]string{
		keyJiraURL: srv.URL,
		keyJiraPAT: "org-pat",
	}}
	r := NewResolver(secrets, &fakeOrgs{})

	c, err := r.ForSystem(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("ForSystem: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if auth, _ := rec.read(); auth != "Bearer org-pat" {
		t.Errorf("Authorization = %q, want %q (system path uses the org service cred)", auth, "Bearer org-pat")
	}
}

// TestForSystem_MissingCredential pins ErrNoJiraSystemCredential when either
// half (URL or PAT) is absent — the poller/exec "not configured" boundary.
func TestForSystem_MissingCredential(t *testing.T) {
	cases := map[string]map[string]string{
		"both empty":    {},
		"url only":      {keyJiraURL: "https://jira.example.com"},
		"pat only":      {keyJiraPAT: "org-pat"},
		"malformed url": {keyJiraURL: "not-a-url", keyJiraPAT: "org-pat"},
	}
	for name, sys := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewResolver(&fakeSecrets{sys: sys}, &fakeOrgs{})
			_, err := r.ForSystem(context.Background(), testOrgID)
			if !errors.Is(err, ErrNoJiraSystemCredential) {
				t.Errorf("ForSystem err = %v, want ErrNoJiraSystemCredential", err)
			}
		})
	}
}

// TestForSystem_BackendError propagates a secret-store read error rather than
// misreporting it as "not configured".
func TestForSystem_BackendError(t *testing.T) {
	sentinel := errors.New("vault down")
	r := NewResolver(&fakeSecrets{sysErr: sentinel}, &fakeOrgs{})
	_, err := r.ForSystem(context.Background(), testOrgID)
	if !errors.Is(err, sentinel) {
		t.Errorf("ForSystem err = %v, want the backend error", err)
	}
	if errors.Is(err, ErrNoJiraSystemCredential) {
		t.Errorf("a backend read error must not collapse to ErrNoJiraSystemCredential")
	}
}

// TestForSystem_CanonicalizesBaseURL proves ForSystem runs the stored jira_url
// through CanonicalHost, so the system client talks to the same trimmed origin
// a per-user client would even when the secret carries stray whitespace or a
// trailing slash. The leading space alone would make http.NewRequest fail if it
// weren't trimmed.
func TestForSystem_CanonicalizesBaseURL(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	secrets := &fakeSecrets{sys: map[string]string{
		keyJiraURL: "  " + srv.URL + "/  ",
		keyJiraPAT: "org-pat",
	}}
	c, err := NewResolver(secrets, &fakeOrgs{}).ForSystem(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("ForSystem: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("GetIssue (stored base URL not canonicalized?): %v", err)
	}
	if _, path := rec.read(); path != "/rest/api/2/issue/PROJ-1" {
		t.Errorf("path = %q, want /rest/api/2/issue/PROJ-1 (trailing slash not trimmed)", path)
	}
}

// TestForSystem_BuildsCloudClient pins the Cloud system path: an org whose
// stored auth-method marker is cloud_api_token resolves to a Basic / REST v3
// client built from the email + API token, talking to the org host.
func TestForSystem_BuildsCloudClient(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	secrets := &fakeSecrets{sys: map[string]string{
		keyJiraURL:        srv.URL,
		keyJiraAuthMethod: string(AuthMethodCloudAPIToken),
		keyJiraEmail:      "bot@acme.com",
		keyJiraAPIToken:   "cloud-token",
	}}
	r := NewResolver(secrets, &fakeOrgs{})

	c, err := r.ForSystem(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("ForSystem: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	auth, path := rec.read()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.com:cloud-token"))
	if auth != wantAuth {
		t.Errorf("Authorization = %q, want %q (cloud uses Basic email:token)", auth, wantAuth)
	}
	if path != "/rest/api/3/issue/PROJ-1" {
		t.Errorf("path = %q, want /rest/api/3/issue/PROJ-1 (cloud uses REST v3)", path)
	}
}

// TestForSystem_CloudMissingCredential pins that a Cloud marker without the
// email/token pair is ErrNoJiraSystemCredential — the resolver never falls back
// to a DC PAT for a Cloud-marked org.
func TestForSystem_CloudMissingCredential(t *testing.T) {
	cases := map[string]map[string]string{
		"no email or token": {keyJiraURL: "https://acme.atlassian.net", keyJiraAuthMethod: string(AuthMethodCloudAPIToken)},
		"token only":        {keyJiraURL: "https://acme.atlassian.net", keyJiraAuthMethod: string(AuthMethodCloudAPIToken), keyJiraAPIToken: "t"},
		"email only":        {keyJiraURL: "https://acme.atlassian.net", keyJiraAuthMethod: string(AuthMethodCloudAPIToken), keyJiraEmail: "e@x.com"},
	}
	for name, sys := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewResolver(&fakeSecrets{sys: sys}, &fakeOrgs{}).ForSystem(context.Background(), testOrgID)
			if !errors.Is(err, ErrNoJiraSystemCredential) {
				t.Errorf("ForSystem err = %v, want ErrNoJiraSystemCredential", err)
			}
		})
	}
}

// TestDeploymentForMarker pins the marker→deployment routing: a stored marker is
// authoritative (and overrides the host shape); an absent marker falls back to
// host-shape detection, so a pre-Cloud org (DC host, no marker) stays DC and a
// Cloud host with no marker is inferred Cloud.
func TestDeploymentForMarker(t *testing.T) {
	cases := []struct {
		method AuthMethod
		host   string
		want   Deployment
	}{
		{AuthMethodCloudAPIToken, "https://jira.dc.example", DeploymentCloud},
		{AuthMethodDCPAT, "https://acme.atlassian.net", DeploymentDataCenter},
		{"", "https://acme.atlassian.net", DeploymentCloud},
		{"", "https://jira.dc.example", DeploymentDataCenter},
	}
	for _, tc := range cases {
		if got := DeploymentForMarker(tc.method, tc.host); got != tc.want {
			t.Errorf("DeploymentForMarker(%q, %q) = %q, want %q", tc.method, tc.host, got, tc.want)
		}
	}
}

// TestForUser_UsesUserToken is the heart of the ticket: ForUser authenticates
// with the ACTING USER's token, keyed under the org host — and never the org
// service cred, even when one is present.
func TestForUser_UsesUserToken(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	secrets := &fakeSecrets{
		// Org service cred present — if ForUser ever fell back to it, the
		// captured header below would read "Bearer org-pat" and fail.
		sys:  map[string]string{keyJiraURL: srv.URL, keyJiraPAT: "org-pat"},
		user: map[string]string{UserTokenKey(srv.URL): "user-pat"},
	}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: srv.URL})

	c, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if auth, _ := rec.read(); auth != "Bearer user-pat" {
		t.Errorf("Authorization = %q, want %q (user path must carry the user's token, never the org cred)", auth, "Bearer user-pat")
	}
	// The per-user read is routed by (orgID, userID) under the host-scoped key.
	if secrets.gotUserOrg != testOrgID || secrets.gotUserID != testUserID {
		t.Errorf("GetUserSystem routed (org=%q user=%q); want (org=%q user=%q)",
			secrets.gotUserOrg, secrets.gotUserID, testOrgID, testUserID)
	}
	if want := UserTokenKey(srv.URL); secrets.gotUserKey != want {
		t.Errorf("GetUserSystem key = %q, want %q", secrets.gotUserKey, want)
	}
}

// TestForUser_CloudEnvelope pins the Cloud per-user path: a stored
// cloud_api_token envelope resolves to a Basic / REST v3 client built from the
// user's own email + API token — never the org service cred.
func TestForUser_CloudEnvelope(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	env, err := MarshalUserCredential(UserCredential{
		Method: AuthMethodCloudAPIToken, Email: "u@acme.com", Token: "user-cloud-tok",
	})
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	secrets := &fakeSecrets{
		// Org service cred present — ForUser must still use the user envelope.
		// The cloud marker makes the org's deployment Cloud (the httptest host is
		// 127.0.0.1, which host-shape-classifies as DC) so the cloud user cred is
		// usable for it.
		sys: map[string]string{
			keyJiraURL:        srv.URL,
			keyJiraPAT:        "org-pat",
			keyJiraAuthMethod: string(AuthMethodCloudAPIToken),
		},
		user: map[string]string{UserTokenKey(srv.URL): env},
	}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: srv.URL})

	c, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	auth, path := rec.read()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("u@acme.com:user-cloud-tok"))
	if auth != wantAuth {
		t.Errorf("Authorization = %q, want %q (cloud user uses Basic email:token)", auth, wantAuth)
	}
	if path != "/rest/api/3/issue/PROJ-1" {
		t.Errorf("path = %q, want /rest/api/3/issue/PROJ-1 (cloud uses REST v3)", path)
	}
}

// TestForUser_DCEnvelope pins that an explicit dc_pat envelope resolves to a
// Bearer / REST v2 client (the same outcome as the back-compat bare token in
// TestForUser_UsesUserToken).
func TestForUser_DCEnvelope(t *testing.T) {
	srv, rec := captureServer(t, `{"key":"PROJ-1"}`)
	env, err := MarshalUserCredential(UserCredential{Method: AuthMethodDCPAT, Token: "user-pat"})
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	secrets := &fakeSecrets{user: map[string]string{UserTokenKey(srv.URL): env}}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: srv.URL})

	c, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if auth, path := rec.read(); auth != "Bearer user-pat" || path != "/rest/api/2/issue/PROJ-1" {
		t.Errorf("got (%q, %q), want (Bearer user-pat, /rest/api/2/issue/PROJ-1)", auth, path)
	}
}

// TestForUser_CloudEnvelopeIncomplete treats a cloud_api_token envelope missing
// the email half as absent — ErrNoJiraUserCredential, never an org fallback.
func TestForUser_CloudEnvelopeIncomplete(t *testing.T) {
	env, err := MarshalUserCredential(UserCredential{Method: AuthMethodCloudAPIToken, Token: "tok"})
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	const base = "https://acme.atlassian.net"
	secrets := &fakeSecrets{
		sys:  map[string]string{keyJiraURL: base, keyJiraPAT: "org-pat"},
		user: map[string]string{UserTokenKey(base): env},
	}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: base})
	if _, err := r.ForUser(context.Background(), testOrgID, testUserID); !errors.Is(err, ErrNoJiraUserCredential) {
		t.Fatalf("ForUser err = %v, want ErrNoJiraUserCredential", err)
	}
}

// TestForUser_StaleCrossScheme_NoClient pins the deployment gate in both cutover
// directions: a credential whose scheme no longer matches the org's marker
// (Cloud token after a Cloud→DC cutover, or a DC PAT after a DC→Cloud cutover)
// resolves to ErrNoJiraUserCredential — never a wrong-scheme client — matching
// the status reader's connected=false. Each credential is complete; only the
// scheme-vs-deployment mismatch makes it unusable.
func TestForUser_StaleCrossScheme_NoClient(t *testing.T) {
	cases := map[string]struct {
		host   string
		marker AuthMethod
		cred   UserCredential
	}{
		"cloud cred on dc-marked org": {
			host:   "https://jira.dc.example", // non-cloud host
			marker: AuthMethodDCPAT,
			cred:   UserCredential{Method: AuthMethodCloudAPIToken, Email: "u@acme.com", Token: "cloud-tok"},
		},
		"dc cred on cloud-marked org": {
			host:   "https://acme.atlassian.net",
			marker: AuthMethodCloudAPIToken,
			cred:   UserCredential{Method: AuthMethodDCPAT, Token: "dc-pat"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env, err := MarshalUserCredential(tc.cred)
			if err != nil {
				t.Fatalf("MarshalUserCredential: %v", err)
			}
			secrets := &fakeSecrets{
				sys:  map[string]string{keyJiraURL: tc.host, keyJiraAuthMethod: string(tc.marker)},
				user: map[string]string{UserTokenKey(tc.host): env},
			}
			r := NewResolver(secrets, &fakeOrgs{jiraBase: tc.host})
			if _, err := r.ForUser(context.Background(), testOrgID, testUserID); !errors.Is(err, ErrNoJiraUserCredential) {
				t.Fatalf("ForUser err = %v, want ErrNoJiraUserCredential (stale cross-scheme cred)", err)
			}
		})
	}
}

// TestForUser_UnknownMethod_Errors pins that an envelope with an unrecognized
// method (corruption, or a forward-version credential this build predates) is
// surfaced as a hard error rather than misrouted to a DC client — and is NOT
// ErrNoJiraUserCredential (the credential exists; re-binding isn't the fix).
func TestForUser_UnknownMethod_Errors(t *testing.T) {
	const base = "https://acme.atlassian.net"
	// A forward-version method this build predates (cloud_oauth is now a known
	// method, so it can no longer stand in for "unknown" — use a value no build
	// recognizes).
	env, err := MarshalUserCredential(UserCredential{Method: "saml_bearer_v9", Token: "tok"})
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	secrets := &fakeSecrets{
		sys:  map[string]string{keyJiraURL: base, keyJiraPAT: "org-pat"},
		user: map[string]string{UserTokenKey(base): env},
	}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: base})
	_, err = r.ForUser(context.Background(), testOrgID, testUserID)
	if err == nil {
		t.Fatal("ForUser err = nil, want an unknown-method error")
	}
	if errors.Is(err, ErrNoJiraUserCredential) {
		t.Errorf("an unknown method must not collapse to ErrNoJiraUserCredential: %v", err)
	}
}

// TestUserCredential_UsableFor pins the network-free precondition the status
// reader and ForUser share: a credential is usable only when its method matches
// the deployment and the required fields are present.
func TestUserCredential_UsableFor(t *testing.T) {
	cases := []struct {
		name string
		cred UserCredential
		dep  Deployment
		want bool
	}{
		{"cloud ok", UserCredential{Method: AuthMethodCloudAPIToken, Email: "e", Token: "t"}, DeploymentCloud, true},
		{"cloud no email", UserCredential{Method: AuthMethodCloudAPIToken, Token: "t"}, DeploymentCloud, false},
		{"cloud on dc host", UserCredential{Method: AuthMethodCloudAPIToken, Email: "e", Token: "t"}, DeploymentDataCenter, false},
		{"dc ok", UserCredential{Method: AuthMethodDCPAT, Token: "t"}, DeploymentDataCenter, true},
		{"dc no token", UserCredential{Method: AuthMethodDCPAT}, DeploymentDataCenter, false},
		{"dc on cloud host", UserCredential{Method: AuthMethodDCPAT, Token: "t"}, DeploymentCloud, false},
		{"cloud oauth ok", UserCredential{Method: AuthMethodCloudOAuth, CloudID: "c", RefreshToken: "r"}, DeploymentCloud, true},
		{"cloud oauth no refresh", UserCredential{Method: AuthMethodCloudOAuth, CloudID: "c"}, DeploymentCloud, false},
		{"cloud oauth no cloud_id", UserCredential{Method: AuthMethodCloudOAuth, RefreshToken: "r"}, DeploymentCloud, false},
		{"cloud oauth on dc host", UserCredential{Method: AuthMethodCloudOAuth, CloudID: "c", RefreshToken: "r"}, DeploymentDataCenter, false},
		{"unknown method", UserCredential{Method: "saml_bearer_v9", Token: "t"}, DeploymentCloud, false},
	}
	for _, tc := range cases {
		if got := tc.cred.UsableFor(tc.dep); got != tc.want {
			t.Errorf("%s: UsableFor(%q) = %v, want %v", tc.name, tc.dep, got, tc.want)
		}
	}
}

// TestParseUserCredential pins the envelope decode + the bare-token back-compat
// the resolver leans on: a JSON envelope round-trips, a bare token reads back as
// a dc_pat, and empty/malformed inputs error rather than silently passing.
func TestParseUserCredential(t *testing.T) {
	// Bare token (pre-envelope DC bind) → dc_pat.
	if c, err := ParseUserCredential("bare-tok"); err != nil || c.Method != AuthMethodDCPAT || c.Token != "bare-tok" {
		t.Errorf("ParseUserCredential(bare) = (%+v, %v), want a dc_pat envelope", c, err)
	}
	// Envelope round-trip.
	want := UserCredential{Method: AuthMethodCloudAPIToken, Email: "e@x.com", Token: "t"}
	env, err := MarshalUserCredential(want)
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	if got, err := ParseUserCredential(env); err != nil || got != want {
		t.Errorf("ParseUserCredential(%q) = (%+v, %v), want %+v", env, got, err, want)
	}
	// Empty + malformed envelope error.
	if _, err := ParseUserCredential("   "); err == nil {
		t.Error("ParseUserCredential(empty) = nil err, want an error")
	}
	if _, err := ParseUserCredential("{not json"); err == nil {
		t.Error("ParseUserCredential(malformed) = nil err, want an error")
	}
}

// TestForUser_AbsentCredential_NoOrgFallback pins the load-bearing rule: an
// absent user credential is ErrNoJiraUserCredential even when the org service
// cred is present — the resolver must never silently act as the bot.
func TestForUser_AbsentCredential_NoOrgFallback(t *testing.T) {
	secrets := &fakeSecrets{
		sys:  map[string]string{keyJiraURL: "https://jira.example.com", keyJiraPAT: "org-pat"},
		user: map[string]string{}, // no user token stored
	}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: "https://jira.example.com"})

	_, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if !errors.Is(err, ErrNoJiraUserCredential) {
		t.Fatalf("ForUser err = %v, want ErrNoJiraUserCredential (no org-cred fallback)", err)
	}
}

// TestForUser_NoOrgHost treats a missing/malformed org Jira host as the
// absent-credential boundary — there's no host to key a user credential under.
func TestForUser_NoOrgHost(t *testing.T) {
	for _, base := range []string{"", "   ", "not-a-url"} {
		r := NewResolver(&fakeSecrets{}, &fakeOrgs{jiraBase: base})
		_, err := r.ForUser(context.Background(), testOrgID, testUserID)
		if !errors.Is(err, ErrNoJiraUserCredential) {
			t.Errorf("ForUser(base=%q) err = %v, want ErrNoJiraUserCredential", base, err)
		}
	}
}

// TestForUser_BackendError propagates a per-user vault read error rather than
// reporting "not connected".
func TestForUser_BackendError(t *testing.T) {
	sentinel := errors.New("vault down")
	secrets := &fakeSecrets{userErr: sentinel}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: "https://jira.example.com"})
	_, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if !errors.Is(err, sentinel) {
		t.Errorf("ForUser err = %v, want the backend error", err)
	}
	if errors.Is(err, ErrNoJiraUserCredential) {
		t.Errorf("a backend read error must not collapse to ErrNoJiraUserCredential")
	}
}

// TestForUser_OrgSettingsError propagates an org-settings read error (we can't
// know the host, so we can't claim "no credential").
func TestForUser_OrgSettingsError(t *testing.T) {
	sentinel := errors.New("db down")
	r := NewResolver(&fakeSecrets{}, &fakeOrgs{err: sentinel})
	_, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if !errors.Is(err, sentinel) {
		t.Errorf("ForUser err = %v, want the org-settings read error", err)
	}
	if errors.Is(err, ErrNoJiraUserCredential) {
		t.Errorf("an org-settings read error must not collapse to ErrNoJiraUserCredential")
	}
}

// TestCanonicalHostAndKey pins the host canonicalization + key composition the
// bind flow (server.resolveJiraHost / jiraTokenKey) must stay in lockstep with.
func TestCanonicalHostAndKey(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOK  bool
		wantKey string
	}{
		{"https://jira.example.com", "https://jira.example.com", true, "jira_token/https://jira.example.com"},
		{"https://jira.example.com/", "https://jira.example.com", true, "jira_token/https://jira.example.com"},
		{"  https://jira.example.com  ", "https://jira.example.com", true, "jira_token/https://jira.example.com"},
		{"", "", false, ""},
		{"ftp://nope", "", false, ""},
		{"not-a-url", "", false, ""},
	}
	for _, tt := range cases {
		host, ok := CanonicalHost(tt.in)
		if ok != tt.wantOK || host != tt.want {
			t.Errorf("CanonicalHost(%q) = (%q, %v), want (%q, %v)", tt.in, host, ok, tt.want, tt.wantOK)
		}
		if ok {
			if got := UserTokenKey(host); got != tt.wantKey {
				t.Errorf("UserTokenKey(%q) = %q, want %q", host, got, tt.wantKey)
			}
		}
	}
}

// fakeOAuthSource records the args ForUser hands it and returns a canned
// (cloudID, accessToken). It's the seam the AuthMethodCloudOAuth branch
// delegates the mint + rotation write-back to (internal/jiraoauth in prod).
type fakeOAuthSource struct {
	cloudID, access          string
	err                      error
	gotOrg, gotUser, gotHost string
	calls                    int
}

func (f *fakeOAuthSource) AccessTokenForUser(_ context.Context, orgID, userID, host string) (string, string, error) {
	f.calls++
	f.gotOrg, f.gotUser, f.gotHost = orgID, userID, host
	return f.cloudID, f.access, f.err
}

// TestForUser_CloudOAuthEnvelope pins the Cloud OAuth path: a stored cloud_oauth
// envelope routes through the OAuth source (by org/user/host) and yields a
// Bearer / REST v3 client against the api.atlassian.com gateway — never the org
// service cred.
func TestForUser_CloudOAuthEnvelope(t *testing.T) {
	const base = "https://acme.atlassian.net"
	env, err := MarshalUserCredential(UserCredential{
		Method: AuthMethodCloudOAuth, CloudID: "cloud-9", RefreshToken: "r1",
	})
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	secrets := &fakeSecrets{
		sys:  map[string]string{keyJiraURL: base, keyJiraAuthMethod: string(AuthMethodCloudOAuth)},
		user: map[string]string{UserTokenKey(base): env},
	}
	src := &fakeOAuthSource{cloudID: "cloud-9", access: "acc-tok"}
	r := NewResolverWithOAuth(secrets, &fakeOrgs{jiraBase: base}, src)

	c, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if c == nil {
		t.Fatal("ForUser returned a nil client")
	}
	if src.calls != 1 || src.gotOrg != testOrgID || src.gotUser != testUserID || src.gotHost != base {
		t.Errorf("source called with (calls=%d org=%q user=%q host=%q), want (1, %q, %q, %q)",
			src.calls, src.gotOrg, src.gotUser, src.gotHost, testOrgID, testUserID, base)
	}
	// The built client must target the gateway with the minted bearer token.
	req, err := c.cfg.NewAPIRequest(context.Background(), "GET", "myself", nil)
	if err != nil {
		t.Fatalf("NewAPIRequest: %v", err)
	}
	if got := req.URL.String(); got != "https://api.atlassian.com/ex/jira/cloud-9/rest/api/3/myself" {
		t.Errorf("request URL = %q, want the cloud-9 gateway v3 path", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer acc-tok" {
		t.Errorf("Authorization = %q, want Bearer acc-tok", got)
	}
}

// TestForUser_CloudOAuth_NoSource: a cloud_oauth envelope on a resolver built
// without OAuth wiring is a configuration error — NOT ErrNoJiraUserCredential
// (the credential exists; the minter just isn't wired), so handlers don't tell
// the user to re-connect when that wouldn't be the fix.
func TestForUser_CloudOAuth_NoSource(t *testing.T) {
	const base = "https://acme.atlassian.net"
	env, _ := MarshalUserCredential(UserCredential{
		Method: AuthMethodCloudOAuth, CloudID: "cloud-9", RefreshToken: "r1",
	})
	secrets := &fakeSecrets{
		sys:  map[string]string{keyJiraURL: base, keyJiraAuthMethod: string(AuthMethodCloudOAuth)},
		user: map[string]string{UserTokenKey(base): env},
	}
	r := NewResolver(secrets, &fakeOrgs{jiraBase: base}) // no OAuth source
	_, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if err == nil {
		t.Fatal("ForUser err = nil, want a config error")
	}
	if errors.Is(err, ErrNoJiraUserCredential) {
		t.Errorf("a missing OAuth source must not collapse to ErrNoJiraUserCredential: %v", err)
	}
}

// TestForUser_CloudOAuthIncomplete treats a cloud_oauth envelope missing the
// refresh token (or cloud_id) as absent — ErrNoJiraUserCredential, re-Connect is
// the fix — never a half-built client or an org fallback.
func TestForUser_CloudOAuthIncomplete(t *testing.T) {
	const base = "https://acme.atlassian.net"
	env, _ := MarshalUserCredential(UserCredential{Method: AuthMethodCloudOAuth, CloudID: "cloud-9"})
	secrets := &fakeSecrets{
		sys:  map[string]string{keyJiraURL: base, keyJiraAuthMethod: string(AuthMethodCloudOAuth)},
		user: map[string]string{UserTokenKey(base): env},
	}
	src := &fakeOAuthSource{cloudID: "cloud-9", access: "acc-tok"}
	r := NewResolverWithOAuth(secrets, &fakeOrgs{jiraBase: base}, src)
	if _, err := r.ForUser(context.Background(), testOrgID, testUserID); !errors.Is(err, ErrNoJiraUserCredential) {
		t.Fatalf("ForUser err = %v, want ErrNoJiraUserCredential", err)
	}
	if src.calls != 0 {
		t.Errorf("OAuth source called %d times for an unusable envelope, want 0", src.calls)
	}
}
