package domain

import (
	"encoding/json"
	"testing"
)

func TestNewBranchArtifact_Shape(t *testing.T) {
	a, ok := NewBranchArtifact("octo/repo", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false for a valid branch push")
	}
	if a.Provider != ArtifactProviderGit || a.Kind != ArtifactKindBranch {
		t.Errorf("provider/kind = %q/%q, want git/branch", a.Provider, a.Kind)
	}
	if a.Target != "octo/repo" {
		t.Errorf("target = %q, want octo/repo", a.Target)
	}
	if a.ExternalID != "refs/heads/feature/x" {
		t.Errorf("external_id = %q, want refs/heads/feature/x", a.ExternalID)
	}
	if a.State != ArtifactStateBranchPushed {
		t.Errorf("state = %q, want pushed", a.State)
	}
	// The constructor has no org context, so no host to link on: the
	// host-side sink sets the URL (BranchArtifactWebURL).
	if a.URL != "" {
		t.Errorf("url = %q, want none until the sink anchors it to the org's host", a.URL)
	}
	// Nor the registry row: the sink keys it (BranchDedupKey).
	if a.DedupKey != "" {
		t.Errorf("dedup_key = %q, want none until the sink keys it on the registry row", a.DedupKey)
	}
	var d branchDetails
	if err := json.Unmarshal([]byte(a.DetailsJSON), &d); err != nil {
		t.Fatalf("details_json %q: %v", a.DetailsJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
	}
}

func TestBranchDedupKey(t *testing.T) {
	if got, want := BranchDedupKey("repo-uuid", "refs/heads/feature/x", "conv-1"), "git:branch:repo-uuid:refs/heads/feature/x:conv-1"; got != want {
		t.Errorf("BranchDedupKey = %q, want %q", got, want)
	}
	if got, want := BranchDedupKey("repo-uuid", "refs/heads/feature/x", ""), "git:branch:repo-uuid:refs/heads/feature/x"; got != want {
		t.Errorf("BranchDedupKey with no conversation = %q, want %q", got, want)
	}
	if got := BranchDedupKey("", "refs/heads/x", ""); got != "" {
		t.Errorf("BranchDedupKey with no repository = %q, want empty", got)
	}
	if got := BranchDedupKey("repo-uuid", "", ""); got != "" {
		t.Errorf("BranchDedupKey with no ref = %q, want empty", got)
	}
}

// A branch's registry row comes back out of its key exactly as BranchDedupKey
// wrote it, since both observers of a push key its ledger row on it; nothing
// that is not a branch keyed on a registry row names one.
func TestBranchArtifactRepositoryID(t *testing.T) {
	keyed, _ := NewBranchArtifact("octo/api", "refs/heads/feature/x", "abc", true)
	keyed.DedupKey = BranchDedupKey("repo-uuid", keyed.ExternalID, "")
	unkeyed, _ := NewBranchArtifact("octo/api", "refs/heads/feature/x", "abc", true)
	byName := unkeyed
	byName.DedupKey = "git:branch:octo/api:refs/heads/feature/x"
	pr := NewPullRequestArtifact("octo/api", 3, "PR_3", "fix", "main", "", "t", "b", true)
	for name, c := range map[string]struct {
		a    Artifact
		want string
	}{
		"keyed branch":               {keyed, "repo-uuid"},
		"unkeyed branch":             {unkeyed, ""},
		"branch keyed on owner/repo": {byName, ""},
		"pull request":               {pr, ""},
	} {
		if got, ok := BranchArtifactRepositoryID(c.a); got != c.want || ok != (c.want != "") {
			t.Errorf("%s: BranchArtifactRepositoryID = (%q, %v), want %q", name, got, ok, c.want)
		}
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

// TestArtifactAction_ReferencesWhatTheArtifactIsAbout pins the references an
// approval-lifecycle audit row takes from its artifact: the entity a pull
// request or review artifact is linked to, and the registry row a branch's key
// names.
func TestArtifactAction_ReferencesWhatTheArtifactIsAbout(t *testing.T) {
	pr := NewPullRequestArtifact("octo/api", 3, "PR_3", "fix", "main", "", "t", "b", true)
	pr.EntityID = "entity-3"
	if act := ArtifactAction(&pr, "u", ActionPRClosed, "", "", CredentialGitHubApp); act.EntityID != "entity-3" || act.RepositoryID != "" {
		t.Errorf("pull request action references (%q, %q), want the entity alone", act.EntityID, act.RepositoryID)
	}
	branch, _ := NewBranchArtifact("octo/api", "refs/heads/fix", "abc", false)
	branch.DedupKey = BranchDedupKey("repo-uuid", branch.ExternalID, "")
	if act := ArtifactAction(&branch, "u", ActionBranchDeleted, "", "", CredentialGitHubApp); act.EntityID != "" || act.RepositoryID != "repo-uuid" {
		t.Errorf("branch action references (%q, %q), want the registry row alone", act.EntityID, act.RepositoryID)
	}
}

// TestNewBranchArtifact_NotCreatedOmitsNew pins that created=false drops the
// "new" key entirely (omitempty), so the hook and the proxy backstop produce
// byte-identical details_json for the same push and never flap the row.
func TestNewBranchArtifact_NotCreatedOmitsNew(t *testing.T) {
	a, ok := NewBranchArtifact("octo/repo", "refs/heads/main", "deadbeef", false)
	if !ok {
		t.Fatal("ok=false, want a valid artifact")
	}
	if want := `{"sha":"deadbeef"}`; a.DetailsJSON != want {
		t.Errorf("details_json = %q, want %q", a.DetailsJSON, want)
	}
}

func TestNewBranchArtifact_Rejects(t *testing.T) {
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
			if _, ok := NewBranchArtifact(c.repoPath, c.ref, "sha", false); ok {
				t.Errorf("NewBranchArtifact(%q, %q) ok=true, want false", c.repoPath, c.ref)
			}
		})
	}
}

func TestNewBranchArtifact_URLEscapesBranchSegments(t *testing.T) {
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
		got, ok := BranchArtifactWebURL("https://github.com", "octo/repo", branchRefPrefix+c.branch)
		if !ok {
			t.Fatalf("BranchArtifactWebURL for branch %q: ok=false", c.branch)
		}
		if got != c.want {
			t.Errorf("url for branch %q = %q, want %q", c.branch, got, c.want)
		}
	}
}

// TestBranchArtifactWebURL pins the host-aware builder the artifact sink uses to
// re-anchor a branch link to the org's own GitHub host: a GHES base is honored
// (with a trailing slash trimmed and segments still escaped), an empty base
// names no GitHub and builds no link, and a non-branch ref / malformed repo
// returns ok=false so the sink leaves the existing URL untouched.
func TestBranchArtifactWebURL(t *testing.T) {
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
			got, ok := BranchArtifactWebURL(c.base, c.repoPath, c.ref)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if got != c.want {
				t.Errorf("url = %q, want %q", got, c.want)
			}
		})
	}
}
