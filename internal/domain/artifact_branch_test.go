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
	if want := "https://github.com/octo/repo/tree/feature/x"; a.URL != want {
		t.Errorf("url = %q, want %q", a.URL, want)
	}
	if want := "git:branch:octo/repo:refs/heads/feature/x"; a.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", a.DedupKey, want)
	}
	var d branchDetails
	if err := json.Unmarshal([]byte(a.DetailsJSON), &d); err != nil {
		t.Fatalf("details_json %q: %v", a.DetailsJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
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
		a, ok := NewBranchArtifact("octo/repo", branchRefPrefix+c.branch, "sha", false)
		if !ok {
			t.Fatalf("NewBranchArtifact for branch %q: ok=false", c.branch)
		}
		if a.URL != c.want {
			t.Errorf("url for branch %q = %q, want %q", c.branch, a.URL, c.want)
		}
	}
}
