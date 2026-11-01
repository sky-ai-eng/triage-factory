package domain

import (
	"reflect"
	"testing"
)

func TestDetectRepoRenames(t *testing.T) {
	const gh, ghe = "https://github.com", "https://ghe.example.com"
	stored := []RepoIdentity{
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "api", ExternalID: "1"}},
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "web", ExternalID: "2"}},
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "no-id"}},
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "gone", ExternalID: "4"}, Retired: true},
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
		{
			// A retired row holds no name, so the provider answering for its
			// id under the very slug it is stored under still gives it one.
			name:     "a retired row observed under its stored name is a candidate",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "gone", ExternalID: "4"}},
			want:     []string{"octo/gone"},
		},
		{
			name:     "a retired row observed under another name is a candidate",
			observed: []RepoRef{{Host: gh, Owner: "acme", Repo: "found", ExternalID: "4"}},
			want:     []string{"acme/found"},
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

func TestDetectReassignedRepoNames(t *testing.T) {
	const gh, ghe = "https://github.com", "https://ghe.example.com"
	stored := []RepoIdentity{
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "api", ExternalID: "1"}},
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "web", ExternalID: "2"}},
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "no-id"}},
		{RepoRef: RepoRef{Source: "github", Host: gh, Owner: "octo", Repo: "gone", ExternalID: "4"}, Retired: true},
	}

	tests := []struct {
		name     string
		observed []RepoRef
		want     []string // stored ids
	}{
		{
			// Deleted and recreated, or renamed away and the name reused: the
			// row holding the name is the one to resolve.
			name:     "the same name under a different id reports the stored row",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "api", ExternalID: "99"}},
			want:     []string{"1"},
		},
		{
			name:     "matching folds case",
			observed: []RepoRef{{Host: gh, Owner: "Octo", Repo: "API", ExternalID: "99"}},
			want:     []string{"1"},
		},
		{
			name:     "the same name under the same id is the steady state",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "api", ExternalID: "1"}},
			want:     nil,
		},
		{
			// A swap: the observation already says where id 1 went, and the
			// rename half acts on it.
			name: "a stored row whose own id is observed is left to the rename",
			observed: []RepoRef{
				{Host: gh, Owner: "octo", Repo: "api", ExternalID: "2"},
				{Host: gh, Owner: "octo", Repo: "web", ExternalID: "1"},
			},
			want: nil,
		},
		{
			name:     "a retired row holds no name to reassign",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "gone", ExternalID: "99"}},
			want:     nil,
		},
		{
			// No id to ask GitHub about; registering the observed id or a
			// rename onto the name takes such a row in instead.
			name:     "a stored row with no id is never reported",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "no-id", ExternalID: "99"}},
			want:     nil,
		},
		{
			name:     "an observation with no id reports nothing",
			observed: []RepoRef{{Host: gh, Owner: "octo", Repo: "api"}},
			want:     nil,
		},
		{
			name:     "the same name on another host is another repository",
			observed: []RepoRef{{Host: ghe, Owner: "octo", Repo: "api", ExternalID: "99"}},
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range DetectReassignedRepoNames(stored, tc.observed) {
				got = append(got, r.ExternalID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DetectReassignedRepoNames = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenamedOwnerExternalID(t *testing.T) {
	tests := []struct {
		stored, storedOwnerID string
		observed              RepoRef
		want                  string
		because               string
	}{
		{"octo/api", "900", RepoRef{Owner: "globex", Repo: "api", OwnerExternalID: "950"}, "950", "a transfer the observation names"},
		{"octo/api", "900", RepoRef{Owner: "Octo", Repo: "platform-api"}, "900", "the owner login did not move, case-insensitively"},
		{"octo/api", "900", RepoRef{Owner: "globex", Repo: "api"}, "", "the owner moved and the observation names no account"},
	}
	for _, tc := range tests {
		t.Run(tc.because, func(t *testing.T) {
			if got := RenamedOwnerExternalID(tc.stored, tc.storedOwnerID, tc.observed); got != tc.want {
				t.Errorf("RenamedOwnerExternalID(%q, %q, %s) = %q, want %q", tc.stored, tc.storedOwnerID, tc.observed.Slug(), got, tc.want)
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
