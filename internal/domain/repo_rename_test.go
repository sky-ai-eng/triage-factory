package domain

import (
	"reflect"
	"testing"
)

func TestDetectRepoRenames(t *testing.T) {
	const gh, ghe = "https://github.com", "https://ghe.example.com"
	stored := []RepoRef{
		{Source: "github", Host: gh, Owner: "octo", Repo: "api", ExternalID: "1"},
		{Source: "github", Host: gh, Owner: "octo", Repo: "web", ExternalID: "2"},
		{Source: "github", Host: gh, Owner: "octo", Repo: "no-id"},
	}

	tests := []struct {
		name     string
		observed []RepoRef
		want     []string // slugs
	}{
		{
			name:     "same id under a different name is a rename",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "platform-api", ExternalID: "1"}},
			want:     []string{"octo/platform-api"},
		},
		{
			name: "a transfer to another owner is the same condition",
			// GitHub keeps the id across a transfer, so the owner half moving
			// is a rename by exactly the same rule.
			observed: []RepoRef{{Host: gh, Owner: "acme", Repo: "api", ExternalID: "1"}},
			want:     []string{"acme/api"},
		},
		{
			name:     "the same name under a different id is NOT a rename",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "api", ExternalID: "99"}},
			want:     nil,
		},
		{
			name:     "an observation with no id is never a rename",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "renamed"}},
			want:     nil,
		},
		{
			name:     "a stored row with no id is never renamed",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "still-no-id", ExternalID: "3"}},
			want:     nil,
		},
		{
			name:     "casing alone is not a rename",
			observed: []RepoRef{{Host: gh, Owner: "Octo", Repo: "API", ExternalID: "1"}},
			want:     nil,
		},
		{
			name:     "the steady state reports nothing",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "api", ExternalID: "1"}, {Host: gh, Owner: "octo", Repo: "web", ExternalID: "2"}},
			want:     nil,
		},
		{
			// Repository ids are per-deployment sequences, so another host's
			// repository carrying the same id is a different repository.
			name:     "the same id on another host is NOT a rename",
			observed: []RepoRef{{Host: ghe, Owner: "acme", Repo: "other", ExternalID: "1"}},
			want:     nil,
		},
		{
			name:     "an observation with no host matches nothing",
			observed: []RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}},
			want:     nil,
		},
		{
			name:     "an unknown source contributes nothing rather than erroring",
			observed: []RepoRef{{Source: "gitlob", Host: gh, Owner: "octo", Repo: "renamed", ExternalID: "1"}},
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

func TestRewriteRepoURL(t *testing.T) {
	const from, to = "octo/api", "octo/platform-api"
	const dotcom, ghe = "https://github.com", "https://ghe.acme.dev"
	const ctxHost = "https://example.com/github"
	tests := []struct {
		in      string
		host    string
		want    string
		wantOK  bool
		because string
	}{
		{in: "https://github.com/octo/api/pull/18", host: dotcom, want: "https://github.com/octo/platform-api/pull/18", wantOK: true, because: "a PR link"},
		{in: "https://github.com/Octo/API", host: dotcom, want: "https://github.com/octo/platform-api", wantOK: true, because: "the repo home, matched case-insensitively"},
		{in: "https://ghe.acme.dev/octo/api/pull/2?diff=split#discussion_r1", host: ghe, want: "https://ghe.acme.dev/octo/platform-api/pull/2?diff=split#discussion_r1", wantOK: true, because: "an enterprise host, query and fragment kept"},
		// A branch named after the repository repeats the slug later in the
		// path. Only the segments right after the host are the repository;
		// the branch is a ref that never moved.
		{in: "https://github.com/octo/api/tree/octo/api", host: dotcom, want: "https://github.com/octo/platform-api/tree/octo/api", wantOK: true, because: "a later segment repeating the slug stays put"},
		{in: "https://github.com/octo/api-gateway/pull/4", host: dotcom, want: "https://github.com/octo/api-gateway/pull/4", because: "a longer name that merely starts the same"},
		{in: "https://octo.api/somewhere", host: dotcom, want: "https://octo.api/somewhere", because: "a host spelling the slug is not the slug"},
		{in: "https://github.com/acme/octo/api", host: dotcom, want: "https://github.com/acme/octo/api", because: "the slug is not right after the host"},
		// A context path is part of the host: the slug is read after it.
		{in: "https://example.com/github/octo/api/pull/1", host: ctxHost, want: "https://example.com/github/octo/platform-api/pull/1", wantOK: true, because: "a link under a context path"},
		{in: "https://EXAMPLE.com/github/octo/api", host: ctxHost, want: "https://EXAMPLE.com/github/octo/platform-api", wantOK: true, because: "the authority compares case-insensitively and is kept as linked"},
		{in: "https://example.com/github/octo/api", host: ctxHost + "/", want: "https://example.com/github/octo/platform-api", wantOK: true, because: "a host with a trailing slash"},
		{in: "https://example.com/GitHub/octo/api/pull/1", host: ctxHost, want: "https://example.com/GitHub/octo/api/pull/1", because: "a context path in another case is another host"},
		{in: "https://example.com/gitlab/octo/api/pull/1", host: ctxHost, want: "https://example.com/gitlab/octo/api/pull/1", because: "another context path on the same authority"},
		{in: "https://example.com/octo/api/pull/1", host: ctxHost, want: "https://example.com/octo/api/pull/1", because: "the authority without the context path"},
		{in: "https://example.com/github/octo/api/pull/1", host: "https://example.com", want: "https://example.com/github/octo/api/pull/1", because: "a host without the context path reads the path's head, which is not the slug"},
		// A link on another host is another host's object, whatever it spells.
		{in: "https://ghe.acme.dev/octo/api/pull/2", host: dotcom, want: "https://ghe.acme.dev/octo/api/pull/2", because: "a link on another host"},
		{in: "http://github.com/octo/api", host: dotcom, want: "http://github.com/octo/api", because: "another scheme is another host"},
		{in: "https://github.com/octo/api", host: "", want: "https://github.com/octo/api", because: "no host is no match, never a default"},
		{in: "octo/api#18", host: dotcom, want: "octo/api#18", because: "a composite key is not a URL"},
		{in: "", host: dotcom, want: "", because: "a URL TF never learned stays empty"},
	}
	for _, tc := range tests {
		t.Run(tc.because, func(t *testing.T) {
			got, ok := RewriteRepoURL(tc.in, tc.host, from, to)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("RewriteRepoURL(%q, %q) = (%q, %v), want (%q, %v)", tc.in, tc.host, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRewriteRepoCloneURL(t *testing.T) {
	const from, to = "octo/api", "octo/platform-api"
	const dotcom, ghe = "https://github.com", "https://ghe.acme.dev:8443"
	const ctxHost = "https://example.com/github"
	tests := []struct {
		in      string
		host    string
		want    string
		wantOK  bool
		because string
	}{
		{in: "https://github.com/octo/api.git", host: dotcom, want: "https://github.com/octo/platform-api.git", wantOK: true, because: "an HTTPS clone URL"},
		{in: "https://github.com/Octo/API", host: dotcom, want: "https://github.com/octo/platform-api", wantOK: true, because: "an HTTPS clone URL without .git, matched case-insensitively"},
		{in: "https://example.com/github/octo/api.git", host: ctxHost, want: "https://example.com/github/octo/platform-api.git", wantOK: true, because: "an HTTPS clone URL under a context path"},
		{in: "git@github.com:octo/api.git", host: dotcom, want: "git@github.com:octo/platform-api.git", wantOK: true, because: "an SSH clone URL"},
		{in: "git@GitHub.com:octo/api", host: dotcom, want: "git@GitHub.com:octo/platform-api", wantOK: true, because: "an SSH clone URL without .git, its hostname matched case-insensitively"},
		{in: "git@ghe.acme.dev:octo/api.git", host: ghe, want: "git@ghe.acme.dev:octo/platform-api.git", wantOK: true, because: "an SSH clone URL on an enterprise host with a web port"},
		{in: "ssh://git@ghe.acme.dev:2222/octo/api.git", host: ghe, want: "ssh://git@ghe.acme.dev:2222/octo/platform-api.git", wantOK: true, because: "an ssh:// clone URL with its own port"},
		{in: "https://github.com/octo/api-gateway.git", host: dotcom, want: "https://github.com/octo/api-gateway.git", because: "a longer name that merely starts the same"},
		{in: "https://github.com/octo/api/tree/main", host: dotcom, want: "https://github.com/octo/api/tree/main", because: "a path past the slug is not a clone URL"},
		{in: "git@github.com:octo/api-gateway.git", host: dotcom, want: "git@github.com:octo/api-gateway.git", because: "an SSH URL for another repository"},
		{in: "https://ghe.acme.dev/octo/api.git", host: dotcom, want: "https://ghe.acme.dev/octo/api.git", because: "an HTTPS URL on another host"},
		{in: "git@ghe.acme.dev:octo/api.git", host: dotcom, want: "git@ghe.acme.dev:octo/api.git", because: "an SSH URL on another host"},
		{in: "http://github.com/octo/api.git", host: dotcom, want: "http://github.com/octo/api.git", because: "another scheme is another host"},
		{in: "https://github.com/octo/api.git", host: "", want: "https://github.com/octo/api.git", because: "no host is no match"},
		{in: "", host: dotcom, want: "", because: "a URL TF never learned stays empty"},
	}
	for _, tc := range tests {
		t.Run(tc.because, func(t *testing.T) {
			got, ok := RewriteRepoCloneURL(tc.in, tc.host, from, to)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("RewriteRepoCloneURL(%q, %q) = (%q, %v), want (%q, %v)", tc.in, tc.host, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
