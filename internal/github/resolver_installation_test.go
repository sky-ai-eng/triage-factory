package github

import (
	"context"
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
	r := NewResolver(
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

	inst, ok := r.installationFor(context.Background(), "org-1", accountRef{ID: "1234", Login: "acme-corp"})
	if !ok {
		t.Fatal("installationFor found nothing for a renamed account; want the installation")
	}
	if inst.InstallationID != "456" {
		t.Fatalf("resolved installation %q; want 456", inst.InstallationID)
	}

	tok, err := r.installationToken(context.Background(), "org-1", activeApp(), inst, gh.srv.URL)
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
			inst, ok := r.installationFor(context.Background(), "org-1", target)
			if !ok || inst.InstallationID != "456" {
				t.Errorf("installationFor(%+v) = (%q, %v); want (456, true)", target, inst.InstallationID, ok)
			}
		}
	})

	t.Run("TargetHasNoID", func(t *testing.T) {
		r, _ := installResolver(t, installOnAccount("456", "1234", "acme"))
		inst, ok := r.installationFor(context.Background(), "org-1", accountByLogin("acme"))
		if !ok || inst.InstallationID != "456" {
			t.Errorf("installationFor(login-only) = (%q, %v); want (456, true)", inst.InstallationID, ok)
		}
	})

	t.Run("NoMatchAtAll", func(t *testing.T) {
		r, _ := installResolver(t, installOnAccount("456", "1234", "acme"))
		if inst, ok := r.installationFor(context.Background(), "org-1", accountRef{ID: "9999", Login: "other"}); ok {
			t.Errorf("installationFor(unknown account) = %q; want no match", inst.InstallationID)
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
			inst, ok := r.installationFor(context.Background(), "org-1", tc.target)
			if !ok {
				t.Fatalf("installationFor(%+v) found nothing; want %s", tc.target, tc.want)
			}
			if inst.InstallationID != tc.want {
				t.Errorf("installationFor(%+v) = %q; want %q", tc.target, inst.InstallationID, tc.want)
			}
		})
	}
}

// TestInstallationFor_EmptyRefUnchanged pins that carrying an id changes
// nothing about the "no specific account" rule: one installation is an
// unambiguous choice, more than one is not.
func TestInstallationFor_EmptyRefUnchanged(t *testing.T) {
	one, _ := installResolver(t, installOnAccount("456", "1234", "acme"))
	if inst, ok := one.installationFor(context.Background(), "org-1", accountRef{}); !ok || inst.InstallationID != "456" {
		t.Errorf("installationFor(empty ref, one install) = (%q, %v); want (456, true)", inst.InstallationID, ok)
	}

	two, _ := installResolver(t,
		installOnAccount("456", "1234", "acme"),
		installOnAccount("789", "5678", "widgets"),
	)
	if inst, ok := two.installationFor(context.Background(), "org-1", accountRef{}); ok {
		t.Errorf("installationFor(empty ref, two installs) = %q; want no match (ambiguous)", inst.InstallationID)
	}
}

// TestResolver_MintsForAnInstallationMirroredWithoutAnAccountID walks the
// public entry point for an un-backfilled row: the org's App resolves and mints
// off the login alone, so the column's arrival costs a row that predates it
// nothing.
func TestResolver_MintsForAnInstallationMirroredWithoutAnAccountID(t *testing.T) {
	gh := newGHTestServer(t)
	r := NewResolver(
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
