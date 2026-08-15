package domain

import (
	"reflect"
	"testing"
)

func TestDetectRepoRenames(t *testing.T) {
	stored := []RepoRef{
		{Source: "github", Owner: "octo", Repo: "api", ExternalID: "1"},
		{Source: "github", Owner: "octo", Repo: "web", ExternalID: "2"},
		{Source: "github", Owner: "octo", Repo: "no-id"},
	}

	tests := []struct {
		name     string
		observed []RepoRef
		want     []string // slugs
	}{
		{
			name:     "same id under a different name is a rename",
			observed: []RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}},
			want:     []string{"octo/platform-api"},
		},
		{
			name: "a transfer to another owner is the same condition",
			// GitHub keeps the id across a transfer, so the owner half moving
			// is a rename by exactly the same rule.
			observed: []RepoRef{{Owner: "acme", Repo: "api", ExternalID: "1"}},
			want:     []string{"acme/api"},
		},
		{
			name:     "the same name under a different id is NOT a rename",
			observed: []RepoRef{{Owner: "octo", Repo: "api", ExternalID: "99"}},
			want:     nil,
		},
		{
			name:     "an observation with no id is never a rename",
			observed: []RepoRef{{Owner: "octo", Repo: "renamed"}},
			want:     nil,
		},
		{
			name:     "a stored row with no id is never renamed",
			observed: []RepoRef{{Owner: "octo", Repo: "still-no-id", ExternalID: "3"}},
			want:     nil,
		},
		{
			name:     "casing alone is not a rename",
			observed: []RepoRef{{Owner: "Octo", Repo: "API", ExternalID: "1"}},
			want:     nil,
		},
		{
			name:     "the steady state reports nothing",
			observed: []RepoRef{{Owner: "octo", Repo: "api", ExternalID: "1"}, {Owner: "octo", Repo: "web", ExternalID: "2"}},
			want:     nil,
		},
		{
			name:     "an unknown source contributes nothing rather than erroring",
			observed: []RepoRef{{Source: "gitlob", Owner: "octo", Repo: "renamed", ExternalID: "1"}},
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range DetectRepoRenames(stored, tc.observed) {
				got = append(got, r.Slug())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DetectRepoRenames = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRewriteRepoSlugPrefix(t *testing.T) {
	const from, to = "octo/api", "octo/platform-api"
	tests := []struct {
		in      string
		want    string
		wantOK  bool
		because string
	}{
		{in: "octo/api", want: "octo/platform-api", wantOK: true, because: "the bare slug"},
		{in: "octo/api#18", want: "octo/platform-api#18", wantOK: true, because: "a PR composite"},
		{in: "Octo/API#18", want: "octo/platform-api#18", wantOK: true, because: "matching is case-insensitive"},
		{in: "octo/api:refs/heads/x", want: "octo/platform-api:refs/heads/x", wantOK: true, because: "a dedup resource with an anchor"},
		{in: "octo/api-gateway#4", want: "octo/api-gateway#4", because: "a longer name that merely starts the same"},
		{in: "octo/ap", want: "octo/ap", because: "shorter than the slug"},
		{in: "acme/octo/api#1", want: "acme/octo/api#1", because: "the slug is not at the head"},
		{in: "", want: "", because: "nothing to rewrite"},
	}
	for _, tc := range tests {
		t.Run(tc.because, func(t *testing.T) {
			got, ok := RewriteRepoSlugPrefix(tc.in, from, to)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("RewriteRepoSlugPrefix(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRewriteArtifactDedupKey(t *testing.T) {
	const from, to = "octo/api", "octo/platform-api"
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "github:pull_request:octo/api#18", want: "github:pull_request:octo/platform-api#18", wantOK: true},
		{in: "github:review:octo/api#18:conv-1", want: "github:review:octo/platform-api#18:conv-1", wantOK: true},
		// The anchor repeats the repository's own name. A substring rewrite
		// would move the branch too, renaming a ref that never moved.
		{in: "git:branch:octo/api:refs/heads/octo/api-fix", want: "git:branch:octo/platform-api:refs/heads/octo/api-fix", wantOK: true},
		{in: "github:pull_request:octo/api-gateway#4", want: "github:pull_request:octo/api-gateway#4"},
		{in: "jira:issue:PROJ-123", want: "jira:issue:PROJ-123"},
		{in: "github:comment:5551212", want: "github:comment:5551212"},
		{in: "github:pull_request", want: "github:pull_request"},
		{in: "malformed", want: "malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := RewriteArtifactDedupKey(tc.in, from, to)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("RewriteArtifactDedupKey(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRewritePinnedRepos(t *testing.T) {
	const from, to = "octo/api", "octo/platform-api"

	got, changed := RewritePinnedRepos([]string{"acme/tools", "Octo/API", "octo/api-gateway"}, from, to)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if want := []string{"acme/tools", to, "octo/api-gateway"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rewrite = %v, want %v — order is preserved and the neighbour is left alone", got, want)
	}

	if _, changed := RewritePinnedRepos([]string{"acme/tools"}, from, to); changed {
		t.Error("changed = true for a list with nothing pinned under the old slug")
	}
	if _, changed := RewritePinnedRepos(nil, from, to); changed {
		t.Error("changed = true for an empty list")
	}

	// A project that already pins the target absorbs the moved pin rather than
	// listing one repository twice.
	got, changed = RewritePinnedRepos([]string{to, from}, from, to)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if want := []string{to}; !reflect.DeepEqual(got, want) {
		t.Errorf("rewrite = %v, want %v", got, want)
	}
}
