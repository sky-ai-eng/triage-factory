package hook

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newPolicyStores opens a provisioned local install (schema + the synthetic
// org/team rows), which is what the pre-push hook actually runs against.
func newPolicyStores(t *testing.T) db.Stores {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(conn)
}

func setPushPolicy(t *testing.T, stores db.Stores, policy string) {
	t.Helper()
	ctx := context.Background()
	set, err := stores.Teams.GetSettingsSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("read team settings: %v", err)
	}
	set.BaseBranchPushPolicy = policy
	if err := stores.Teams.UpdateSettings(ctx, runmode.LocalDefaultTeamID, set); err != nil {
		t.Fatalf("write team settings: %v", err)
	}
}

// prePushStdin renders git's pre-push stdin format for one ref.
func prePushStdin(remoteRef string) *strings.Reader {
	return strings.NewReader("refs/heads/local abc123 " + remoteRef + " def456\n")
}

func policyHost(stores db.Stores, eventTriggered bool) agenthost.Client {
	return agenthost.NewLocal(stores, agenthost.ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   "r1",
		IsEventTriggered: eventTriggered,
	})
}

// TestCheckPush_Policy is local mode's half of the base-branch push policy —
// the same matrix the multi-mode ref gate enforces, reached through the
// pre-push hook instead of a proxy.
func TestCheckPush_Policy(t *testing.T) {
	cases := []struct {
		name           string
		policy         string
		eventTriggered bool
		ref            string
		want           int
	}{
		{"default refuses the default branch", domain.BaseBranchPushNever, false, "refs/heads/main", ExitRefused},
		{"default refuses master too", domain.BaseBranchPushNever, false, "refs/heads/master", ExitRefused},
		{"a feature branch is never the policy's business", domain.BaseBranchPushNever, false, "refs/heads/agent/x", 0},
		{"always permits it", domain.BaseBranchPushAlways, false, "refs/heads/main", 0},
		{"manual_only permits a human-dispatched run", domain.BaseBranchPushManualOnly, false, "refs/heads/main", 0},
		{"manual_only refuses an event-triggered run", domain.BaseBranchPushManualOnly, true, "refs/heads/main", ExitRefused},
		// A tag push carries no branch policy — nothing to protect.
		{"tags pass through", domain.BaseBranchPushNever, false, "refs/tags/v1", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stores := newPolicyStores(t)
			setPushPolicy(t, stores, c.policy)
			got := runCheckPush(policyHost(stores, c.eventTriggered), stores,
				[]string{"--remote", "https://github.com/octo/repo.git"}, prePushStdin(c.ref))
			if got != c.want {
				t.Errorf("exit = %d, want %d", got, c.want)
			}
		})
	}
}

// TestCheckPush_UnprofiledRepoStillRefusesMain: acceptance criterion 5. A repo
// TF has never profiled has no recorded default branch, and that is exactly
// when it must fall back to the universal names rather than shrug.
func TestCheckPush_UnprofiledRepoStillRefusesMain(t *testing.T) {
	stores := newPolicyStores(t)
	got := runCheckPush(policyHost(stores, false), stores,
		[]string{"--remote", "https://github.com/octo/never-profiled.git"}, prePushStdin("refs/heads/main"))
	if got != ExitRefused {
		t.Errorf("exit = %d, want %d (an unprofiled repo still protects main)", got, ExitRefused)
	}
}

// TestCheckPush_ProfiledDefaultBranchIsProtected: the protected set follows the
// repo, not just the two universal names.
func TestCheckPush_ProfiledDefaultBranchIsProtected(t *testing.T) {
	stores := newPolicyStores(t)
	if _, err := stores.Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{
		Owner: "octo", Repo: "repo",
		DefaultBranch: "trunk", CloneURL: "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if got := runCheckPush(policyHost(stores, false), stores,
		[]string{"--remote", "https://github.com/octo/repo.git"}, prePushStdin("refs/heads/trunk")); got != ExitRefused {
		t.Errorf("exit = %d, want %d (the repository's default branch is protected)", got, ExitRefused)
	}
}

// TestCheckPush_FailsOpen is acceptance criterion 6, and the property that
// keeps this a usable guard rather than a tripwire: anything it cannot
// evaluate lets the push through.
func TestCheckPush_FailsOpen(t *testing.T) {
	stores := newPolicyStores(t)
	host := policyHost(stores, false)

	cases := []struct {
		name  string
		args  []string
		stdin *strings.Reader
		// noStores models an unreachable database — the policy read errors out.
		noStores bool
	}{
		{name: "no remote", args: []string{}, stdin: prePushStdin("refs/heads/main")},
		{name: "no refs on stdin", args: []string{"--remote", "https://github.com/octo/repo.git"}, stdin: strings.NewReader("")},
		{name: "not the org's GitHub host", args: []string{"--remote", "https://gitlab.com/octo/repo.git"}, stdin: prePushStdin("refs/heads/main")},
		{name: "unparseable stdin line", args: []string{"--remote", "https://github.com/octo/repo.git"}, stdin: strings.NewReader("garbage\n")},
		{name: "policy unreadable", args: []string{"--remote", "https://github.com/octo/repo.git"}, stdin: prePushStdin("refs/heads/main"), noStores: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := stores
			if c.noStores {
				s = db.Stores{}
			}
			if got := runCheckPush(host, s, c.args, c.stdin); got != 0 {
				t.Errorf("exit = %d, want 0 (a guard that cannot evaluate policy must allow the push)", got)
			}
		})
	}
}

// TestReadPushRefs pins the stdin parse: git's own pre-push format, remote ref
// (field 3) — the name the policy is about, since a push refspec can map a
// local branch onto a different remote one. Deletes stay in, because deleting
// the base branch is the mistake this guard most wants to catch. Everything
// that isn't exactly git's four-field shape is dropped: a line we had to guess
// at could put anything in the ref position, and guessing wrong here REFUSES a
// push rather than degrading to "allow".
func TestReadPushRefs(t *testing.T) {
	in := strings.NewReader(
		"refs/heads/x abc refs/heads/main def\n" +
			"\n" +
			"short line\n" +
			// Three fields: not git's format. Under a looser parse this would
			// read as a push to refs/heads/main and refuse one that never was.
			"refs/heads/x abc refs/heads/main\n" +
			// Four fields, but the ref position holds something that isn't a ref.
			"one two three four\n" +
			// Five fields: also not git's format.
			"refs/heads/x abc refs/heads/main def extra\n" +
			"refs/heads/y 0000000000000000000000000000000000000000 refs/heads/master 111\n")
	got := readPushRefs(in)
	want := []string{"refs/heads/main", "refs/heads/master"}
	if len(got) != len(want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("refs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCheckPush_MalformedLinesDoNotRefuse is the fail-open guarantee stated as
// behavior rather than as a parse detail: input that isn't git's format must
// never produce a refusal, however protected-looking the tokens in it are.
func TestCheckPush_MalformedLinesDoNotRefuse(t *testing.T) {
	stores := newPolicyStores(t)
	host := policyHost(stores, false)
	for _, line := range []string{
		"refs/heads/x abc refs/heads/main\n",     // three fields
		"refs/heads/main\n",                      // one field
		"refs/heads/x abc refs/heads/main a b\n", // five fields
		"main\n",                                 // a bare branch name
		"refs/heads/x abc main 0000\n",           // ref position isn't a ref
	} {
		if got := runCheckPush(host, stores,
			[]string{"--remote", "https://github.com/octo/repo.git"}, strings.NewReader(line)); got != 0 {
			t.Errorf("exit = %d for malformed input %q, want 0", got, line)
		}
	}
}
