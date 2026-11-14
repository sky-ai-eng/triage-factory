package ghwrite

import (
	"slices"
	"testing"
)

func TestNamedRepository(t *testing.T) {
	want := RepoRef{Owner: "octo", Repo: "repo"}
	named := []string{
		"/repos/octo/repo",
		"/repos/octo/repo/",
		"/repos/octo/repo/pulls/42",
		"/repos/octo/repo/issues/comments/7",
		"/repos/octo/repo/git/refs/heads/main",
		// The GHE prefix gh emits, and the paths the upstream router resolves.
		"/api/v3/repos/octo/repo/pulls/42",
		"/api/v3//repos/octo/repo",
		"/api/v3/repos/other/x/../../octo/repo/pulls",
		"/api/v3/../repos/octo/repo",
		// Past the root, an upstream under /api/v3 resolves this into the
		// collection.
		"/api/v3/../v3/repos/octo/repo",
		"/REPOS/octo/repo",
	}
	for _, path := range named {
		if got, ok := NamedRepository(path); !ok || got != want {
			t.Errorf("NamedRepository(%q) = %+v, %v; want %+v", path, got, ok, want)
		}
	}

	none := []string{
		"/user",
		"/user/repos",
		"/orgs/octo/repos",
		"/search/issues",
		"/repositories/4242",
		"/repos",
		"/repos/octo",
		"/repos/octo/",
		"/api/v3/repos/octo",
		"/api/v3",
		"/api/graphql",
		// Not anchored at the collection.
		"/users/octo/repos/octo/repo",
	}
	for _, path := range none {
		if got, ok := NamedRepository(path); ok {
			t.Errorf("NamedRepository(%q) = %+v, want no repository", path, got)
		}
	}

	// A name outside a repository's shape is still named, so the gate can
	// refuse it rather than pass it.
	if got, ok := NamedRepository("/repos/octo/re po/pulls"); !ok || got.Valid() {
		t.Errorf("NamedRepository(re po) = %+v, %v; want a named, invalid repository", got, ok)
	}
}

func TestRepoRef(t *testing.T) {
	ref := RepoRef{Owner: "Octo", Repo: "Repo.js"}
	if ref.String() != "Octo/Repo.js" || ref.Key() != "octo/repo.js" || !ref.Valid() {
		t.Errorf("RepoRef = %q, key %q, valid %v", ref.String(), ref.Key(), ref.Valid())
	}
	for _, bad := range []RepoRef{{"", "repo"}, {"octo", ""}, {"..", "repo"}, {"octo", "a/b"}, {"octo", "a%2fb"}} {
		if bad.Valid() {
			t.Errorf("%+v is valid, want invalid", bad)
		}
	}
}

// Every repository the query that runs selects at its top level is named,
// whatever it selects under it, and one whose owner or name cannot be read is
// reported rather than dropped.
func TestParseGraphQLRead_Repositories(t *testing.T) {
	own := RepoRef{Owner: "octo", Repo: "repo"}
	other := RepoRef{Owner: "acme", Repo: "payments"}
	cases := []struct {
		name          string
		query         string
		operationName string
		variables     map[string]any
		want          []RepoRef
		unread        bool
	}{
		{
			name:      "variables",
			query:     `query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) { pullRequest(number: 1) { id } } }`,
			variables: map[string]any{"owner": "octo", "repo": "repo"},
			want:      []RepoRef{own},
		},
		{
			name:  "a repository with no object",
			query: `{ repository(owner:"octo", name:"repo") { id name visibility } }`,
			want:  []RepoRef{own},
		},
		{
			name:  "a list under it",
			query: `{ repository(owner:"acme", name:"payments") { pullRequests(first: 30) { nodes { number } } } }`,
			want:  []RepoRef{other},
		},
		{
			name:  "several, aliased, one twice",
			query: `{ a: repository(owner:"octo", name:"repo") { id } b: repository(name:"payments", owner:"acme") { id } c: repository(owner:"OCTO", name:"REPO") { id } }`,
			want:  []RepoRef{own, other},
		},
		{
			// Past the bound on addressed objects, every repository is still
			// named.
			name: "more than the addressed bound",
			query: `{ ` + func() string {
				q := ""
				for i := range maxAddressedReads + 2 {
					q += `r` + string(rune('a'+i)) + `: repository(owner:"acme", name:"r` + string(rune('a'+i)) + `") { id } `
				}
				return q
			}() + `}`,
			want: func() []RepoRef {
				var out []RepoRef
				for i := range maxAddressedReads + 2 {
					out = append(out, RepoRef{Owner: "acme", Repo: "r" + string(rune('a'+i))})
				}
				return out
			}(),
		},
		{
			name:          "only the operation that runs",
			query:         `query A { repository(owner:"acme", name:"payments") { id } } query B { repository(owner:"octo", name:"repo") { id } }`,
			operationName: "B",
			want:          []RepoRef{own},
		},
		{
			name:   "an escaped literal",
			query:  `{ repository(owner:"acme", name:"pay\u006dents") { id } }`,
			unread: true,
		},
		{
			name:      "an unset variable beside a readable one",
			query:     `query($o: String!) { a: repository(owner: $o, name:"x") { id } b: repository(owner:"octo", name:"repo") { id } }`,
			variables: map[string]any{},
			want:      []RepoRef{own},
			unread:    true,
		},
		{
			name:   "an owner that is not one",
			query:  `{ repository(owner: OCTO, name:"repo") { id } }`,
			unread: true,
		},

		// What names no repository here.
		{name: "a repository below the top level", query: `{ organization(login: "acme") { repository(name: "payments") { id } } }`},
		{name: "a repository missing an argument", query: `{ repository(name: "payments") { id } }`},
		{name: "a fragment at the top level", query: `query { ...F } fragment F on Query { repository(owner:"acme", name:"payments") { id } }`},
		{name: "a mutation", query: `mutation { repository(owner:"acme", name:"payments") { id } }`},
		{name: "a node lookup", query: `{ node(id: "R_x") { id } }`},
		{name: "a document the reader cannot walk", query: `{ repository(owner:"acme", name:"payments") { id }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGraphQLRead(readEnvelope(t, tc.query, tc.operationName, tc.variables))
			if !slices.Equal(got.Repositories, tc.want) || got.UnreadRepository != tc.unread {
				t.Errorf("repositories %+v (unread %v), want %+v (unread %v)",
					got.Repositories, got.UnreadRepository, tc.want, tc.unread)
			}
		})
	}
}
