package domain

import "testing"

// repositories.source carries no CHECK constraint in either dialect —
// widening a SQLite CHECK costs a full-table rebuild, so the value set is the
// app's to enforce and NormalizeRepoSource is where it is enforced. If this
// function accepts anything, the column does.
func TestNormalizeRepoSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		// Every caller that names no source means GitHub — it is the only
		// provider TF reads repositories from, so an unset source is a
		// default rather than a missing value.
		{name: "empty defaults to github", in: "", want: RepoSourceGitHub, wantOK: true},
		{name: "blank defaults to github", in: "   ", want: RepoSourceGitHub, wantOK: true},
		{name: "github passes through", in: "github", want: RepoSourceGitHub, wantOK: true},
		{name: "casing folds", in: "GitHub", want: RepoSourceGitHub, wantOK: true},
		{name: "surrounding space is trimmed", in: " github ", want: RepoSourceGitHub, wantOK: true},
		// A typo must not become a row: a repository keyed under a provider
		// nothing resolves is invisible to every reader that names a source.
		{name: "typo is refused", in: "gitlob", wantOK: false},
		// Not yet supported is still refused — the schema has room for it,
		// which is not the same as the app having a client for it.
		{name: "unsupported provider is refused", in: "gitlab", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRepoSource(tc.in)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("NormalizeRepoSource(%q) errored: %v", tc.in, err)
				}
				if got != tc.want {
					t.Errorf("NormalizeRepoSource(%q) = %q, want %q", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("NormalizeRepoSource(%q) = %q, want an error", tc.in, got)
			}
			if got != "" {
				t.Errorf("a refused source returned %q; want the empty string so a caller ignoring err cannot write it", got)
			}
		})
	}
}

func TestRepoRefSlug(t *testing.T) {
	if got := (RepoRef{Owner: "octo", Repo: "widget"}).Slug(); got != "octo/widget" {
		t.Errorf("Slug() = %q, want octo/widget", got)
	}
}

// A repository's handle and its display name are two different values, and the
// point of the split is that one of them can change while the other cannot.
// Slug() renders from the columns a rename moves; ID is the row.
func TestRepositorySlugRendersFromTheNameColumns(t *testing.T) {
	r := Repository{ID: "6f1d5f1e-0b3a-4a1e-9f3c-2b7a5d4c8e90", Owner: "octo", Repo: "widget"}
	if got := r.Slug(); got != "octo/widget" {
		t.Errorf("Slug() = %q, want octo/widget", got)
	}
	if r.Slug() == r.ID {
		t.Error("Slug() returned the handle; the display name is built from owner/repo")
	}

	// The rename: the name moves, the handle does not.
	renamed := r
	renamed.Repo = "gadget"
	if got := renamed.Slug(); got != "octo/gadget" {
		t.Errorf("Slug() after a rename = %q, want octo/gadget", got)
	}
	if renamed.ID != r.ID {
		t.Errorf("ID moved with the name: %q → %q", r.ID, renamed.ID)
	}
}

// Ref() is what a caller holding a row hands to a *ByRef* lookup, so it has to
// carry the provider identity — not just the two name columns.
func TestRepositoryRefCarriesProviderIdentity(t *testing.T) {
	r := Repository{
		ID: "6f1d5f1e-0b3a-4a1e-9f3c-2b7a5d4c8e90", Owner: "octo", Repo: "widget",
		Source: RepoSourceGitHub, ExternalID: "1296269",
	}
	want := RepoRef{Source: RepoSourceGitHub, Owner: "octo", Repo: "widget", ExternalID: "1296269"}
	if got := r.Ref(); got != want {
		t.Errorf("Ref() = %+v, want %+v", got, want)
	}
	if got := r.Ref().Slug(); got != r.Slug() {
		t.Errorf("Ref().Slug() = %q, want it to agree with Repository.Slug() %q", got, r.Slug())
	}
}

// TestSplitGitHubEntitySourceID covers the parse two authorization gates read
// a task's repo through, including the ordering that makes it one helper: the
// "#N" suffix is cut before the owner/repo split, so the repo never comes back
// named "repo#42".
func TestSplitGitHubEntitySourceID(t *testing.T) {
	cases := []struct {
		sourceID string
		owner    string
		repo     string
		prNumber int
	}{
		{"octo/repo#42", "octo", "repo", 42},
		{"octo/repo", "octo", "repo", 0},
		{"octo/repo#notanumber", "octo", "repo", 0},
		{"SKY-123", "", "", 0},
		{"", "", "", 0},
		// A Slack entity's source id splits on "/" perfectly well, which is why
		// the task-level helper below gates on the source rather than on this.
		{"C1/1700000000.000100", "C1", "1700000000.000100", 0},
	}
	for _, c := range cases {
		owner, repo, pr := SplitGitHubEntitySourceID(c.sourceID)
		if owner != c.owner || repo != c.repo || pr != c.prNumber {
			t.Errorf("SplitGitHubEntitySourceID(%q) = (%q, %q, %d), want (%q, %q, %d)",
				c.sourceID, owner, repo, pr, c.owner, c.repo, c.prNumber)
		}
	}
}

// TestGitHubTaskRepo pins the source gate. A Slack task's source id parses as a
// plausible owner/repo, so a caller that skipped the check would authorize a
// run against a repository that does not exist.
func TestGitHubTaskRepo(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want string
	}{
		{"github pr task", Task{EntitySource: "github", EntitySourceID: "octo/repo#42"}, "octo/repo"},
		{"github repo task", Task{EntitySource: "github", EntitySourceID: "octo/repo"}, "octo/repo"},
		{"slack task", Task{EntitySource: "slack", EntitySourceID: "C1/1700000000.000100"}, ""},
		{"jira task", Task{EntitySource: "jira", EntitySourceID: "SKY-123"}, ""},
		{"malformed github id", Task{EntitySource: "github", EntitySourceID: "no-slash"}, ""},
	}
	for _, c := range cases {
		if got := GitHubTaskRepo(c.task); got != c.want {
			t.Errorf("%s: GitHubTaskRepo = %q, want %q", c.name, got, c.want)
		}
	}
}
