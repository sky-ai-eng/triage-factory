package gitsig

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary doubles as git's gpg.ssh.program: a git command a test runs
// with these set calls back into it to sign with this package.
const (
	signingKeyEnv = "COMMITSIGN_TEST_SIGNING_KEY"
	payloadOutEnv = "COMMITSIGN_TEST_PAYLOAD_OUT"
)

func TestMain(m *testing.M) {
	if keyFile := os.Getenv(signingKeyEnv); keyFile != "" {
		if err := signForGit(keyFile, os.Getenv(payloadOutEnv), os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// signForGit answers git's call to its signing program,
// `-Y sign -n git -f <public key file> -U <buffer>`: it signs the buffer with
// the key in keyFile, writes <buffer>.sig, and copies the buffer to
// payloadOut, since git deletes it once it has the signature.
func signForGit(keyFile, payloadOut string, args []string) error {
	if len(args) < 5 || args[0] != "-Y" || args[1] != "sign" || args[2] != "-n" || args[3] != "git" {
		return fmt.Errorf("unexpected signing program arguments %q", args)
	}
	buffer := args[len(args)-1]
	payload, err := os.ReadFile(buffer)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	key, err := ParsePrivateKey(data)
	if err != nil {
		return err
	}
	sig, err := Sign(key, payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(payloadOut, payload, 0o600); err != nil {
		return err
	}
	return os.WriteFile(buffer+".sig", sig, 0o600)
}

// requireGit skips unless git is installed and new enough to sign with SSH.
func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(out), "git version %d.%d", &major, &minor); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if major < 2 || major == 2 && minor < 34 {
		t.Skipf("git %d.%d predates SSH signing (2.34)", major, minor)
	}
	return path
}

type gitRepo struct {
	t   *testing.T
	git string
	dir string
	env []string
}

func newGitRepo(t *testing.T, git string, committer Identity) *gitRepo {
	t.Helper()
	r := &gitRepo{t: t, git: git, dir: t.TempDir()}
	r.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Someone Else",
		"GIT_AUTHOR_EMAIL=else@example.com",
		"GIT_AUTHOR_DATE=1600000000 -0700",
		"GIT_COMMITTER_NAME=" + committer.Name,
		"GIT_COMMITTER_EMAIL=" + committer.Email,
		"GIT_COMMITTER_DATE=1700000000 +0100",
	}
	r.run(nil, nil, "init", "-q")
	return r
}

func (r *gitRepo) run(env []string, stdin []byte, args ...string) []byte {
	r.t.Helper()
	cmd := exec.Command(r.git, args...)
	cmd.Dir = r.dir
	cmd.Env = append(append([]string(nil), r.env...), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return out
}

func (r *gitRepo) id(env []string, stdin []byte, args ...string) string {
	r.t.Helper()
	return strings.TrimSpace(string(r.run(env, stdin, args...)))
}

// Git signs each commit through this package, and the object and id
// SignedCommit and CommitID compute for its payload are the ones git wrote.
func TestSignedCommitMatchesGit(t *testing.T) {
	git := requireGit(t)
	committer := Identity{Name: "Triage Factory", Email: "9+tf@users.noreply.github.com"}
	repo := newGitRepo(t, git, committer)
	key := mustKey(t)
	keyFile := writeKeyFile(t, key)
	pub := mustPublicKey(t, key)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, keygenErr := exec.LookPath("ssh-keygen")
	if keygenErr != nil {
		t.Log("ssh-keygen not installed; git verify-commit is not run")
	}

	tree := repo.id(nil, []byte{}, "mktree")
	p1 := repo.id(nil, nil, "commit-tree", tree, "-m", "one")
	p2 := repo.id(nil, nil, "commit-tree", tree, "-p", p1, "-m", "two")
	p3 := repo.id(nil, nil, "commit-tree", tree, "-m", "three")

	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, []byte(committer.Email+` namespaces="git" `+pub+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		parents []string
		message string
	}{
		{name: "root commit", message: "subject\n"},
		{name: "one parent", parents: []string{p1}, message: "subject\n"},
		{
			name:    "three parents and a multi-line message",
			parents: []string{p1, p2, p3},
			message: "Merge three lines\n\nThe body runs\nover several lines.\n\nCo-authored-by: Octo Cat <1+octocat@users.noreply.github.com>\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payloadOut := filepath.Join(t.TempDir(), "payload")
			args := []string{
				"-c", "gpg.format=ssh",
				"-c", "user.signingkey=key::" + pub,
				"-c", "gpg.ssh.program=" + self,
				"commit-tree", "-S", tree,
			}
			for _, p := range tc.parents {
				args = append(args, "-p", p)
			}
			args = append(args, "-F", "-")
			gitID := repo.id([]string{signingKeyEnv + "=" + keyFile, payloadOutEnv + "=" + payloadOut}, []byte(tc.message), args...)

			payload, err := os.ReadFile(payloadOut)
			if err != nil {
				t.Fatalf("git did not call the signing program: %v", err)
			}
			got, err := ParseCommitter(payload)
			if err != nil {
				t.Fatalf("ParseCommitter: %v", err)
			}
			if got != committer {
				t.Fatalf("ParseCommitter = %+v, want %+v", got, committer)
			}

			sig, err := Sign(key, payload)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			object := SignedCommit(payload, sig)
			if written := repo.run(nil, nil, "cat-file", "commit", gitID); !bytes.Equal(object, written) {
				t.Fatalf("SignedCommit =\n%s\ngit wrote\n%s", object, written)
			}
			if id := CommitID(object); id != gitID {
				t.Fatalf("CommitID = %s, git wrote %s", id, gitID)
			}
			if hashed := repo.id(nil, object, "hash-object", "-t", "commit", "--stdin"); hashed != gitID {
				t.Fatalf("git hash-object = %s, git wrote %s", hashed, gitID)
			}
			if keygenErr == nil {
				repo.run(nil, nil, "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", gitID)
			}
		})
	}
}

// IdentName returns the committer name git itself writes for each user.name.
func TestIdentNameMatchesGit(t *testing.T) {
	git := requireGit(t)
	repo := newGitRepo(t, git, Identity{Name: "unused", Email: "unused@example.com"})
	names := []string{
		"Acme Inc.", `"Acme"`, "Acme;", "  Acme  ", "Acme <x>", `'Ac,me'`, `\Acme\`,
		"\tAc\x01me\t", ":Acme:", "Ä \"Acme\" é,", "Ac\nme", ".Acme.", "\x7fAcme",
	}
	tree := repo.id(nil, []byte{}, "mktree")
	for _, name := range names {
		// The repo's env names a committer; drop it so user.name is what git
		// reads.
		cmd := exec.Command(git, "-c", "user.name="+name, "-c", "user.email=a@example.com", "commit-tree", tree, "-m", "x")
		cmd.Dir = repo.dir
		for _, kv := range repo.env {
			if !strings.HasPrefix(kv, "GIT_COMMITTER_NAME=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git commit-tree with user.name %q: %v", name, err)
		}
		object := repo.run(nil, nil, "cat-file", "commit", strings.TrimSpace(string(out)))
		got, err := ParseCommitter(object)
		if err != nil {
			t.Fatalf("ParseCommitter for user.name %q: %v", name, err)
		}
		if want := IdentName(name); got.Name != want {
			t.Errorf("git wrote %q for user.name %q; IdentName = %q", got.Name, name, want)
		}
	}
}
