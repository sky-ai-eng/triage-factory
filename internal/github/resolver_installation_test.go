package github

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// installOnAccount is installOn plus the account's numeric id and an explicit
// installation id, so a test can stage two installations whose logins collide.
func installOnAccount(installationID, accountID, login string) domain.OrgGitHubAppInstallation {
	return domain.OrgGitHubAppInstallation{
		InstallationID: installationID,
		OrgID:          "org-1",
		AccountType:    "Organization",
		AccountID:      accountID,
		AccountLogin:   login,
	}
}

// installResolver builds a resolver over a fixed installation mirror. Only the
// apps store matters to installationFor; the GitHub server is wired so a test
// can go on to mint against whatever it resolved.
func installResolver(t *testing.T, insts ...domain.OrgGitHubAppInstallation) (*resolver, *ghTestServer) {
	t.Helper()
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
		&fakeApps{app: activeApp(), insts: insts},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	).(*resolver)
	return r, gh
}

// TestInstallationFor_ResolvesRenamedAccountByID is the headline case: the
// account behind an installation was renamed, so the mirrored account_login is
// whatever it used to be called, and the caller knows the account by the id
// that didn't change. Resolution must find it — and the row it finds must still
// mint, since a resolved-but-unmintable installation is the same outage.
func TestInstallationFor_ResolvesRenamedAccountByID(t *testing.T) {
	r, gh := installResolver(t, installOnAccount("456", "1234", "acme"))

	inst, err := r.installationFor(context.Background(), "org-1", accountRef{ID: "1234", Login: "acme-corp"})
	if err != nil {
		t.Fatalf("installationFor for a renamed account: %v", err)
	}
	if inst.InstallationID != "456" {
		t.Fatalf("resolved installation %q; want 456", inst.InstallationID)
	}

	tok, err := r.installationToken(context.Background(), "org-1", resolvedApp{org: activeApp()}, inst, gh.srv.URL)
	if err != nil {
		t.Fatalf("installationToken for a renamed account: %v", err)
	}
	if tok.Value != "ghs_minted" {
		t.Errorf("minted token %q; want the installation token", tok.Value)
	}
}

// TestInstallationFor_LoginMatchWhenEitherSideHasNoID pins the negative space:
// with an id missing on either side there is nothing to compare, so the match
// is the case-insensitive login compare it has always been. A row that predates
// the account_id column must resolve exactly as it did, and a caller that knows
// only a handle must be unaffected by rows that do carry an id.
func TestInstallationFor_LoginMatchWhenEitherSideHasNoID(t *testing.T) {
	t.Run("RowHasNoID", func(t *testing.T) {
		r, _ := installResolver(t, installOnAccount("456", "", "acme"))
		for _, target := range []accountRef{
			{Login: "acme"},             // caller knows only the handle
			{Login: "ACME"},             // ... in another capitalisation
			{ID: "1234", Login: "acme"}, // caller knows the id, the row does not
		} {
			inst, err := r.installationFor(context.Background(), "org-1", target)
			if err != nil || inst.InstallationID != "456" {
				t.Errorf("installationFor(%s) = (%q, %v); want (456, nil)", target, inst.InstallationID, err)
			}
		}
	})

	t.Run("TargetHasNoID", func(t *testing.T) {
		r, _ := installResolver(t, installOnAccount("456", "1234", "acme"))
		inst, err := r.installationFor(context.Background(), "org-1", accountByLogin("acme"))
		if err != nil || inst.InstallationID != "456" {
			t.Errorf("installationFor(login-only) = (%q, %v); want (456, nil)", inst.InstallationID, err)
		}
	})
}

// TestInstallationFor_CollidingLoginsNeverCrossResolve stages the state a
// rename leaves behind: one account renamed away, another claimed the freed
// handle, and the mirror has not caught up on the first. Two installations then
// answer to the same login, and each caller that knows an account id must get
// its own — a login collision must never hand one account's caller the other's
// installation, since the token minted from it would act as the wrong account.
func TestInstallationFor_CollidingLoginsNeverCrossResolve(t *testing.T) {
	r, _ := installResolver(t,
		installOnAccount("456", "1234", "acme"), // renamed to acme-corp; mirror is stale
		installOnAccount("789", "5678", "acme"), // the account that claimed the freed handle
	)

	for _, tc := range []struct {
		name   string
		target accountRef
		want   string
	}{
		{"renamed account, stale login", accountRef{ID: "1234", Login: "acme"}, "456"},
		{"renamed account, new login", accountRef{ID: "1234", Login: "acme-corp"}, "456"},
		{"the account now holding the handle", accountRef{ID: "5678", Login: "acme"}, "789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := r.installationFor(context.Background(), "org-1", tc.target)
			if err != nil {
				t.Fatalf("installationFor(%s): %v; want %s", tc.target, err, tc.want)
			}
			if inst.InstallationID != tc.want {
				t.Errorf("installationFor(%s) = %q; want %q", tc.target, inst.InstallationID, tc.want)
			}
		})
	}
}

// TestInstallationFor_Selection is the whole resolution matrix, and the case it
// exists for is the middle one: a workspace with more than one installation.
// One is what a private BYO App produces, and while that was the only shape an
// unnamed account had a single obvious answer; an enterprise-owned App installs
// once per GitHub org and it stops having one. Each row fixes what a caller
// gets, and the two failures are checked to stay distinguishable — "which
// account did you mean" and "you have no GitHub" send a user to different
// places, and only one of them is somewhere worth going.
func TestInstallationFor_Selection(t *testing.T) {
	multi := []domain.OrgGitHubAppInstallation{
		installOnAccount("456", "1234", "acme"),
		installOnAccount("789", "5678", "globex"),
	}

	for _, tc := range []struct {
		name    string
		insts   []domain.OrgGitHubAppInstallation
		target  accountRef
		want    string // installation id, when resolution must succeed
		wantErr error  // sentinel the failure must carry, when it must fail
	}{
		{name: "one installation, no target", insts: multi[:1], target: accountRef{}, want: "456"},
		{name: "one installation, target it holds", insts: multi[:1], target: accountByLogin("acme"), want: "456"},
		{name: "many installations, target by login", insts: multi, target: accountByLogin("globex"), want: "789"},
		{name: "many installations, target by login in another case", insts: multi, target: accountByLogin("GLOBEX"), want: "789"},
		{name: "many installations, target by id against a stale login", insts: multi, target: accountRef{ID: "5678", Login: "globex-inc"}, want: "789"},

		{name: "many installations, no target", insts: multi, target: accountRef{}, wantErr: ErrAmbiguousInstallation},
		{name: "many installations, target in none of them", insts: multi, target: accountByLogin("initech"), wantErr: ErrNoGitHubCredentials},
		{name: "one installation, target it does not hold", insts: multi[:1], target: accountByLogin("globex"), wantErr: ErrNoGitHubCredentials},
		{name: "one installation, target matching neither its id nor its login", insts: multi[:1], target: accountRef{ID: "9999", Login: "other"}, wantErr: ErrNoGitHubCredentials},
		{name: "no installations, no target", insts: nil, target: accountRef{}, wantErr: ErrNoGitHubCredentials},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := installResolver(t, tc.insts...)
			inst, err := r.installationFor(context.Background(), "org-1", tc.target)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("installationFor(%s) err = %v; want %v", tc.target, err, tc.wantErr)
				}
				if errors.Is(err, ErrAmbiguousInstallation) && errors.Is(err, ErrNoGitHubCredentials) {
					t.Errorf("installationFor(%s) err = %v; the two failures must not both match", tc.target, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("installationFor(%s): %v", tc.target, err)
			}
			if inst.InstallationID != tc.want {
				t.Errorf("installationFor(%s) = %q; want %q", tc.target, inst.InstallationID, tc.want)
			}
		})
	}
}

// TestInstallationFor_AmbiguityNamesTheCount pins the one thing the ambiguity
// error is read for. Whoever hits it is looking at a call site that forgot to
// say which account it meant, and the count is what tells them the workspace's
// installations are the reason — an error that only said "ambiguous" would
// leave them counting rows in the mirror by hand.
func TestInstallationFor_AmbiguityNamesTheCount(t *testing.T) {
	r, _ := installResolver(t,
		installOnAccount("456", "1234", "acme"),
		installOnAccount("789", "5678", "globex"),
		installOnAccount("012", "9012", "initech"),
	)
	_, err := r.installationFor(context.Background(), "org-1", accountRef{})
	if !errors.Is(err, ErrAmbiguousInstallation) {
		t.Fatalf("installationFor(empty target) err = %v; want ErrAmbiguousInstallation", err)
	}
	if !strings.Contains(err.Error(), "3 installations") {
		t.Errorf("ambiguity error %q does not name the 3 installations behind it", err)
	}
}

// TestInstallationFor_ReadFailurePropagates pins the third answer, which is
// neither of the other two. An unreadable installation mirror reported as
// missing credentials sends a user to reconnect a GitHub that is fine, and
// reported as ambiguous blames a caller that named its account correctly — so
// the cause travels out intact, for whoever can actually act on it.
func TestInstallationFor_ReadFailurePropagates(t *testing.T) {
	errMirrorDown := errors.New("installation mirror unavailable")
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), listErr: errMirrorDown},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	).(*resolver)

	_, err := r.installationFor(context.Background(), "org-1", accountByLogin("acme"))
	if !errors.Is(err, errMirrorDown) {
		t.Fatalf("installationFor err = %v; want the read failure in the chain", err)
	}
	if errors.Is(err, ErrNoGitHubCredentials) || errors.Is(err, ErrAmbiguousInstallation) {
		t.Errorf("installationFor err = %v; a backend failure is neither missing credentials nor ambiguous", err)
	}

	// And out through the public entry point, where the org also has a PAT the
	// active App must not borrow: the outage surfaces rather than being papered
	// over by a credential this org isn't using.
	if _, err := r.ClientFor(context.Background(), "org-1", "acme"); !errors.Is(err, errMirrorDown) {
		t.Errorf("ClientFor err = %v; want the read failure", err)
	}
	if gh.mintCalls != 0 {
		t.Errorf("unreadable mirror should not mint; mintCalls=%d", gh.mintCalls)
	}
}

// TestResolver_MultiInstallationMintsPerAccount walks the public entry point
// for the shape this unblocks — one App, installed once per GitHub org — in
// both directions at once: each account resolves ITS installation, and the
// mints prove the negative space, that neither account was ever served the
// other's. A token minted from the wrong installation either 422s at GitHub or
// succeeds against repositories the caller never meant to touch.
func TestResolver_MultiInstallationMintsPerAccount(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{
			installOnAccount("456", "1234", "acme"),
			installOnAccount("789", "5678", "globex"),
		}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	for _, owner := range []string{"acme", "globex"} {
		if _, err := r.ClientFor(context.Background(), "org-1", owner); err != nil {
			t.Fatalf("ClientFor(%s): %v", owner, err)
		}
	}
	if got, want := gh.minted(), []string{"456", "789"}; !slices.Equal(got, want) {
		t.Errorf("minted for installations %v; want %v (each account served by its own)", got, want)
	}
}

// TestResolver_MintsForAnInstallationMirroredWithoutAnAccountID walks the
// public entry point for an un-backfilled row: the org's App resolves and mints
// off the login alone, so the column's arrival costs a row that predates it
// nothing.
func TestResolver_MintsForAnInstallationMirroredWithoutAnAccountID(t *testing.T) {
	gh := newGHTestServer(t)
	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOnAccount("456", "", "acme")}},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gh.lastProbe != "Bearer ghs_minted" {
		t.Errorf("client carried %q, want the minted App token", gh.lastProbe)
	}
}
