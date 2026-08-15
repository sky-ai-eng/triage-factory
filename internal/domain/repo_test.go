package domain

import "testing"

// repo_profiles.source carries no CHECK constraint in either dialect —
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
