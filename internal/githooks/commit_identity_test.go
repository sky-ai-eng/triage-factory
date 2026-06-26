package githooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResolveCommitIdentity pins the pure resolver's decision table (TFAC-452,
// TFAC-474): the org name/email (resolved upstream) is always the
// author/committer, passed through verbatim — including the App-bot numeric-id
// noreply email form; the Co-authored-by trailer is emitted iff the run is
// manual AND a distinct delegating user is known, and always uses the plain
// human noreply form (never the numeric form). The exact spellings matter
// because the email/trailer strings are GitHub-linkage-bearing.
func TestResolveCommitIdentity(t *testing.T) {
	const aliceTrailer = "Co-authored-by: alice <alice@users.noreply.github.com>"

	cases := []struct {
		name        string
		orgName     string
		orgEmail    string
		manual      bool
		userLogin   string
		wantStamp   bool
		wantName    string
		wantTrailer string
	}{
		{
			name:    "manual distinct user -> trailer (PAT form)",
			orgName: "octocat", orgEmail: "octocat@users.noreply.github.com",
			manual: true, userLogin: "alice",
			wantStamp: true, wantName: "octocat", wantTrailer: aliceTrailer,
		},
		{
			name:    "manual App org distinct user -> trailer (plain bot email form)",
			orgName: "acme-bot[bot]", orgEmail: "acme-bot[bot]@users.noreply.github.com",
			manual: true, userLogin: "alice",
			wantStamp: true, wantName: "acme-bot[bot]", wantTrailer: aliceTrailer,
		},
		{
			// TFAC-474: an App-bot email in numeric-id noreply form passes through
			// verbatim (the resolver built it), and the manual co-author trailer
			// still uses the plain "<userLogin>@..." human form — the numeric form
			// must never be applied to the co-author. A human login never equals a
			// "<slug>[bot]" name, so a manual App run always co-attributes.
			name:    "manual App org numeric-id email -> verbatim email + plain human trailer",
			orgName: "acme-bot[bot]", orgEmail: "41898282+acme-bot[bot]@users.noreply.github.com",
			manual: true, userLogin: "alice",
			wantStamp: true, wantName: "acme-bot[bot]", wantTrailer: aliceTrailer,
		},
		{
			name:    "manual same login case-insensitive -> no trailer (N=1 same PAT)",
			orgName: "OctoCat", orgEmail: "OctoCat@users.noreply.github.com",
			manual: true, userLogin: "octocat",
			wantStamp: true, wantName: "OctoCat", wantTrailer: "",
		},
		{
			name:    "manual empty user -> no trailer (no GitHub identity)",
			orgName: "octocat", orgEmail: "octocat@users.noreply.github.com",
			manual: true, userLogin: "",
			wantStamp: true, wantName: "octocat", wantTrailer: "",
		},
		{
			name:    "event run -> org only, no trailer",
			orgName: "octocat", orgEmail: "octocat@users.noreply.github.com",
			manual: false, userLogin: "alice",
			wantStamp: true, wantName: "octocat", wantTrailer: "",
		},
		{
			name:    "empty org name -> ok=false, stamp nothing",
			orgName: "", orgEmail: "octocat@users.noreply.github.com",
			manual: true, userLogin: "alice",
			wantStamp: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, stamp := ResolveCommitIdentity(tc.orgName, tc.orgEmail, tc.manual, tc.userLogin)
			if stamp != tc.wantStamp {
				t.Fatalf("stamp = %v, want %v", stamp, tc.wantStamp)
			}
			if !tc.wantStamp {
				if id != (CommitIdentity{}) {
					t.Errorf("ok=false must yield zero CommitIdentity, got %+v", id)
				}
				return
			}
			if id.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", id.Name, tc.wantName)
			}
			// The email is passed through verbatim — the App-vs-PAT form decision
			// lives upstream in the resolver, not here.
			if id.Email != tc.orgEmail {
				t.Errorf("Email = %q, want %q (verbatim passthrough)", id.Email, tc.orgEmail)
			}
			if id.CoAuthorTrailer != tc.wantTrailer {
				t.Errorf("CoAuthorTrailer = %q, want %q", id.CoAuthorTrailer, tc.wantTrailer)
			}
		})
	}
}

// TestIdentityConfigPairs pins the git-config bridge: both halves set yields the
// user.name/user.email pair list; either half empty yields nil (so the
// injection paths add nothing and fall back to ambient config).
func TestIdentityConfigPairs(t *testing.T) {
	got := IdentityConfigPairs("acme-bot[bot]", "acme-bot[bot]@users.noreply.github.com")
	want := [][2]string{
		{"user.name", "acme-bot[bot]"},
		{"user.email", "acme-bot[bot]@users.noreply.github.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if IdentityConfigPairs("", "e@x") != nil {
		t.Error("empty name must yield nil pairs")
	}
	if IdentityConfigPairs("n", "") != nil {
		t.Error("empty email must yield nil pairs")
	}
}

// TestDirectAgentEnv_WithIdentity pins that the org identity layers into the
// SAME GIT_CONFIG_* block as core.hooksPath in the local path: hooks at index 0
// (stable), user.name/user.email at the next free indices, exactly one
// re-emitted GIT_CONFIG_COUNT covering all three. An empty identity reproduces
// the hooks-only block (covered by the existing TestDirectAgentEnv_*).
func TestDirectAgentEnv_WithIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, "/s")
	wantHooks := filepath.Join("/s", "hooks")

	env := DirectAgentEnv([]string{"PATH=/usr/bin"},
		IdentityConfigPairs("acme-bot[bot]", "acme-bot[bot]@users.noreply.github.com")...)

	assertEnv(t, env, "PATH", "/usr/bin")
	assertEnv(t, env, "GIT_CONFIG_KEY_0", ConfigKey)
	assertEnv(t, env, "GIT_CONFIG_VALUE_0", wantHooks)
	assertEnv(t, env, "GIT_CONFIG_KEY_1", "user.name")
	assertEnv(t, env, "GIT_CONFIG_VALUE_1", "acme-bot[bot]")
	assertEnv(t, env, "GIT_CONFIG_KEY_2", "user.email")
	assertEnv(t, env, "GIT_CONFIG_VALUE_2", "acme-bot[bot]@users.noreply.github.com")
	assertEnv(t, env, "GIT_CONFIG_COUNT", "3")
	assertSingleCount(t, env)
}

// TestCommitIdentity_EndToEnd is the git-level acceptance test for TFAC-452: an
// agent env carrying the injected org identity (DirectAgentEnv +
// IdentityConfigPairs) and the co-author trailer var produces a real commit
// authored AND committed by the org identity — overriding the repo's own
// user.* config (the "injected user.* is authoritative" invariant) — with the
// Co-authored-by trailer in the body exactly once, idempotent across an amend.
func TestCommitIdentity_EndToEnd(t *testing.T) {
	requireGit(t)
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}

	const (
		orgName  = "acme-bot[bot]"
		orgEmail = "acme-bot[bot]@users.noreply.github.com"
		trailer  = "Co-authored-by: alice <alice@users.noreply.github.com>"
	)

	home := t.TempDir()
	base := append(sanitizeEnv(os.Environ()),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER="+trailer,
	)
	env := DirectAgentEnv(base, IdentityConfigPairs(orgName, orgEmail)...)

	// A repo whose OWN config sets a different identity — the env-injected config
	// must win, proving identity is authoritative regardless of per-repo config.
	dir := t.TempDir()
	mustGit(t, env, dir, "init", "-b", "main")
	mustGit(t, env, dir, "config", "user.name", "Repo Local")
	mustGit(t, env, dir, "config", "user.email", "repo@local.test")
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	mustGit(t, env, dir, "add", ".")
	mustGit(t, env, dir, "commit", "-m", "first")

	if an := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%an")); an != orgName {
		t.Errorf("author name = %q, want %q (injected identity must override repo config)", an, orgName)
	}
	if ae := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%ae")); ae != orgEmail {
		t.Errorf("author email = %q, want %q", ae, orgEmail)
	}
	if cn := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%cn")); cn != orgName {
		t.Errorf("committer name = %q, want %q", cn, orgName)
	}
	body := mustGit(t, env, dir, "log", "-1", "--format=%B")
	if n := strings.Count(body, trailer); n != 1 {
		t.Errorf("trailer appears %d times after first commit, want 1:\n%s", n, body)
	}

	// Amend re-runs prepare-commit-msg over a message already carrying the
	// trailer; the idempotency guard must keep it at exactly one.
	writeFile(t, filepath.Join(dir, "a.txt"), "a2")
	mustGit(t, env, dir, "add", ".")
	mustGit(t, env, dir, "commit", "--amend", "--no-edit")
	body = mustGit(t, env, dir, "log", "-1", "--format=%B")
	if n := strings.Count(body, trailer); n != 1 {
		t.Errorf("trailer appears %d times after amend, want 1 (idempotent):\n%s", n, body)
	}
}

// TestCommitIdentity_EndToEnd_NumericNoreplyForm is the git-level acceptance for
// TFAC-474: when the resolver hands an App bot the numeric-id noreply email
// "<bot_user_id>+<slug>[bot]@users.noreply.github.com", a real commit carries it
// verbatim as both the author AND committer email — the only form that links a
// bot's commits to its account on github.com. The "+" and digits must survive
// the GIT_CONFIG_* injection untouched, and the NAME stays "<slug>[bot]" (the
// numeric id lives only in the email — locked decision 1).
func TestCommitIdentity_EndToEnd_NumericNoreplyForm(t *testing.T) {
	requireGit(t)
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}

	const (
		orgName  = "acme-bot[bot]"
		orgEmail = "41898282+acme-bot[bot]@users.noreply.github.com"
	)

	home := t.TempDir()
	base := append(sanitizeEnv(os.Environ()),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		// No trailer env: an autonomous/event run stamps the org identity only.
	)
	env := DirectAgentEnv(base, IdentityConfigPairs(orgName, orgEmail)...)

	dir := t.TempDir()
	mustGit(t, env, dir, "init", "-b", "main")
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	mustGit(t, env, dir, "add", ".")
	mustGit(t, env, dir, "commit", "-m", "first")

	if ae := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%ae")); ae != orgEmail {
		t.Errorf("author email = %q, want the numeric-id noreply form %q", ae, orgEmail)
	}
	if ce := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%ce")); ce != orgEmail {
		t.Errorf("committer email = %q, want the numeric-id noreply form %q", ce, orgEmail)
	}
	if an := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%an")); an != orgName {
		t.Errorf("author name = %q, want %q (the numeric id lives only in the email)", an, orgName)
	}
}

// TestPrepareCommitMsg_NoOpWithoutTrailerEnv pins the env gate: with the org
// identity injected but no TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER (an autonomous /
// event run, or the N=1 same-login case), the prepare-commit-msg hook is a
// no-op — the commit is authored by the org identity but carries no trailer.
func TestPrepareCommitMsg_NoOpWithoutTrailerEnv(t *testing.T) {
	requireGit(t)
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}

	const orgName = "acme-bot[bot]"
	const orgEmail = "acme-bot[bot]@users.noreply.github.com"

	home := t.TempDir()
	base := append(sanitizeEnv(os.Environ()),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		// deliberately no TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER
	)
	env := DirectAgentEnv(base, IdentityConfigPairs(orgName, orgEmail)...)

	dir := t.TempDir()
	mustGit(t, env, dir, "init", "-b", "main")
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	mustGit(t, env, dir, "add", ".")
	mustGit(t, env, dir, "commit", "-m", "first")

	if an := strings.TrimSpace(mustGit(t, env, dir, "log", "-1", "--format=%an")); an != orgName {
		t.Errorf("author name = %q, want %q", an, orgName)
	}
	body := mustGit(t, env, dir, "log", "-1", "--format=%B")
	if strings.Contains(body, "Co-authored-by:") {
		t.Errorf("hook appended a trailer with no env var set:\n%s", body)
	}
}
