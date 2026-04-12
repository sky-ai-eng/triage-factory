package gh

import (
	"os"
	"testing"
)

func TestParseGitRemoteURL(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		// HTTPS
		{"https with .git", "https://github.com/sky-ai-eng/todo-triage.git", "sky-ai-eng", "todo-triage", true},
		{"https without .git", "https://github.com/sky-ai-eng/todo-triage", "sky-ai-eng", "todo-triage", true},
		{"https with trailing slash stripped", "https://github.com/octo/repo.git", "octo", "repo", true},
		{"http enterprise host", "http://github.example.com/team/proj.git", "team", "proj", true},

		// SCP-style SSH (git@host:path)
		{"scp ssh with .git", "git@github.com:sky-ai-eng/todo-triage.git", "sky-ai-eng", "todo-triage", true},
		{"scp ssh without .git", "git@github.com:octo/repo", "octo", "repo", true},
		{"scp ssh ghe host", "git@github.example.com:team/proj.git", "team", "proj", true},

		// URL-style SSH
		{"ssh:// with .git", "ssh://git@github.com/sky-ai-eng/todo-triage.git", "sky-ai-eng", "todo-triage", true},
		{"ssh:// without .git", "ssh://git@github.com/octo/repo", "octo", "repo", true},

		// git://
		{"git:// with .git", "git://github.com/octo/repo.git", "octo", "repo", true},

		// Tolerated edge cases
		{"trailing slash", "https://github.com/octo/repo/", "octo", "repo", true},
		{"trailing slash after .git", "https://github.com/octo/repo.git/", "octo", "repo", true},
		{"scp with trailing slash", "git@github.com:octo/repo.git/", "octo", "repo", true},

		// Multi-segment path rejection (regression guard for silent
		// mis-resolution on Bitbucket / nested GitLab / custom layouts).
		// These are rejected rather than guessed because "first two" and
		// "last two" are both wrong in different environments, and
		// silent wrong-target is worse than a hard error that prompts
		// --repo.
		{"bitbucket scm layout rejected", "https://bitbucket.example.com/scm/project/repo.git", "", "", false},
		{"gitlab nested groups rejected", "https://gitlab.com/group/subgroup/repo.git", "", "", false},
		{"deep gitlab nesting rejected", "https://gitlab.com/a/b/c/d/repo.git", "", "", false},
		{"scp bitbucket layout rejected", "git@bitbucket.org:scm/project/repo.git", "", "", false},

		// Failures
		{"empty string", "", "", "", false},
		{"no path", "https://github.com", "", "", false},
		{"only owner", "https://github.com/octo", "", "", false},
		{"scp no colon", "git@github.com", "", "", false},
		{"unknown scheme", "ftp://github.com/octo/repo", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOwner, gotRepo, gotOK := parseGitRemoteURL(tc.url)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (owner=%q repo=%q)", gotOK, tc.wantOK, gotOwner, gotRepo)
			}
			if tc.wantOK {
				if gotOwner != tc.wantOwner || gotRepo != tc.wantRepo {
					t.Errorf("got (%q, %q), want (%q, %q)", gotOwner, gotRepo, tc.wantOwner, tc.wantRepo)
				}
			}
		})
	}
}

func TestSplitOwnerRepoStr(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
		owner   string
		repo    string
	}{
		{"valid", "sky-ai-eng/todo-triage", false, "sky-ai-eng", "todo-triage"},
		{"valid with dashes", "my-org/my-repo", false, "my-org", "my-repo"},
		{"empty", "", true, "", ""},
		{"no slash", "owner", true, "", ""},
		{"trailing slash", "owner/", true, "", ""},
		{"leading slash", "/repo", true, "", ""},
		{"only slash", "/", true, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := splitOwnerRepoStr(tc.value, "test source")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if owner != tc.owner || repo != tc.repo {
					t.Errorf("got (%q, %q), want (%q, %q)", owner, repo, tc.owner, tc.repo)
				}
			}
		})
	}
}

// TestResolveRepo_FlagWins verifies the explicit flag beats the env var.
// Uses t.Setenv so the env state is scoped to this test and auto-restored.
func TestResolveRepo_FlagWins(t *testing.T) {
	t.Setenv("TODOTRIAGE_REPO", "env-owner/env-repo")

	owner, repo, err := resolveRepo("flag-owner/flag-repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "flag-owner" || repo != "flag-repo" {
		t.Errorf("got (%q, %q), want (flag-owner, flag-repo)", owner, repo)
	}
}

func TestResolveRepo_EnvWhenNoFlag(t *testing.T) {
	t.Setenv("TODOTRIAGE_REPO", "env-owner/env-repo")

	owner, repo, err := resolveRepo("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "env-owner" || repo != "env-repo" {
		t.Errorf("got (%q, %q), want (env-owner, env-repo)", owner, repo)
	}
}

// TestResolveRepo_HardErrorWhenNothingResolves runs from a temp directory
// with no env var and no git checkout, so every resolution path fails and
// the resolver returns a clear error.
func TestResolveRepo_HardErrorWhenNothingResolves(t *testing.T) {
	t.Setenv("TODOTRIAGE_REPO", "")

	tmp := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	_, _, err = resolveRepo("")
	if err == nil {
		t.Fatal("expected error when no resolution path succeeds, got nil")
	}
}

// TestResolveRepo_InvalidFlagFormat — a malformed --repo value should
// error, not fall through to env/git/hardcoded.
func TestResolveRepo_InvalidFlagFormat(t *testing.T) {
	t.Setenv("TODOTRIAGE_REPO", "env-owner/env-repo")

	_, _, err := resolveRepo("not-a-valid-format")
	if err == nil {
		t.Fatal("expected error on invalid flag value, got nil")
	}
}
