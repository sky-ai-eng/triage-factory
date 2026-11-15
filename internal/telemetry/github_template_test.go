package telemetry

import "testing"

// TestGitHubURLTemplate pins the normalizer's rules: the query is dropped,
// the segments after repos, orgs, and users take placeholders whatever they
// say, GitHub's own words are kept, and every other segment becomes "{}".
func TestGitHubURLTemplate(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "pull request reviews",
			path: "/repos/acme/widgets/pulls/7/reviews",
			want: "/repos/{owner}/{repo}/pulls/{}/reviews",
		},
		{
			name: "repository named like a path word, with a query string",
			path: "/repos/acme/pulls/pulls/7/reviews?per_page=100",
			want: "/repos/{owner}/{repo}/pulls/{}/reviews",
		},
		{
			name: "owner and repository both named like the named words",
			path: "/repos/repos/orgs/issues/3/comments",
			want: "/repos/{owner}/{repo}/issues/{}/comments",
		},
		{
			name: "enterprise prefix and a paged timeline",
			path: "/api/v3/repos/acme/widgets/issues/18/timeline?per_page=100&page=2",
			want: "/api/v3/repos/{owner}/{repo}/issues/{}/timeline",
		},
		{
			name: "free-form branch segments",
			path: "/repos/acme/widgets/git/ref/heads/feature/secret-launch",
			want: "/repos/{owner}/{repo}/git/ref/heads/{}/{}",
		},
		{
			name: "free-form segment equal to a path word is kept",
			path: "/repos/acme/widgets/git/refs/heads/comments",
			want: "/repos/{owner}/{repo}/git/refs/heads/comments",
		},
		{
			name: "escaped file path under contents",
			path: "/repos/acme/widgets/contents/docs/launch%20plan.md",
			want: "/repos/{owner}/{repo}/contents/{}/{}",
		},
		{
			name: "compare range",
			path: "/repos/acme/widgets/compare/main...feature%2Fx",
			want: "/repos/{owner}/{repo}/compare/{}",
		},
		{
			name: "organization",
			path: "/orgs/acme/teams?per_page=100&page=1",
			want: "/orgs/{org}/teams",
		},
		{
			name: "user by login",
			path: "/users/octocat/ssh_signing_keys?per_page=100&page=1",
			want: "/users/{user}/ssh_signing_keys",
		},
		{
			name: "user by id",
			path: "/user/583231",
			want: "/user/{}",
		},
		{
			name: "installation token",
			path: "/app/installations/987/access_tokens",
			want: "/app/installations/{}/access_tokens",
		},
		{
			name: "search query",
			path: "/search/issues?q=repo:acme/widgets+is:pr&per_page=50",
			want: "/search/issues",
		},
		{
			name: "graphql",
			path: "/api/graphql",
			want: "/api/graphql",
		},
		{
			name: "unknown prefix",
			path: "/acme-gateway/api/v3/user",
			want: "/{}/api/v3/user",
		},
		{
			name: "named word at the end",
			path: "/repos/acme",
			want: "/repos/{owner}",
		},
		{
			name: "empty segments stay empty",
			path: "/repos//widgets/",
			want: "/repos//{repo}/",
		},
		{
			name: "root",
			path: "/",
			want: "/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := githubURLTemplate(tc.path); got != tc.want {
				t.Errorf("githubURLTemplate(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
