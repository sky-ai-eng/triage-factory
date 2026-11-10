package githooks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// withStubLFS puts a stand-in git-lfs first on PATH — `git lfs` runs whatever
// git-lfs it finds there — that appends its argv and stdin to the returned log
// and exits with exit. It stands in for the real one so these tests pin what
// the hook hands git-lfs, not what git-lfs then does with it.
func withStubLFS(t *testing.T, env []string, exit int) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "lfs.log")
	stub := "#!/bin/sh\nprintf 'argv: %s\\n' \"$*\" >> \"$LFS_LOG\"\ncat >> \"$LFS_LOG\"\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git-lfs"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write git-lfs stub: %v", err)
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			kv = "PATH=" + dir + string(os.PathListSeparator) + v
		}
		out = append(out, kv)
	}
	return append(out, "LFS_LOG="+log), log
}

// markLFSRepo gives work the directory git-lfs creates under the git dir once
// it has stored anything, which is how the hook tells an LFS repository apart.
func markLFSRepo(t *testing.T, env []string, work string) {
	t.Helper()
	gitDir := strings.TrimSpace(mustGit(t, env, work, "rev-parse", "--absolute-git-dir"))
	if err := os.MkdirAll(filepath.Join(gitDir, "lfs", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func lfsLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestPrePushHook_RunsLFSUnderProxyCapture is the case every managed run is:
// a proxy owns policy and recording, and the hook's one remaining job is the
// LFS upload git would have run from git-lfs's own hook. git-lfs gets git's
// arguments and the whole ref list.
func TestPrePushHook_RunsLFSUnderProxyCapture(t *testing.T) {
	requireGit(t)
	env, recLog := setupEmbeddedHook(t, 0)
	env = append(env, PushCaptureEnvVar+"="+PushCaptureProxy)
	env, log := withStubLFS(t, env, 0)
	work := newRepoWithRemote(t, env)
	markLFSRepo(t, env, work)

	mustGit(t, env, work, "push", "-u", "origin", "main")

	got := lfsLog(t, log)
	head := strings.TrimSpace(mustGit(t, env, work, "rev-parse", "HEAD"))
	if !strings.Contains(got, "argv: pre-push origin ") || !strings.Contains(got, "refs/heads/main "+head+" refs/heads/main ") {
		t.Errorf("git-lfs was not handed the push: %q", got)
	}
	if rec, err := os.ReadFile(recLog); err == nil && strings.Contains(string(rec), "record-push") {
		t.Errorf("hook recorded %q under proxy capture; the proxy owns recording", rec)
	}
}

// TestPrePushHook_SkipsLFSWithoutLFSObjects pins that a repository git-lfs
// has never stored anything in is left alone: nothing in it can need
// uploading.
func TestPrePushHook_SkipsLFSWithoutLFSObjects(t *testing.T) {
	requireGit(t)
	for name, proxy := range map[string]bool{"proxy capture": true, "hook capture": false} {
		t.Run(name, func(t *testing.T) {
			env, _ := setupEmbeddedHook(t, 0)
			if proxy {
				env = append(env, PushCaptureEnvVar+"="+PushCaptureProxy)
			}
			env, log := withStubLFS(t, env, 0)
			work := newRepoWithRemote(t, env)

			mustGit(t, env, work, "push", "-u", "origin", "main")
			if got := lfsLog(t, log); got != "" {
				t.Errorf("git-lfs ran for a repository with no LFS objects: %q", got)
			}
		})
	}
}

// TestPrePushHook_LFSFailureAbortsPush keeps git-lfs's own semantics: a push
// whose objects could not be uploaded does not happen.
func TestPrePushHook_LFSFailureAbortsPush(t *testing.T) {
	requireGit(t)
	for name, proxy := range map[string]bool{"proxy capture": true, "hook capture": false} {
		t.Run(name, func(t *testing.T) {
			env, recLog := setupEmbeddedHook(t, 0)
			if proxy {
				env = append(env, PushCaptureEnvVar+"="+PushCaptureProxy)
			}
			env, _ = withStubLFS(t, env, 2)
			work := newRepoWithRemote(t, env)
			markLFSRepo(t, env, work)

			if out, err := runGit(env, work, "push", "-u", "origin", "main"); err == nil {
				t.Fatalf("push succeeded though its LFS upload failed:\n%s", out)
			}
			if rec, err := os.ReadFile(recLog); err == nil && strings.Contains(string(rec), "record-push") {
				t.Errorf("recorded %q for a push that did not happen", rec)
			}
		})
	}
}

// TestPrePushHook_LFSRunsBetweenPolicyAndRecording pins the order under hook
// capture: a push the policy refuses uploads nothing, and an allowed one is
// uploaded and then recorded, with each pass seeing the same refs.
func TestPrePushHook_LFSRunsBetweenPolicyAndRecording(t *testing.T) {
	requireGit(t)

	refuse := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REC_LOG\"\ncase \"$2\" in check-push) exit 3 ;; esac\nexit 0\n"
	env, _ := setupEmbeddedHookBody(t, refuse)
	env, log := withStubLFS(t, env, 0)
	work := newRepoWithRemote(t, env)
	markLFSRepo(t, env, work)
	if out, err := runGit(env, work, "push", "-u", "origin", "main"); err == nil {
		t.Fatalf("push succeeded despite a policy refusal:\n%s", out)
	}
	if got := lfsLog(t, log); got != "" {
		t.Errorf("git-lfs uploaded for a push the policy refused: %q", got)
	}

	env, recLog := setupEmbeddedHook(t, 0)
	env, log = withStubLFS(t, env, 0)
	work = newRepoWithRemote(t, env)
	markLFSRepo(t, env, work)
	mustGit(t, env, work, "push", "-u", "origin", "main")
	if got := lfsLog(t, log); !strings.Contains(got, "refs/heads/main ") {
		t.Errorf("git-lfs was not handed the allowed push: %q", got)
	}
	if rec := readMarker(t, recLog); !strings.Contains(rec, "--ref refs/heads/main") {
		t.Errorf("the allowed push was not recorded after the upload: %q", rec)
	}
}
