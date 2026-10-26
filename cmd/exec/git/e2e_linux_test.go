//go:build linux

package git

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testSocketEnv names the agenthost socket the test binary dials when git runs
// it as TRIAGE_FACTORY_BIN: the ssh-sign shim calls `<bin> exec git sign …`,
// and TestMain answers that call as the jailed CLI would, over the socket.
const testSocketEnv = "TF_TEST_AGENTHOST_SOCKET"

func TestMain(m *testing.M) {
	if sock := os.Getenv(testSocketEnv); sock != "" && len(os.Args) > 2 && os.Args[1] == "exec" && os.Args[2] == "git" {
		client := agenthost.Dial(sock)
		os.Exit(Handle(context.Background(), func() agenthost.Client { return client }, os.Args[3:], os.Stderr))
	}
	os.Exit(m.Run())
}

// memSecrets is the secret store's system read over a map. A read armed by
// failRead fails, the way a keychain that does not answer would.
type memSecrets struct {
	db.SecretStore
	mu     sync.Mutex
	values map[string]string
	// failIn counts down the reads before the one that fails; zero fails
	// none.
	failIn int
}

func (m *memSecrets) GetSystem(_ context.Context, _, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failIn > 0 {
		m.failIn--
		if m.failIn == 0 {
			return "", errors.New("the keychain did not answer")
		}
	}
	return m.values[key], nil
}

func (m *memSecrets) set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
}

// failRead fails the nth read from now. A signature reads its key once, so
// that is the nth signature's.
func (m *memSecrets) failRead(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failIn = n
}

// signingRun is one local run whose org has an active signer, served by its
// agenthost daemon, and a repository whose git is configured as the spawner
// configures a signing run's.
type signingRun struct {
	t       *testing.T
	git     string
	dir     string
	env     []string
	conn    *sql.DB
	stores  db.Stores
	secrets *memSecrets
	signer  commitsigner.Identity
}

// startSigningRun registers the signer with profileName and key, resolves the
// run's identity as the spawner does when a run starts, writes the
// allowed-signers file, and starts the run's daemon. arrange, when set, runs
// before the run starts.
func startSigningRun(t *testing.T, profileName string, key ed25519.PrivateKey, arrange func(*signingRun)) *signingRun {
	t.Helper()
	git := requireSigningTools(t)
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := githooks.Ensure(); err != nil {
		t.Fatalf("githooks.Ensure: %v", err)
	}

	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	r := &signingRun{t: t, git: git, dir: t.TempDir(), conn: conn, stores: sqlitestore.New(conn)}
	r.secrets = &memSecrets{values: map[string]string{integrations.KeyGitHubCommitSigningKey: marshal(t, key)}}
	r.stores.Secrets = r.secrets

	ctx := context.Background()
	public := publicKey(t, key)
	if _, err := r.stores.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: runmode.LocalDefaultOrgID, Host: "https://github.com", GitHubUserID: 583231,
		Login: "octocat", Name: profileName, PublicKey: public,
	}); err != nil {
		t.Fatalf("Create signer: %v", err)
	}
	if _, err := r.stores.OrgCommitSigners.Activate(ctx, runmode.LocalDefaultOrgID, public); err != nil {
		t.Fatalf("Activate signer: %v", err)
	}
	if arrange != nil {
		arrange(r)
	}

	conversationID := uuid.New().String()
	dbtest.SeedConversation(t, conn, domain.Conversation{ID: conversationID})
	claimID := dbtest.SeedActiveClaim(t, conn, conversationID, "executor-1", 1)
	r.signer, err = commitsigner.Resolver{Signers: r.stores.OrgCommitSigners}.Resolve(ctx, runmode.LocalDefaultOrgID)
	if err != nil || !r.signer.Mode.Signs() {
		t.Fatalf("Resolve = %+v, %v; want a signing identity", r.signer, err)
	}

	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, githooks.AllowedSigners(r.signer.Email, r.signer.PublicKey, r.signer.PendingPublicKey), 0o600); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "tfsign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	daemon, err := agenthost.StartLocal(r.stores, agenthost.ConversationInfo{
		OrgID: runmode.LocalDefaultOrgID, ConversationID: conversationID, IsEventTriggered: true,
	}, filepath.Join(sockDir, "run.sock"), &agenthost.LocalCommitSigning{
		ClaimID: claimID, BootEpoch: 1, Signer: r.signer,
		Budgets: commitsigner.Budgets{PerOrgMinute: commitsigner.DefaultMaxSignaturesPerOrgMinute, PerClaim: commitsigner.DefaultMaxSignaturesPerClaim},
	})
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Close() })

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pairs := append(githooks.IdentityConfigPairs(r.signer.Name, r.signer.Email),
		githooks.SigningConfigPairs(githooks.HostDir(), r.signer.PublicKey, allowed)...)
	r.env = githooks.DirectAgentEnv([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		githooks.BinEnvVar + "=" + self,
		testSocketEnv + "=" + daemon.SocketPath(),
	}, pairs...)
	r.run("init", "-q", "-b", "main")
	return r
}

// requireSigningTools skips unless git signs with SSH keys and ssh-keygen,
// which checks the signatures, is installed.
func requireSigningTools(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if git := commitsigner.ProbeLocalGit(context.Background()); git.UnsignedReason != "" {
		t.Skipf("git %s cannot sign with an SSH key", git.Version)
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	return path
}

func (r *signingRun) cmd(args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(r.git, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func (r *signingRun) run(args ...string) string {
	r.t.Helper()
	out, errOut, err := r.cmd(args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errOut)
	}
	return strings.TrimSpace(out)
}

func (r *signingRun) commitFile(name, message string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(message+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.run("add", name)
	r.run("commit", "-q", "-m", message)
}

// assertSigned checks that git verifies rev inside the run, and that the
// signature log holds the commit id with the key that signed it.
func (r *signingRun) assertSigned(rev, publicKey string) {
	r.t.Helper()
	if _, errOut, err := r.cmd("verify-commit", rev); err != nil {
		r.t.Fatalf("git verify-commit %s: %v\n%s", rev, err, errOut)
	}
	sha := r.run("rev-parse", rev)
	row, err := r.stores.CommitSignatures.ForCommitSystem(context.Background(), runmode.LocalDefaultOrgID, sha)
	if err != nil || row == nil {
		r.t.Fatalf("no signature row for %s (%s): %v", rev, sha, err)
	}
	if row.PublicKey != publicKey {
		r.t.Errorf("%s was signed with %q, want %q", rev, row.PublicKey, publicKey)
	}
}

func marshal(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	raw, err := commitsign.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func publicKey(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	public, err := commitsign.PublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return public
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A commit in a signing run is signed by the run's daemon through the shim,
// verifies inside the run, and is in the signature log under its commit id.
// The signer's profile name is "Acme" with the quotes: git writes Acme, and
// the identity the run was given is already Acme, so the committer check
// passes.
func TestSigningRun_CommitIsSignedAndVerifies(t *testing.T) {
	key := newKey(t)
	r := startSigningRun(t, `"Acme"`, key, nil)
	if r.signer.Name != "Acme" {
		t.Fatalf("the run's name is %q, want Acme", r.signer.Name)
	}

	r.commitFile("a.txt", "first")
	r.assertSigned("HEAD", publicKey(t, key))
	object := r.run("cat-file", "commit", "HEAD")
	if want := "committer Acme <583231+octocat@users.noreply.github.com> "; !strings.Contains(object, want) {
		t.Errorf("commit object has no %q:\n%s", want, object)
	}
	if !strings.Contains(object, "-----BEGIN SSH SIGNATURE-----") {
		t.Errorf("commit object carries no signature:\n%s", object)
	}
	if out := r.run("log", "--show-signature", "-1"); !strings.Contains(out, `Good "git" signature for 583231+octocat@users.noreply.github.com`) {
		t.Errorf("git log --show-signature:\n%s", out)
	}
}

// A commit naming another committer is refused with the line the agent reads,
// nothing is committed, and the refusal is recorded.
func TestSigningRun_AnotherCommitterIsRefused(t *testing.T) {
	r := startSigningRun(t, "The Octocat", newKey(t), nil)
	r.commitFile("a.txt", "first")
	head := r.run("rev-parse", "HEAD")

	_, errOut, err := r.cmd("-c", "user.name=Mallory", "commit", "--allow-empty", "-m", "not mine")
	if err == nil {
		t.Fatal("a commit as Mallory was signed")
	}
	want := "triagefactory: this run signs commits only as The Octocat <583231+octocat@users.noreply.github.com>, " +
		"and this commit's committer is Mallory <583231+octocat@users.noreply.github.com>. Nothing was committed."
	if !strings.Contains(errOut, want) {
		t.Errorf("git's stderr has no refusal line:\n%s", errOut)
	}
	if got := r.run("rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s after a refused signature", got)
	}
	var denied int
	if err := r.conn.QueryRow(`SELECT COUNT(*) FROM external_actions WHERE action = ?`, domain.ActionCommitSignDenied).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if denied != 1 {
		t.Errorf("commit_sign_denied rows = %d, want 1", denied)
	}
}

// topicBehindMain builds main with one commit, topic with three on it, and a
// further commit on main, and leaves topic checked out: a rebase of topic onto
// main rewrites, and so re-signs, three commits. conflict, when set, makes
// the second topic commit and main's later commit change the same file.
func (r *signingRun) topicBehindMain(conflict bool) {
	r.t.Helper()
	r.commitFile("base.txt", "base")
	r.run("checkout", "-q", "-b", "topic")
	r.commitFile("topic1.txt", "topic 1")
	if conflict {
		r.commitFile("shared.txt", "topic 2")
	} else {
		r.commitFile("topic2.txt", "topic 2")
	}
	r.commitFile("topic3.txt", "topic 3")
	r.run("checkout", "-q", "main")
	if conflict {
		r.commitFile("shared.txt", "main moves on")
	} else {
		r.commitFile("main.txt", "main moves on")
	}
	r.run("checkout", "-q", "topic")
}

// assertRebased checks that topic's three commits now sit on main.
func (r *signingRun) assertRebased() {
	r.t.Helper()
	if got := r.run("log", "--format=%s", "main..topic"); got != "topic 3\ntopic 2\ntopic 1" {
		r.t.Errorf("topic after the rebase:\n%s", got)
	}
	if base := r.run("merge-base", "main", "topic"); base != r.run("rev-parse", "main") {
		r.t.Errorf("topic is not on main after the rebase")
	}
}

// A rebase re-signs every commit it rewrites, and the refusal lines tell the
// agent how a rebase that stopped on a signature carries on. These follow the
// lines' instructions through git itself, in each of the ways a rebase stops.
func TestSigningRun_RebaseStoppedBySigning(t *testing.T) {
	// A commit the rebase picks cleanly and then cannot sign is rescheduled
	// with its changes left staged. git rebase --continue refuses that, and
	// after git reset --hard the rescheduled pick applies and signs them.
	t.Run("a failed pick resumes after a reset", func(t *testing.T) {
		key := newKey(t)
		r := startSigningRun(t, "The Octocat", key, nil)
		r.topicBehindMain(false)
		r.secrets.failRead(2)

		_, errOut, err := r.cmd("rebase", "main")
		if err == nil {
			t.Fatal("the rebase finished although a signature failed")
		}
		if !strings.Contains(errOut, commitsigner.UnansweredLine()) {
			t.Errorf("the rebase's stderr has no unanswered line:\n%s", errOut)
		}
		if _, errOut, err := r.cmd("rebase", "--continue"); err == nil || !strings.Contains(errOut, "staged changes") {
			t.Fatalf("git rebase --continue over the rescheduled pick = %v, want the staged-changes refusal the line names:\n%s", err, errOut)
		}
		r.run("reset", "-q", "--hard")
		r.run("rebase", "--continue")
		r.assertRebased()
		for _, rev := range []string{"topic~2", "topic~1", "topic"} {
			r.assertSigned(rev, publicKey(t, key))
		}
	})

	// A commit that fails inside git rebase --continue, after the agent
	// resolved a conflict, is not rescheduled: the next --continue commits
	// the resolution, and no reset is needed or wanted.
	t.Run("a failed commit of a resolution resumes as it is", func(t *testing.T) {
		key := newKey(t)
		r := startSigningRun(t, "The Octocat", key, nil)
		r.topicBehindMain(true)
		if _, _, err := r.cmd("rebase", "main"); err == nil {
			t.Fatal("the rebase did not stop on the conflict")
		}
		if err := os.WriteFile(filepath.Join(r.dir, "shared.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r.run("add", "shared.txt")
		r.secrets.failRead(1)

		_, errOut, err := r.cmd("rebase", "--continue")
		if err == nil {
			t.Fatal("the rebase finished although a signature failed")
		}
		if !strings.Contains(errOut, commitsigner.UnansweredLine()) {
			t.Errorf("git rebase --continue's stderr has no unanswered line:\n%s", errOut)
		}
		r.run("rebase", "--continue")
		r.assertRebased()
		if got, err := os.ReadFile(filepath.Join(r.dir, "shared.txt")); err != nil || string(got) != "resolved\n" {
			t.Errorf("shared.txt = %q, %v; the resolution was lost", got, err)
		}
		for _, rev := range []string{"topic~2", "topic~1", "topic"} {
			r.assertSigned(rev, publicKey(t, key))
		}
	})

	// Once signing is off for the run, a rebase that already started keeps
	// signing whatever -c commit.gpgsign=false says, so the line says to abort
	// it and run it again unsigned.
	t.Run("signing off restarts the rebase unsigned", func(t *testing.T) {
		r := startSigningRun(t, "The Octocat", newKey(t), nil)
		r.topicBehindMain(false)
		if _, err := r.conn.Exec(`UPDATE org_commit_signers SET status = 'broken', status_reason = 'key_missing'`); err != nil {
			t.Fatal(err)
		}

		_, errOut, err := r.cmd("rebase", "main")
		if err == nil {
			t.Fatal("the rebase finished although signing is off")
		}
		if !strings.Contains(errOut, "commit signing is off for the rest of this run: the signer's key is no longer on its GitHub account.") ||
			!strings.Contains(errOut, "run git rebase --abort, then the rebase again with git -c commit.gpgsign=false") {
			t.Errorf("the rebase's stderr has no signing-off line:\n%s", errOut)
		}
		// What the abort is for: the rebase still signs.
		if _, _, err := r.cmd("-c", "commit.gpgsign=false", "rebase", "--continue"); err == nil {
			t.Fatal("git rebase --continue with signing off finished; the line's abort is not needed")
		}
		r.run("rebase", "--abort")
		r.run("-c", "commit.gpgsign=false", "rebase", "main")
		r.assertRebased()
		if object := r.run("cat-file", "commit", "topic"); strings.Contains(object, "gpgsig") {
			t.Errorf("the rerun rebase signed:\n%s", object)
		}
	})
}

// Git does not compare the key that signs with user.signingkey. A run started
// during a rotation keeps the old key in its git config; once verify promotes
// the new key, the daemon signs with it, git takes the signature, and the
// run's allowed-signers file, written with both keys, verifies the commits on
// either side of the promotion.
func TestSigningRun_CommitsAcrossAKeyRotation(t *testing.T) {
	oldKey, newKey := newKey(t), newKey(t)
	oldPublic, newPublic := publicKey(t, oldKey), publicKey(t, newKey)
	r := startSigningRun(t, "The Octocat", oldKey, func(r *signingRun) {
		r.secrets.set(integrations.KeyGitHubCommitSigningKeyPending, marshal(t, newKey))
		if _, err := r.stores.OrgCommitSigners.StagePendingKey(context.Background(), runmode.LocalDefaultOrgID, newPublic); err != nil {
			t.Fatalf("StagePendingKey: %v", err)
		}
	})
	if r.signer.PublicKey != oldPublic || r.signer.PendingPublicKey != newPublic {
		t.Fatalf("the run started with %q (pending %q)", r.signer.PublicKey, r.signer.PendingPublicKey)
	}

	r.commitFile("a.txt", "before the promotion")
	r.assertSigned("HEAD", oldPublic)

	// Verify promotes the pending key and destroys the old private half.
	if _, err := r.stores.OrgCommitSigners.Activate(context.Background(), runmode.LocalDefaultOrgID, newPublic); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	r.secrets.set(integrations.KeyGitHubCommitSigningKey, marshal(t, newKey))
	r.secrets.set(integrations.KeyGitHubCommitSigningKeyPending, "")
	if got := r.run("config", "user.signingkey"); got != "key::"+oldPublic {
		t.Fatalf("the run's git config names %q", got)
	}

	r.commitFile("b.txt", "after the promotion")
	r.assertSigned("HEAD", newPublic)
	r.assertSigned("HEAD~1", oldPublic)
}
