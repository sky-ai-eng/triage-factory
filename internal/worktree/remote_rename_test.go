package worktree

import (
	"context"
	"path/filepath"
	"testing"
)

// TestStaleRemoteURLs_RewritesTheRepositorysOwnRemotes: a checkout made while
// its repository was acme/app has an origin, and for a pull request from the
// same repository a push remote, naming acme/app. Both move to the new name,
// in either clone-URL form; a fork's push remote and a remote on another host
// stay where they are, and so does every other value of a key.
func TestStaleRemoteURLs_RewritesTheRepositorysOwnRemotes(t *testing.T) {
	dir := ptRepo(t)
	coRun(t, dir, "remote", "add", "origin", "https://github.com/acme/app.git")
	coRun(t, dir, "remote", "add", "tfpush-run-1-7", "git@github.com:acme/app.git")
	coRun(t, dir, "remote", "add", "tfpush-run-1-8", "https://github.com/fork/app.git")
	coRun(t, dir, "remote", "add", "mirror", "https://ghe.example.com/acme/app.git")
	coRun(t, dir, "config", "--add", "remote.origin.pushurl", "https://github.com/acme/app.git")
	coRun(t, dir, "config", "--add", "remote.origin.pushurl", "https://github.com/acme/backup.git")

	renames, err := StaleRemoteURLs(dir, "https://github.com", "acme/app", "acme/app-renamed")
	if err != nil {
		t.Fatalf("StaleRemoteURLs: %v", err)
	}
	if len(renames) != 3 {
		t.Fatalf("renames = %+v, want origin's url and pushurl and the same-repository push remote", renames)
	}
	if err := RewriteRemoteURLs(context.Background(), renames); err != nil {
		t.Fatalf("RewriteRemoteURLs: %v", err)
	}

	for remote, want := range map[string]string{
		"origin":         "https://github.com/acme/app-renamed.git",
		"tfpush-run-1-7": "git@github.com:acme/app-renamed.git",
		"tfpush-run-1-8": "https://github.com/fork/app.git",
		"mirror":         "https://ghe.example.com/acme/app.git",
	} {
		if got := coOut(t, dir, "config", "--get", "remote."+remote+".url"); got != want {
			t.Errorf("remote %s url = %q, want %q", remote, got, want)
		}
	}
	got := coOut(t, dir, "config", "--get-all", "remote.origin.pushurl")
	if want := "https://github.com/acme/app-renamed.git\nhttps://github.com/acme/backup.git"; got != want {
		t.Errorf("origin pushurls = %q, want %q", got, want)
	}

	// Once moved there is nothing left to move.
	if again, err := StaleRemoteURLs(dir, "https://github.com", "acme/app", "acme/app-renamed"); err != nil || len(again) != 0 {
		t.Errorf("StaleRemoteURLs after the rewrite = %+v, %v; want none", again, err)
	}
}

// TestStaleRemoteURLs_LinkedWorktreeReadsTheSharedConfig: a linked worktree's
// remotes live in its bare's config, which is the file the rename is written
// to.
func TestStaleRemoteURLs_LinkedWorktreeReadsTheSharedConfig(t *testing.T) {
	src := ptRepo(t)
	bare := filepath.Join(t.TempDir(), "app.git")
	coRun(t, "", "clone", "-q", "--bare", src, bare)
	coRun(t, bare, "remote", "set-url", "origin", "https://github.com/acme/app.git")
	wt := filepath.Join(t.TempDir(), "acme", "app", "default")
	coRun(t, bare, "worktree", "add", "-q", "--detach", wt)

	renames, err := StaleRemoteURLs(wt, "https://github.com", "acme/app", "acme/app-renamed")
	if err != nil {
		t.Fatalf("StaleRemoteURLs: %v", err)
	}
	if len(renames) != 1 || renames[0].ConfigPath != filepath.Join(bare, "config") {
		t.Fatalf("renames = %+v, want origin's url in %s", renames, filepath.Join(bare, "config"))
	}
	if err := RewriteRemoteURLs(context.Background(), renames); err != nil {
		t.Fatalf("RewriteRemoteURLs: %v", err)
	}
	if got := coOut(t, wt, "remote", "get-url", "origin"); got != "https://github.com/acme/app-renamed.git" {
		t.Errorf("origin = %q, want the new name", got)
	}
}

// TestStaleRemoteURLs_NoRemotes: a checkout with no remote has nothing to move.
func TestStaleRemoteURLs_NoRemotes(t *testing.T) {
	renames, err := StaleRemoteURLs(ptRepo(t), "https://github.com", "acme/app", "acme/app-renamed")
	if err != nil || len(renames) != 0 {
		t.Errorf("StaleRemoteURLs = %+v, %v; want none", renames, err)
	}
}
