package domain

import "testing"

func TestNewPullRequestArtifact_DraftShape(t *testing.T) {
	a := NewPullRequestArtifact("octo/repo", 42, "PR_node", "feature/x", "main",
		"https://github.com/octo/repo/pull/42", "Fix the thing", "Body text", true)

	if a.Provider != ArtifactProviderGitHub || a.Kind != ArtifactKindPullRequest {
		t.Errorf("provider/kind = %q/%q", a.Provider, a.Kind)
	}
	if a.Target != "octo/repo#42" {
		t.Errorf("target = %q, want octo/repo#42", a.Target)
	}
	if a.ExternalID != "42" {
		t.Errorf("external_id = %q, want 42", a.ExternalID)
	}
	if a.State != ArtifactStatePRDraft {
		t.Errorf("state = %q, want draft", a.State)
	}
	if a.DedupKey != "github:pull_request:octo/repo#42" {
		t.Errorf("dedup_key = %q", a.DedupKey)
	}

	d, err := ParsePRArtifactDetails(a.DetailsJSON)
	if err != nil {
		t.Fatalf("ParsePRArtifactDetails: %v", err)
	}
	if d.NodeID != "PR_node" || d.HeadBranch != "feature/x" || d.Base != "main" {
		t.Errorf("details coords = %+v", d)
	}
	if d.Proposed.Title != "Fix the thing" || d.Proposed.Body != "Body text" {
		t.Errorf("proposed = %+v, want agent draft", d.Proposed)
	}
	if d.Snapshot != d.Proposed {
		t.Errorf("snapshot %+v should start equal to proposed %+v", d.Snapshot, d.Proposed)
	}
}

func TestNewPullRequestArtifact_NonDraftOpenState(t *testing.T) {
	a := NewPullRequestArtifact("o/r", 1, "n", "h", "main", "url", "t", "b", false)
	if a.State != ArtifactStatePROpen {
		t.Errorf("state = %q, want open", a.State)
	}
}

func TestParsePRArtifactDetails_Empty(t *testing.T) {
	d, err := ParsePRArtifactDetails("")
	if err != nil {
		t.Fatalf("empty details should not error: %v", err)
	}
	if d != (PRArtifactDetails{}) {
		t.Errorf("empty details should be zero value, got %+v", d)
	}
}

func TestParsePRTarget(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		number      int
		ok          bool
	}{
		{"octo/repo#42", "octo", "repo", 42, true},
		{"a/b#1", "a", "b", 1, true},
		{"octo/repo", "", "", 0, false},     // no number
		{"octo/repo#", "", "", 0, false},    // trailing hash
		{"#42", "", "", 0, false},           // no repo
		{"octorepo#42", "", "", 0, false},   // no slash
		{"a/b/c#42", "", "", 0, false},      // nested path
		{"octo/repo#abc", "", "", 0, false}, // non-numeric
		{"octo/repo#0", "", "", 0, false},   // zero number
	}
	for _, c := range cases {
		owner, repo, number, ok := ParsePRTarget(c.in)
		if ok != c.ok || owner != c.owner || repo != c.repo || number != c.number {
			t.Errorf("ParsePRTarget(%q) = (%q,%q,%d,%v), want (%q,%q,%d,%v)",
				c.in, owner, repo, number, ok, c.owner, c.repo, c.number, c.ok)
		}
	}
}
