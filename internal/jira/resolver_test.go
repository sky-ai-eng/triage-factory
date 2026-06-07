package jira

import (
	"context"
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
		"both empty": {},
		"url only":   {keyJiraURL: "https://jira.example.com"},
		"pat only":   {keyJiraPAT: "org-pat"},
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
