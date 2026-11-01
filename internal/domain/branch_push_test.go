package domain

import (
	"encoding/json"
	"testing"
)

func TestNewBranchPushAction_Shape(t *testing.T) {
	act, ok := NewBranchPushAction("octo/repo", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchPushAction returned ok=false for a valid branch push")
	}
	if act.Provider != ProviderGitHub || act.Action != ActionBranchPushed {
		t.Errorf("provider/action = %q/%q, want github/branch_pushed", act.Provider, act.Action)
	}
	if act.Target != "octo/repo" || act.ExternalID != "refs/heads/feature/x" || act.ToState != BranchStatePushed {
		t.Errorf("target/external_id/to = %q/%q/%q, want octo/repo, the ref, pushed", act.Target, act.ExternalID, act.ToState)
	}
	if act.ObjectKind != ObjectKindBranch || act.ObjectKey != "refs/heads/feature/x" {
		t.Errorf("object = %q %q, want the branch keyed on its ref", act.ObjectKind, act.ObjectKey)
	}
	// The push alone names no host, repository row, credential or run: the
	// host-side recorder sets them.
	if act.URL != "" || act.RepositoryID != "" || act.DedupKey != "" || act.Credential != "" || act.ConversationID != "" {
		t.Errorf("row = %+v, want no link, repository, key, credential or run until the recorder sets them", act)
	}
	var d branchPushDetails
	if err := json.Unmarshal([]byte(act.DetailJSON), &d); err != nil {
		t.Fatalf("detail_json %q: %v", act.DetailJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
	}
}

// TestBranchPushDedupKey_NamesTheRepository pins that two repositories'
// pushes of one ref and sha in one conversation are two keys, while the same
// push observed twice is one.
func TestBranchPushDedupKey_NamesTheRepository(t *testing.T) {
	a := BranchPushDedupKey("conv", "repo-a", "refs/heads/x", "sha")
	if want := "branch:conv:repo-a:refs/heads/x:sha"; a != want {
		t.Errorf("BranchPushDedupKey = %q, want %q", a, want)
	}
	if b := BranchPushDedupKey("conv", "repo-b", "refs/heads/x", "sha"); a == b {
		t.Errorf("two repositories' pushes share the key %q", a)
	}
	if again := BranchPushDedupKey("conv", "repo-a", "refs/heads/x", "sha"); again != a {
		t.Errorf("one push keyed twice = %q and %q", a, again)
	}
}

// TestDeliverableAction_ReferencesWhatTheDeliverableIsAbout pins the reference
// an approval-lifecycle audit row takes from its deliverable: the entity its
// pull request is linked to.
func TestDeliverableAction_ReferencesWhatTheDeliverableIsAbout(t *testing.T) {
	pr := NewPullRequestDeliverable("octo/api", 3, "PR_3", "fix", "main", "", "t", "b", true)
	pr.EntityID = "entity-3"
	if act := DeliverableAction(&pr, PerformedByPerson, "u", ActionPRClosed, "", "", CredentialGitHubApp); act.EntityID != "entity-3" || act.RepositoryID != "" {
		t.Errorf("pull request action references (%q, %q), want the entity alone", act.EntityID, act.RepositoryID)
	}
}

// TestNewBranchPushAction_NotCreatedOmitsNew pins that created=false drops the
// "new" key entirely (omitempty), so the hook and the git proxy produce
// byte-identical detail_json for the same push.
func TestNewBranchPushAction_NotCreatedOmitsNew(t *testing.T) {
	act, ok := NewBranchPushAction("octo/repo", "refs/heads/main", "deadbeef", false)
	if !ok {
		t.Fatal("ok=false, want a valid push")
	}
	if want := `{"sha":"deadbeef"}`; act.DetailJSON != want {
		t.Errorf("detail_json = %q, want %q", act.DetailJSON, want)
	}
}

func TestNewBranchPushAction_Rejects(t *testing.T) {
	cases := []struct {
		name     string
		repoPath string
		ref      string
	}{
		{"tag ref", "octo/repo", "refs/tags/v1.0"},
		{"bare ref", "octo/repo", "main"},
		{"other namespace", "octo/repo", "refs/pull/1/head"},
		{"single-segment repo", "octo", "refs/heads/main"},
		{"nested repo path", "scm/octo/repo", "refs/heads/main"},
		{"empty repo segment", "octo/", "refs/heads/main"},
		{"trailing slash", "octo/repo/", "refs/heads/main"},
		{"empty repo path", "", "refs/heads/main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := NewBranchPushAction(c.repoPath, c.ref, "sha", false); ok {
				t.Errorf("NewBranchPushAction(%q, %q) ok=true, want false", c.repoPath, c.ref)
			}
		})
	}
}

func TestBranchWebURL_EscapesBranchSegments(t *testing.T) {
	cases := []struct {
		branch string
		want   string
	}{
		{"main", "https://github.com/octo/repo/tree/main"},
		// '/' separators are preserved as path separators.
		{"feature/x", "https://github.com/octo/repo/tree/feature/x"},
		// '#', space, and '%' are escaped so they don't break the link.
		{"feature/#123", "https://github.com/octo/repo/tree/feature/%23123"},
		{"wip/a b", "https://github.com/octo/repo/tree/wip/a%20b"},
		{"odd/100%done", "https://github.com/octo/repo/tree/odd/100%25done"},
	}
	for _, c := range cases {
		got, ok := BranchWebURL("https://github.com", "octo/repo", branchRefPrefix+c.branch)
		if !ok {
			t.Fatalf("BranchWebURL for branch %q: ok=false", c.branch)
		}
		if got != c.want {
			t.Errorf("url for branch %q = %q, want %q", c.branch, got, c.want)
		}
	}
}

// TestBranchWebURL pins the host-aware builder a branch's ledger rows link
// through: a GHES base is honored (with a trailing slash trimmed and segments
// still escaped), an empty base names no GitHub and builds no link, and a
// non-branch ref / malformed repo returns ok=false so no link is recorded.
func TestBranchWebURL(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		repoPath string
		ref      string
		want     string
		wantOK   bool
	}{
		{"ghes host", "https://ghe.example.com", "octo/repo", branchRefPrefix + "feature/x", "https://ghe.example.com/octo/repo/tree/feature/x", true},
		{"ghes trailing slash trimmed", "https://ghe.example.com/", "octo/repo", branchRefPrefix + "main", "https://ghe.example.com/octo/repo/tree/main", true},
		{"ghes escapes segments", "https://ghe.example.com", "octo/repo", branchRefPrefix + "wip/a b", "https://ghe.example.com/octo/repo/tree/wip/a%20b", true},
		{"empty base builds no link", "", "octo/repo", branchRefPrefix + "main", "", true},
		{"non-branch ref rejected", "https://ghe.example.com", "octo/repo", "refs/tags/v1", "", false},
		{"malformed repo rejected", "https://ghe.example.com", "octo", branchRefPrefix + "main", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := BranchWebURL(c.base, c.repoPath, c.ref)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if got != c.want {
				t.Errorf("url = %q, want %q", got, c.want)
			}
		})
	}
}
