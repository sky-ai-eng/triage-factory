package linear

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeSecrets embeds the interface so a method the resolver should not reach
// for panics on its nil receiver. It records every key read through each door.
type fakeSecrets struct {
	db.SecretStore
	sys     map[string]string
	user    map[string]string
	sysErr  error
	userErr error

	sysRead                           []string
	gotUserOrg, gotUserID, gotUserKey string
}

func (f *fakeSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	f.sysRead = append(f.sysRead, key)
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
	workspaceID string
	err         error
}

func (f *fakeOrgs) GetSettingsSystem(_ context.Context, _ string) (domain.OrgSettings, error) {
	if f.err != nil {
		return domain.OrgSettings{}, f.err
	}
	return domain.OrgSettings{LinearWorkspaceID: f.workspaceID}, nil
}

const (
	testOrgID       = "org-1"
	testUserID      = "user-1"
	testWorkspaceID = "ws-1"
)

// orgAPIKey is an org whose service credential is a valid api_key.
func orgAPIKey() map[string]string {
	return map[string]string{
		keyLinearAuthMethod: string(AuthMethodAPIKey),
		keyLinearAPIKey:     "lin_api_org",
	}
}

func userEnvelope(t *testing.T, c UserCredential) string {
	t.Helper()
	env, err := MarshalUserCredential(c)
	if err != nil {
		t.Fatalf("MarshalUserCredential: %v", err)
	}
	return env
}

// authorizationOf sends one request through c and returns the Authorization
// header it carried.
func authorizationOf(t *testing.T, c *Client) string {
	t.Helper()
	s := newStub(t, scripted(reply{body: viewerOK}))
	c.cfg.Endpoint = s.srv.URL
	if _, err := c.Viewer(context.Background()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	return s.requests()[0].Auth
}

func TestForSystem_BuildsAPIKeyClient(t *testing.T) {
	r := NewResolver(&fakeSecrets{sys: orgAPIKey()}, &fakeOrgs{})

	c, err := r.ForSystem(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("ForSystem: %v", err)
	}
	if c.cfg.Endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want Linear's", c.cfg.Endpoint)
	}
	if c.orgID != testOrgID {
		t.Errorf("client counts under org %q, want %q", c.orgID, testOrgID)
	}
	if got := authorizationOf(t, c); got != "lin_api_org" {
		t.Errorf("Authorization = %q, want the bare org key", got)
	}
}

func TestForSystem_MissingCredential(t *testing.T) {
	cases := map[string]map[string]string{
		"nothing stored":   {},
		"marker, no key":   {keyLinearAuthMethod: string(AuthMethodAPIKey)},
		"key, no marker":   {keyLinearAPIKey: "lin_api_org"},
		"marker empty key": {keyLinearAuthMethod: string(AuthMethodAPIKey), keyLinearAPIKey: ""},
	}
	for name, sys := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewResolver(&fakeSecrets{sys: sys}, &fakeOrgs{}).ForSystem(context.Background(), testOrgID)
			if !errors.Is(err, ErrNoLinearSystemCredential) {
				t.Errorf("ForSystem err = %v, want ErrNoLinearSystemCredential", err)
			}
		})
	}
}

func TestForSystem_AppInstallMarkerUnsupportedHere(t *testing.T) {
	sys := map[string]string{
		keyLinearAuthMethod:  string(AuthMethodAppInstall),
		"linear_app_install": `{"refresh_token":"r1"}`,
	}
	r := NewResolver(&fakeSecrets{sys: sys}, &fakeOrgs{})
	if _, err := r.ForSystem(context.Background(), testOrgID); !errors.Is(err, ErrNoLinearSystemCredential) {
		t.Errorf("ForSystem err = %v, want ErrNoLinearSystemCredential", err)
	}
	if _, err := r.ResolveSystemCredential(context.Background(), testOrgID); !errors.Is(err, ErrNoLinearSystemCredential) {
		t.Errorf("ResolveSystemCredential err = %v, want ErrNoLinearSystemCredential", err)
	}
}

func TestForSystem_UnknownMarkerIsNotUnconfigured(t *testing.T) {
	sys := map[string]string{keyLinearAuthMethod: "saml_v9", keyLinearAPIKey: "lin_api_org"}
	_, err := NewResolver(&fakeSecrets{sys: sys}, &fakeOrgs{}).ForSystem(context.Background(), testOrgID)
	if err == nil {
		t.Fatal("ForSystem err = nil, want an unknown-method error")
	}
	if errors.Is(err, ErrNoLinearSystemCredential) {
		t.Errorf("an unknown marker must not read as unconfigured: %v", err)
	}
}

func TestForSystem_BackendError(t *testing.T) {
	sentinel := errors.New("vault down")
	_, err := NewResolver(&fakeSecrets{sysErr: sentinel}, &fakeOrgs{}).ForSystem(context.Background(), testOrgID)
	if !errors.Is(err, sentinel) {
		t.Errorf("ForSystem err = %v, want the backend error", err)
	}
	if errors.Is(err, ErrNoLinearSystemCredential) {
		t.Error("a backend read error must not collapse to ErrNoLinearSystemCredential")
	}
}

func TestResolveSystemCredential_APIKey(t *testing.T) {
	cred, err := NewResolver(&fakeSecrets{sys: orgAPIKey()}, &fakeOrgs{}).ResolveSystemCredential(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("ResolveSystemCredential: %v", err)
	}
	if want := (SystemCredential{Method: AuthMethodAPIKey, APIKey: "lin_api_org"}); cred != want {
		t.Errorf("credential = %+v, want %+v", cred, want)
	}
	if cfg := cred.Config(); cfg != APIKey("lin_api_org") {
		t.Errorf("Config() = %+v, want APIKey(lin_api_org)", cfg)
	}
}

func TestForUser_UsesUserToken(t *testing.T) {
	secrets := &fakeSecrets{
		// The org credential is valid too: a client carrying it would mean
		// ForUser acted as the org.
		sys:  orgAPIKey(),
		user: map[string]string{UserTokenKey(testWorkspaceID): userEnvelope(t, UserCredential{Method: AuthMethodAPIKey, Token: "lin_api_user"})},
	}
	c, err := NewResolver(secrets, &fakeOrgs{workspaceID: testWorkspaceID}).ForUser(context.Background(), testOrgID, testUserID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if got := authorizationOf(t, c); got != "lin_api_user" {
		t.Errorf("Authorization = %q, want the user's own key", got)
	}
	if c.orgID != testOrgID {
		t.Errorf("client counts under org %q, want %q", c.orgID, testOrgID)
	}
	if secrets.gotUserOrg != testOrgID || secrets.gotUserID != testUserID {
		t.Errorf("user secret read for (org=%q user=%q), want (%q, %q)", secrets.gotUserOrg, secrets.gotUserID, testOrgID, testUserID)
	}
	if want := "linear_token/" + testWorkspaceID; secrets.gotUserKey != want {
		t.Errorf("user secret key = %q, want %q", secrets.gotUserKey, want)
	}
}

func TestForUser_MissingIsErrNoUserCredential(t *testing.T) {
	cases := map[string]struct {
		workspaceID string
		user        map[string]string
	}{
		"org has no workspace": {workspaceID: "", user: map[string]string{UserTokenKey(""): `{"method":"api_key","token":"k"}`}},
		"nothing stored":       {workspaceID: testWorkspaceID},
		"empty api key":        {workspaceID: testWorkspaceID, user: map[string]string{UserTokenKey(testWorkspaceID): `{"method":"api_key"}`}},
		"connect_oauth":        {workspaceID: testWorkspaceID, user: map[string]string{UserTokenKey(testWorkspaceID): `{"method":"connect_oauth","token":"t","refresh_token":"r"}`}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewResolver(&fakeSecrets{user: tc.user}, &fakeOrgs{workspaceID: tc.workspaceID})
			if _, err := r.ForUser(context.Background(), testOrgID, testUserID); !errors.Is(err, ErrNoLinearUserCredential) {
				t.Errorf("ForUser err = %v, want ErrNoLinearUserCredential", err)
			}
		})
	}
}

func TestForUser_NeverFallsBackToSystem(t *testing.T) {
	secrets := &fakeSecrets{sys: orgAPIKey(), user: map[string]string{}}
	r := NewResolver(secrets, &fakeOrgs{workspaceID: testWorkspaceID})

	_, err := r.ForUser(context.Background(), testOrgID, testUserID)
	if !errors.Is(err, ErrNoLinearUserCredential) {
		t.Fatalf("ForUser err = %v, want ErrNoLinearUserCredential", err)
	}
	if len(secrets.sysRead) != 0 {
		t.Errorf("ForUser read org secrets %v; it must never reach for the org credential", secrets.sysRead)
	}
}

func TestForUser_UnreadableCredentialIsNotAbsent(t *testing.T) {
	cases := map[string]string{
		"unknown method":   `{"method":"saml_v9","token":"t"}`,
		"corrupt envelope": `{not json`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			secrets := &fakeSecrets{user: map[string]string{UserTokenKey(testWorkspaceID): raw}}
			_, err := NewResolver(secrets, &fakeOrgs{workspaceID: testWorkspaceID}).ForUser(context.Background(), testOrgID, testUserID)
			if err == nil {
				t.Fatal("ForUser err = nil, want an error")
			}
			if errors.Is(err, ErrNoLinearUserCredential) {
				t.Errorf("an unreadable credential must not read as absent: %v", err)
			}
		})
	}
}

func TestForUser_ReadErrorsPropagate(t *testing.T) {
	sentinel := errors.New("store down")
	cases := map[string]Resolver{
		"user secret":  NewResolver(&fakeSecrets{userErr: sentinel}, &fakeOrgs{workspaceID: testWorkspaceID}),
		"org settings": NewResolver(&fakeSecrets{}, &fakeOrgs{err: sentinel}),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.ForUser(context.Background(), testOrgID, testUserID)
			if !errors.Is(err, sentinel) {
				t.Errorf("ForUser err = %v, want the read error", err)
			}
			if errors.Is(err, ErrNoLinearUserCredential) {
				t.Error("a read error must not collapse to ErrNoLinearUserCredential")
			}
		})
	}
}

func TestUserCredential_RoundTrip(t *testing.T) {
	want := UserCredential{Method: AuthMethodConnectOAuth, Token: "t", RefreshToken: "r"}
	raw := userEnvelope(t, want)
	if raw != `{"method":"connect_oauth","token":"t","refresh_token":"r"}` {
		t.Errorf("envelope = %s", raw)
	}
	got, err := ParseUserCredential(raw)
	if err != nil || got != want {
		t.Errorf("ParseUserCredential = (%+v, %v), want %+v", got, err, want)
	}
	for _, bad := range []string{"", "   ", "lin_api_bare"} {
		if _, err := ParseUserCredential(bad); err == nil {
			t.Errorf("ParseUserCredential(%q) succeeded, want an error", bad)
		}
	}
}
