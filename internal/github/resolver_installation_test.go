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

// testInstallHost is the host the direct installationFor calls below select
// on. Their fixtures name no host, which the fake store reads as the org's
// current host, so every one of them is on it.
const testInstallHost = "https://ghe.example.test"

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

	inst, err := r.installationFor(context.Background(), "org-1", testInstallHost, accountRef{ID: "1234", Login: "acme-corp"})
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
			inst, err := r.installationFor(context.Background(), "org-1", testInstallHost, target)
			if err != nil || inst.InstallationID != "456" {
				t.Errorf("installationFor(%s) = (%q, %v); want (456, nil)", target, inst.InstallationID, err)
			}
		}
	})

	t.Run("TargetHasNoID", func(t *testing.T) {
		r, _ := installResolver(t, installOnAccount("456", "1234", "acme"))
		inst, err := r.installationFor(context.Background(), "org-1", testInstallHost, accountByLogin("acme"))
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
			inst, err := r.installationFor(context.Background(), "org-1", testInstallHost, tc.target)
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
			inst, err := r.installationFor(context.Background(), "org-1", testInstallHost, tc.target)

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
	_, err := r.installationFor(context.Background(), "org-1", testInstallHost, accountRef{})
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

	_, err := r.installationFor(context.Background(), "org-1", testInstallHost, accountByLogin("acme"))
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

// installHostA is the GitHub an org was on before its settings moved it to the
// host its test server stands for. Nothing listens there, and nothing may be
// minted for it.
const installHostA = "https://ghe.old.example.com"

// installOnHost is installOnAccount on an explicit GitHub host.
func installOnHost(installationID, accountID, login, host string) domain.OrgGitHubAppInstallation {
	inst := installOnAccount(installationID, accountID, login)
	inst.GitHubHost = host
	return inst
}

// hostEntryPoints is every public resolution that mints from an installation,
// each named so a failure says which door let the wrong host through. They
// all act on acme/api, so one fixture serves them all.
func hostEntryPoints(ctx context.Context) map[string]func(r Resolver) error {
	return map[string]func(r Resolver) error{
		"ClientFor": func(r Resolver) error {
			_, err := r.ClientFor(ctx, "org-1", "acme")
			return err
		},
		"ClientForRepo": func(r Resolver) error {
			_, err := r.ClientForRepo(ctx, "org-1", "acme", "api")
			return err
		},
		"ClientForRepoWithIdentity": func(r Resolver) error {
			_, _, err := r.(RepoIdentityResolver).ClientForRepoWithIdentity(ctx, "org-1", "acme", "api")
			return err
		},
		"ClientForRepoScoped": func(r Resolver) error {
			_, _, err := r.(ScopedRepoResolver).ClientForRepoScoped(ctx, "org-1", "acme", "api", nil)
			return err
		},
		"TokenFor": func(r Resolver) error {
			_, err := r.TokenFor(ctx, "org-1", "acme")
			return err
		},
		"TokenForRepoScoped": func(r Resolver) error {
			_, err := r.(ScopedResolver).TokenForRepoScoped(ctx, "org-1", "acme", "api", nil)
			return err
		},
		"TokenForReposScoped": func(r Resolver) error {
			_, err := r.(ScopedResolver).TokenForReposScoped(ctx, "org-1", "acme", []string{"api"}, nil)
			return err
		},
	}
}

// hostResolverOn builds a resolver for an org whose settings name gh's host,
// with insts mirrored, and a PAT in the store that an active-App org must
// never fall back to.
func hostResolverOn(t *testing.T, gh *ghTestServer, insts ...domain.OrgGitHubAppInstallation) Resolver {
	t.Helper()
	gh.installRepos = []string{"acme/api"}
	return newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t), integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: activeApp(), insts: insts},
		&fakeOrgs{base: gh.srv.URL},
		&fakeAgents{},
		nil,
	)
}

// TestResolver_SelectsInstallationsOnTheOrgsHostOnly pins the rule for an org
// that has moved to another GitHub and holds an installation for the same
// account login on both: every resolution on the new host mints from the new
// host's installation and never from the old one's. A login names an account
// on one deployment only, and the old installation's id means a different
// installation on the new host — minting it there acts as whichever account
// holds that number.
func TestResolver_SelectsInstallationsOnTheOrgsHostOnly(t *testing.T) {
	ctx := context.Background()
	for name, resolve := range hostEntryPoints(ctx) {
		t.Run(name, func(t *testing.T) {
			gh := newGHTestServer(t)
			hostB := domain.GitHubHost(gh.srv.URL)
			r := hostResolverOn(t, gh,
				installOnHost("111", "1000", "acme", installHostA),
				installOnHost("222", "2000", "acme", hostB),
			)
			if err := resolve(r); err != nil {
				t.Fatalf("%s on host B: %v", name, err)
			}
			if got, want := gh.minted(), []string{"222"}; !slices.Equal(got, want) {
				t.Errorf("%s minted for installations %v; want %v — host B's installation, never host A's", name, got, want)
			}
		})
	}

	t.Run("HasAnyCredential", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := hostResolverOn(t, gh,
			installOnHost("111", "1000", "acme", installHostA),
			installOnHost("222", "2000", "acme", domain.GitHubHost(gh.srv.URL)),
		)
		if ok, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1"); err != nil || !ok {
			t.Errorf("HasAnyCredential = (%v, %v); want (true, nil)", ok, err)
		}
	})

	t.Run("NoTarget", func(t *testing.T) {
		// One installation on the org's host is one installation to choose
		// from, whatever the org holds elsewhere: the empty target resolves to
		// it rather than reporting the two rows as ambiguous.
		gh := newGHTestServer(t)
		r := hostResolverOn(t, gh,
			installOnHost("111", "1000", "acme", installHostA),
			installOnHost("222", "2000", "acme", domain.GitHubHost(gh.srv.URL)),
		)
		if _, err := r.ClientFor(ctx, "org-1", ""); err != nil {
			t.Fatalf("ClientFor(no target): %v", err)
		}
		if got, want := gh.minted(), []string{"222"}; !slices.Equal(got, want) {
			t.Errorf("minted for installations %v; want %v", got, want)
		}
	})
}

// TestResolver_InstallationOnlyOnAnotherHostIsNoCredential is the other half:
// with the account installed only on the host the org left, a resolution on
// the new host has no installation to mint from. That is the no-credential
// error — the App reaches nothing here — not an ambiguity, not the PAT the org
// still holds, and never a mint of the old host's installation id.
func TestResolver_InstallationOnlyOnAnotherHostIsNoCredential(t *testing.T) {
	ctx := context.Background()
	for name, resolve := range hostEntryPoints(ctx) {
		t.Run(name, func(t *testing.T) {
			gh := newGHTestServer(t)
			r := hostResolverOn(t, gh, installOnHost("111", "1000", "acme", installHostA))
			err := resolve(r)
			if !errors.Is(err, ErrNoGitHubCredentials) {
				t.Fatalf("%s on host B with only host A's installation: err = %v; want ErrNoGitHubCredentials", name, err)
			}
			if errors.Is(err, ErrAmbiguousInstallation) {
				t.Errorf("%s err = %v; a missing installation is not an ambiguous one", name, err)
			}
			if got := gh.minted(); len(got) != 0 {
				t.Errorf("%s minted for installations %v; want none", name, got)
			}
		})
	}

	t.Run("HasAnyCredential", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := hostResolverOn(t, gh, installOnHost("111", "1000", "acme", installHostA))
		if ok, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1"); err != nil || ok {
			t.Errorf("HasAnyCredential = (%v, %v); want (false, nil) — an installation on another host mints nothing here", ok, err)
		}
	})

	t.Run("NoTarget", func(t *testing.T) {
		gh := newGHTestServer(t)
		r := hostResolverOn(t, gh, installOnHost("111", "1000", "acme", installHostA))
		if _, err := r.ClientFor(ctx, "org-1", ""); !errors.Is(err, ErrNoGitHubCredentials) {
			t.Errorf("ClientFor(no target) err = %v; want ErrNoGitHubCredentials", err)
		}
	})
}

// TestInstallationFor_AccountIDsAreHostScoped pins the id half of the rule. An
// account id is GitHub's per-deployment number, so the same id on two hosts is
// two accounts: the target's id selects among the installations on the host
// asked about, and a row on another host whose id matches answers nothing —
// even when its login matches too.
func TestInstallationFor_AccountIDsAreHostScoped(t *testing.T) {
	const hostB = "https://ghe.new.example.com"
	r, _ := installResolver(t,
		installOnHost("111", "1000", "acme", installHostA),
		installOnHost("222", "1000", "globex", hostB),
	)
	inst, err := r.installationFor(context.Background(), "org-1", hostB, accountRef{ID: "1000", Login: "acme"})
	if err != nil {
		t.Fatalf("installationFor on host B: %v", err)
	}
	if inst.InstallationID != "222" {
		t.Errorf("installationFor on host B = %q; want 222 — account 1000 on host B, not host A's acme", inst.InstallationID)
	}
}

// TestInstallationFor_EmptyHostSelectsNothing pins the empty host as "no host
// resolved" rather than the deployment default: nothing is selected, whatever
// the mirror holds, and the answer is the no-credential one.
func TestInstallationFor_EmptyHostSelectsNothing(t *testing.T) {
	r, _ := installResolver(t,
		installOnAccount("456", "1234", "acme"),
		installOnHost("789", "5678", "acme", domain.GitHubHost("")),
	)
	if _, err := r.installationFor(context.Background(), "org-1", "", accountByLogin("acme")); !errors.Is(err, ErrNoGitHubCredentials) {
		t.Errorf("installationFor on an empty host err = %v; want ErrNoGitHubCredentials", err)
	}
	if got := installationHost(""); got != "" {
		t.Errorf("installationHost(\"\") = %q; want \"\" — an empty base resolves no host", got)
	}
}
