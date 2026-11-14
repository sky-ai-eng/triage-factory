package ghwrite

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestClassifyRead(t *testing.T) {
	want := ObjectRef{Owner: "octo", Repo: "repo", Number: 42}
	addressed := []string{
		"/repos/octo/repo/pulls/42",
		"/repos/octo/repo/pulls/42/files",
		"/repos/octo/repo/pulls/42/reviews/9/comments",
		"/repos/octo/repo/issues/42",
		"/repos/octo/repo/issues/42/comments",
		"/repos/octo/repo/issues/42/timeline",
		// The GHES prefix is still on an upstream path.
		"/api/v3/repos/octo/repo/pulls/42",
	}
	for _, path := range addressed {
		if got, ok := ClassifyRead("GET", path); !ok || got != want {
			t.Errorf("ClassifyRead(GET %s) = %+v, %v; want %+v", path, got, ok, want)
		}
	}

	none := []struct{ method, path string }{
		// A set, a comment addressed by its own id, and objects that are neither
		// a pull request nor an issue.
		{"GET", "/repos/octo/repo/pulls"},
		{"GET", "/repos/octo/repo/issues"},
		{"GET", "/repos/octo/repo/pulls/comments/7"},
		{"GET", "/repos/octo/repo/issues/comments/7"},
		{"GET", "/repos/octo/repo/issues/events/7"},
		{"GET", "/repos/octo/repo"},
		{"GET", "/repos/octo/repo/commits/abc/pulls"},
		{"GET", "/repos/octo/repo/actions/runs/42"},
		{"GET", "/user"},
		{"GET", "/repos/octo/repo/pulls/0"},
		{"GET", "/repos/octo/repo/pulls/-3"},
		// A name that could address another path of the identity read.
		{"GET", "/repos/../repo/pulls/42"},
		{"GET", "/repos/octo/re po/pulls/42"},
		// A write is the write audit's.
		{"POST", "/repos/octo/repo/issues/42/comments"},
		{"PATCH", "/repos/octo/repo/pulls/42"},
		{"HEAD", "/repos/octo/repo/pulls/42"},
	}
	for _, tc := range none {
		if got, ok := ClassifyRead(tc.method, tc.path); ok {
			t.Errorf("ClassifyRead(%s %s) = %+v, want nothing addressed", tc.method, tc.path, got)
		}
	}
}

func readEnvelope(t *testing.T, query, operationName string, variables map[string]any) []byte {
	t.Helper()
	envelope := map[string]any{"query": query}
	if operationName != "" {
		envelope["operationName"] = operationName
	}
	if variables != nil {
		envelope["variables"] = variables
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

// The documents the pinned gh sends for its addressed reads, trimmed to the
// part that names the object or the branch. gh_wire_test drives the binary
// itself; these pin the reader against the same shapes without it.
func TestParseGraphQLRead_PorcelainDocuments(t *testing.T) {
	object := func(n int) AddressedReads {
		return AddressedReads{Objects: []ObjectRef{{Owner: "octo", Repo: "repo", Number: n}}}
	}
	branch := AddressedReads{Branches: []BranchRef{{Owner: "octo", Repo: "repo", Branch: "feature"}}}
	cases := []struct {
		name      string
		query     string
		variables map[string]any
		want      AddressedReads
	}{
		{
			name:      "pr view",
			query:     `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number) {url,number,title,author{login,...on User{id,name}},comments(first: 100){nodes{body}}} } }`,
			variables: map[string]any{"owner": "octo", "repo": "repo", "pr_number": 42},
			want:      object(42),
		},
		{
			name:      "issue view",
			query:     `query IssueByNumber($owner: String!, $repo: String!, $number: Int!) { repository(owner: $owner, name: $repo) { hasIssuesEnabled issue: issueOrPullRequest(number: $number) { __typename ...on Issue{number,url,title} ...on PullRequest{number,url} } } }`,
			variables: map[string]any{"owner": "octo", "repo": "repo", "number": 7},
			want:      object(7),
		},
		{
			name:      "project items",
			query:     `query IssueProjectItems($endCursor:String$name:String!$number:Int!$owner:String!){repository(owner: $owner, name: $name){issue(number: $number){projectItems(first: 100, after: $endCursor){totalCount,nodes{id,status:fieldValueByName(name: "Status"){...on ProjectV2ItemFieldSingleSelectValue{name}}}}}}}`,
			variables: map[string]any{"endCursor": nil, "name": "repo", "number": 7, "owner": "octo"},
			want:      object(7),
		},
		{
			// `gh pr view` and `gh pr checks` with no number, or with a branch.
			name: "pr view by branch",
			query: "\n\tquery PullRequestForBranch($owner: String!, $repo: String!, $headRefName: String!, $states: [PullRequestState!]) {\n\t\trepository(owner: $owner, name: $repo) {\n\t\t\t" +
				"pullRequests(headRefName: $headRefName, states: $states, first: 30, orderBy: { field: CREATED_AT, direction: DESC }) {\n\t\t\t\t" +
				"nodes {number,headRefName,id,state,baseRefName,isCrossRepository,headRepositoryOwner{id,login,...on User{name}}}\n\t\t\t}\n\t\t\t" +
				"defaultBranchRef { name }\n\t\t}\n\t}",
			variables: map[string]any{"headRefName": "feature", "owner": "octo", "repo": "repo", "states": nil},
			want:      branch,
		},
		{
			// `gh pr status`, behind the fragments it defines first, with the
			// searches beside the repository naming nothing.
			name: "pr status",
			query: "\n\tfragment pr on PullRequest {number,title,state,url,isDraft,headRefName}\n\tfragment prWithReviews on PullRequest {...pr,reviewDecision,latestReviews(first: 100) {nodes {author{login},state}}}\n" +
				"\tquery PullRequestStatus($owner: String!, $repo: String!, $headRefName: String!, $viewerQuery: String!, $reviewerQuery: String!, $per_page: Int = 10) {\n" +
				"\t\trepository(owner: $owner, name: $repo) {\n\t\t\tdefaultBranchRef {\n\t\t\t\tname\n\t\t\t}\n" +
				"\t\t\tpullRequests(headRefName: $headRefName, first: $per_page, orderBy: { field: CREATED_AT, direction: DESC }) {\n\t\t\t\ttotalCount\n\t\t\t\tedges {\n\t\t\t\t\tnode {\n\t\t\t\t\t\t...prWithReviews\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n\t\t}\n" +
				"\t\tviewerCreated: search(query: $viewerQuery, type: ISSUE, first: $per_page) {\n\t\t\ttotalCount: issueCount\n\t\t\tedges {\n\t\t\t\tnode {\n\t\t\t\t\t...prWithReviews\n\t\t\t\t}\n\t\t\t}\n\t\t}\n" +
				"\t\treviewRequested: search(query: $reviewerQuery, type: ISSUE, first: $per_page) {\n\t\t\ttotalCount: issueCount\n\t\t\tedges {\n\t\t\t\tnode {\n\t\t\t\t\t...pr\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t}\n\t",
			variables: map[string]any{"headRefName": "feature", "number": 0, "owner": "octo", "repo": "repo",
				"reviewerQuery": "repo:octo/repo state:open review-requested:", "viewerQuery": "repo:octo/repo state:open is:pr author:"},
			want: branch,
		},
		{
			// `gh pr status --repo`, which reads no current branch and sends it
			// empty.
			name:      "pr status for another repository",
			query:     `query PullRequestStatus($owner: String!, $repo: String!, $headRefName: String!) { repository(owner: $owner, name: $repo) { pullRequests(headRefName: $headRefName, first: 10) { totalCount } } }`,
			variables: map[string]any{"headRefName": "", "owner": "octo", "repo": "repo"},
		},
		{
			// `gh pr list`, which sends no head branch.
			name:      "pr list",
			query:     "fragment pr on PullRequest{number,title,headRefName}\n\t\tquery PullRequestList(\n\t\t\t$owner: String!,\n\t\t\t$repo: String!,\n\t\t\t$limit: Int!,\n\t\t\t$endCursor: String,\n\t\t\t$baseBranch: String,\n\t\t\t$headBranch: String,\n\t\t\t$state: [PullRequestState!] = OPEN\n\t\t) {\n\t\t\trepository(owner: $owner, name: $repo) {\n\t\t\t\tpullRequests(\n\t\t\t\t\tstates: $state,\n\t\t\t\t\tbaseRefName: $baseBranch,\n\t\t\t\t\theadRefName: $headBranch,\n\t\t\t\t\tfirst: $limit,\n\t\t\t\t\tafter: $endCursor,\n\t\t\t\t\torderBy: {field: CREATED_AT, direction: DESC}\n\t\t\t\t) {\n\t\t\t\t\ttotalCount\n\t\t\t\t\tnodes {\n\t\t\t\t\t\t...pr\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n\t\t}",
			variables: map[string]any{"limit": 30, "owner": "octo", "repo": "repo", "state": []string{"OPEN"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGraphQLRead(readEnvelope(t, tc.query, "", tc.variables))
			assertAddressed(t, got, tc.want)
		})
	}
}

// assertAddressed compares what a request addressed with what it should have.
func assertAddressed(t *testing.T, got, want AddressedReads) {
	t.Helper()
	if !slices.Equal(got.Objects, want.Objects) || !slices.Equal(got.Branches, want.Branches) {
		t.Errorf("addressed %+v, want %+v", got, want)
	}
}

func TestParseGraphQLRead_HandWritten(t *testing.T) {
	pr := func(n int) ObjectRef { return ObjectRef{Owner: "octo", Repo: "repo", Number: n} }
	cases := []struct {
		name          string
		query         string
		operationName string
		variables     map[string]any
		want          []ObjectRef
	}{
		{
			name:  "shorthand with literals",
			query: `{ repository(owner:"octo", name:"repo") { pullRequest(number: 42) { reviewThreads(first: 50) { nodes { isResolved } } } } }`,
			want:  []ObjectRef{pr(42)},
		},
		{
			name:  "aliases at both levels",
			query: `query { r: repository(name: "repo", owner: "octo") { a: pullRequest(number: 1) { id } b: issue(number: 2) { id } } }`,
			want:  []ObjectRef{pr(1), pr(2)},
		},
		{
			name:  "two repositories",
			query: `{ x: repository(owner:"octo", name:"repo") { pullRequest(number: 1) { id } } y: repository(owner:"acme", name:"widgets") { issue(number: 9) { id } } }`,
			want:  []ObjectRef{pr(1), {Owner: "acme", Repo: "widgets", Number: 9}},
		},
		{
			name:  "the same object twice is one",
			query: `{ repository(owner:"octo", name:"repo") { a: pullRequest(number: 1) { id } b: issueOrPullRequest(number: 1) { __typename } } }`,
			want:  []ObjectRef{pr(1)},
		},
		{
			name:      "mixed literal and variable",
			query:     `query($n: Int!) { repository(owner:"octo", name:"repo") { pullRequest(number: $n) { id } } }`,
			variables: map[string]any{"n": 42},
			want:      []ObjectRef{pr(42)},
		},
		{
			name:          "operationName picks the query that runs",
			query:         `query A { repository(owner:"octo", name:"repo") { pullRequest(number: 1) { id } } } query B { repository(owner:"octo", name:"repo") { pullRequest(number: 2) { id } } }`,
			operationName: "B",
			want:          []ObjectRef{pr(2)},
		},
		{
			name:  "directives and other arguments are passed over",
			query: `{ repository(owner:"octo", name:"repo", followRenames: true) @include(if: true) { pullRequest(number: 42) @skip(if: false) { id } labels(first: 10, orderBy: {field: NAME, direction: ASC}) { nodes { name } } } }`,
			want:  []ObjectRef{pr(42)},
		},
		{
			name:  "text inside a string is not structure",
			query: `{ search(query: "repository(owner:\"octo\", name:\"repo\") { pullRequest(number: 5) }", type: ISSUE, first: 1) { issueCount } repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } } }`,
			want:  []ObjectRef{pr(42)},
		},

		// What addresses nothing.
		{
			name:  "a list",
			query: `{ repository(owner:"octo", name:"repo") { pullRequests(first: 30, states: [OPEN]) { nodes { number } } } }`,
		},
		{
			name:  "a search",
			query: `{ search(query: "repo:octo/repo is:pr", type: ISSUE, first: 10) { nodes { ... on PullRequest { number } } } }`,
		},
		{
			name:  "a node lookup",
			query: `{ node(id: "PR_kwX") { ... on PullRequest { number } } }`,
		},
		{
			name:  "a repository with no object",
			query: `{ repository(owner:"octo", name:"repo") { id name } }`,
		},
		{
			name:  "a repository below the top level",
			query: `{ organization(login: "octo") { repository(name: "repo") { pullRequest(number: 42) { id } } } }`,
		},
		{
			name:  "an object below the repository's own selection",
			query: `{ repository(owner:"octo", name:"repo") { ref(qualifiedName: "main") { associatedPullRequests(first: 1) { nodes { number } } } } }`,
		},
		{
			name:  "a mutation is the write audit's",
			query: `mutation { addComment(input: {subjectId: "PR_x", body: "hi"}) { clientMutationId } }`,
		},
		{
			name:  "a mutation that selects a repository",
			query: `mutation { repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } } }`,
		},
		{
			name:  "several operations and none named",
			query: `query A { repository(owner:"octo", name:"repo") { pullRequest(number: 1) { id } } } query B { viewer { login } }`,
		},
		{
			name:          "an operationName naming no operation",
			query:         `query A { repository(owner:"octo", name:"repo") { pullRequest(number: 1) { id } } }`,
			operationName: "Z",
		},
		{
			name:      "an unset variable",
			query:     `query($n: Int!) { repository(owner:"octo", name:"repo") { pullRequest(number: $n) { id } } }`,
			variables: map[string]any{},
		},
		{
			name:      "a number that is not a positive integer",
			query:     `query($n: Int!) { repository(owner:"octo", name:"repo") { a: pullRequest(number: $n) { id } b: pullRequest(number: 0) { id } c: pullRequest(number: -4) { id } d: pullRequest(number: 4.5) { id } e: pullRequest(number: "7") { id } } }`,
			variables: map[string]any{"n": "42"},
		},
		{
			name:      "an owner that is not a repository owner",
			query:     `query($o: String!) { a: repository(owner: $o, name:"repo") { pullRequest(number: 1) { id } } b: repository(owner: "oc/to", name:"repo") { pullRequest(number: 1) { id } } c: repository(owner: "..", name:"repo") { pullRequest(number: 1) { id } } d: repository(owner: "octo", name:"re\u0070o") { pullRequest(number: 1) { id } } e: repository(owner: 7, name:"repo") { pullRequest(number: 1) { id } } }`,
			variables: map[string]any{"o": "octo/repo/pulls/1?x="},
		},
		{
			name:  "a block string owner",
			query: `{ repository(owner: """octo""", name:"repo") { pullRequest(number: 1) { id } } }`,
		},
		{
			name:  "a repository reached through a fragment",
			query: `query { ...F } fragment F on Query { repository(owner:"octo", name:"repo") { pullRequest(number: 1) { id } } }`,
		},
		{
			name:  "an unterminated document",
			query: `{ repository(owner:"octo", name:"repo") { pullRequest(number: 1) { id }`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGraphQLRead(readEnvelope(t, tc.query, tc.operationName, tc.variables))
			assertAddressed(t, got, AddressedReads{Objects: tc.want})
		})
	}
}

// A lookup of pull requests by head branch, directly under a repository the
// query names, addresses the branch, in every spelling the object finders are
// read in; anything else that mentions a branch addresses nothing.
func TestParseGraphQLRead_Branches(t *testing.T) {
	branch := func(name string) BranchRef { return BranchRef{Owner: "octo", Repo: "repo", Branch: name} }
	cases := []struct {
		name          string
		query         string
		operationName string
		variables     map[string]any
		want          AddressedReads
	}{
		{
			name:  "a literal",
			query: `{ repository(owner:"octo", name:"repo") { pullRequests(headRefName: "feat/login-form", first: 1) { nodes { number } } } }`,
			want:  AddressedReads{Branches: []BranchRef{branch("feat/login-form")}},
		},
		{
			name:      "a variable, under aliases",
			query:     `query($b: String!) { r: repository(owner:"octo", name:"repo") { open: pullRequests(states: OPEN, headRefName: $b) { totalCount } } }`,
			variables: map[string]any{"b": "TFAC-1415_fix"},
			want:      AddressedReads{Branches: []BranchRef{branch("TFAC-1415_fix")}},
		},
		{
			name:  "an object and a branch in one request",
			query: `{ repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } pullRequests(headRefName: "feature") { totalCount } } }`,
			want: AddressedReads{
				Objects:  []ObjectRef{{Owner: "octo", Repo: "repo", Number: 42}},
				Branches: []BranchRef{branch("feature")},
			},
		},
		{
			name:  "the same branch twice is one, under any case of its repository",
			query: `{ a: repository(owner:"octo", name:"repo") { pullRequests(headRefName: "feature") { totalCount } } b: repository(owner:"OCTO", name:"Repo") { pullRequests(headRefName: "feature") { totalCount } } }`,
			want:  AddressedReads{Branches: []BranchRef{branch("feature")}},
		},
		{
			name:  "branches differing in case are two",
			query: `{ repository(owner:"octo", name:"repo") { a: pullRequests(headRefName: "feature") { totalCount } b: pullRequests(headRefName: "Feature") { totalCount } } }`,
			want:  AddressedReads{Branches: []BranchRef{branch("feature"), branch("Feature")}},
		},
		{
			name:  "a branch name git admits",
			query: `{ repository(owner:"octo", name:"repo") { a: pullRequests(headRefName: "a.b/c-d_e+f#g") { totalCount } b: pullRequests(headRefName: "fonctionnalité") { totalCount } } }`,
			want:  AddressedReads{Branches: []BranchRef{branch("a.b/c-d_e+f#g"), branch("fonctionnalité")}},
		},

		// What addresses nothing.
		{
			name:  "a list with no branch",
			query: `{ repository(owner:"octo", name:"repo") { pullRequests(first: 30, baseRefName: "main") { nodes { number } } } }`,
		},
		{
			name:      "a branch that is null, empty or unset",
			query:     `query($n: String, $e: String, $u: String) { repository(owner:"octo", name:"repo") { a: pullRequests(headRefName: null) { totalCount } b: pullRequests(headRefName: $n) { totalCount } c: pullRequests(headRefName: $e) { totalCount } d: pullRequests(headRefName: $u) { totalCount } e: pullRequests(headRefName: "") { totalCount } } }`,
			variables: map[string]any{"n": nil, "e": ""},
		},
		{
			name:      "a branch that is not a string",
			query:     `query($b: String) { repository(owner:"octo", name:"repo") { a: pullRequests(headRefName: $b) { totalCount } b: pullRequests(headRefName: 7) { totalCount } c: pullRequests(headRefName: FEATURE) { totalCount } d: pullRequests(headRefName: ["feature"]) { totalCount } } }`,
			variables: map[string]any{"b": 7},
		},
		{
			name:      "a branch name git refuses",
			query:     `query($a: String, $b: String, $c: String) { repository(owner:"octo", name:"repo") { a: pullRequests(headRefName: $a) { totalCount } b: pullRequests(headRefName: $b) { totalCount } c: pullRequests(headRefName: $c) { totalCount } d: pullRequests(headRefName: "octo:feature") { totalCount } e: pullRequests(headRefName: "-f") { totalCount } } }`,
			variables: map[string]any{"a": "../main", "b": "feat\u0000ure", "c": "refs/heads/x.lock"},
		},
		{
			name:  "an escaped or block string branch",
			query: `{ repository(owner:"octo", name:"repo") { a: pullRequests(headRefName: "fe\u0061ture") { totalCount } b: pullRequests(headRefName: """feature""") { totalCount } } }`,
		},
		{
			name:  "a branch on the wrong field",
			query: `{ repository(owner:"octo", name:"repo") { pullRequest(headRefName: "feature") { id } issues(headRefName: "feature") { totalCount } } }`,
		},
		{
			name:  "a number on the lookup",
			query: `{ repository(owner:"octo", name:"repo") { pullRequests(number: 42) { totalCount } } }`,
		},
		{
			name:  "a lookup below the repository's own selection",
			query: `{ repository(owner:"octo", name:"repo") { ref(qualifiedName: "refs/heads/feature") { associatedPullRequests(headRefName: "feature", first: 1) { nodes { number } } } } }`,
		},
		{
			name:  "a lookup under a repository below the top level",
			query: `{ organization(login: "octo") { repository(name: "repo") { pullRequests(headRefName: "feature") { totalCount } } } }`,
		},
		{
			name:  "a lookup in a mutation",
			query: `mutation { repository(owner:"octo", name:"repo") { pullRequests(headRefName: "feature") { totalCount } } }`,
		},
		{
			name:  "a lookup reached through a fragment",
			query: `query { repository(owner:"octo", name:"repo") { ...F } } fragment F on Repository { pullRequests(headRefName: "feature") { totalCount } }`,
		},
		{
			name:      "a lookup in a repository the request does not name",
			query:     `query($o: String!) { repository(owner: $o, name:"repo") { pullRequests(headRefName: "feature") { totalCount } } }`,
			variables: map[string]any{"o": "octo/repo/pulls?head="},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGraphQLRead(readEnvelope(t, tc.query, tc.operationName, tc.variables))
			assertAddressed(t, got, tc.want)
		})
	}
}

// The shape a branch name is accepted in is git's own: each name here is one
// `git branch` creates, or refuses, except that a name that is not UTF-8 is
// refused here and not by git.
func TestValidBranchName(t *testing.T) {
	for _, name := range []string{
		"feature", "feat/login-form", "a.b", "a@b", "@", "a-", "a/HEAD", "x.lockx", "a#b", "a+b", "a&b", "a%b",
		"a=b", "{a}", "a]b", "a'b", `a"b`, "a!b", "fonctionnalité",
	} {
		if !validBranchName(name) {
			t.Errorf("validBranchName(%q) = false, want git's name admitted", name)
		}
	}
	for _, name := range []string{
		"", "a..b", ".a", "a/.b", "a.lock", "a/b.lock", "a.lock/b", "a/", "/a", "a//b", "a.", "HEAD", "a@{b", "-a",
		"a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a\x01b", "a\x7fb", "a\xffb",
		strings.Repeat("a", maxBranchNameLength+1),
	} {
		if validBranchName(name) {
			t.Errorf("validBranchName(%q) = true, want a name git refuses refused", name)
		}
	}
}

func TestParseGraphQLRead_NotAnEnvelope(t *testing.T) {
	for _, body := range []string{``, `not json`, `{}`, `{"query":""}`, `[]`, `{"query":7}`} {
		if got := ParseGraphQLRead([]byte(body)); !got.Empty() {
			t.Errorf("ParseGraphQLRead(%q) = %+v, want nothing", body, got)
		}
	}
	// Variables that are not an object resolve no variable, and cost the
	// literals nothing.
	body := `{"query":"query($n:Int!){repository(owner:\"octo\",name:\"repo\"){a:pullRequest(number:$n){id} b:pullRequest(number:2){id}}}","variables":[1,2]}`
	assertAddressed(t, ParseGraphQLRead([]byte(body)), AddressedReads{Objects: []ObjectRef{{Owner: "octo", Repo: "repo", Number: 2}}})
}

// One request addresses a bounded number of objects and branches, together,
// however many it spells.
func TestParseGraphQLRead_Bounded(t *testing.T) {
	var doc strings.Builder
	doc.WriteString(`{ repository(owner:"octo", name:"repo") {`)
	for i := 1; i <= 3*maxAddressedReads; i++ {
		doc.WriteString(` p` + strconv.Itoa(i) + `: pullRequest(number: ` + strconv.Itoa(i) + `) { id }`)
		doc.WriteString(` b` + strconv.Itoa(i) + `: pullRequests(headRefName: "b` + strconv.Itoa(i) + `") { totalCount }`)
	}
	doc.WriteString(` } }`)
	got := ParseGraphQLRead(readEnvelope(t, doc.String(), "", nil))
	if n := len(got.Objects) + len(got.Branches); n != maxAddressedReads {
		t.Errorf("addressed %d objects and branches, want the bound %d", n, maxAddressedReads)
	}
	if len(got.Objects) == 0 || len(got.Branches) == 0 {
		t.Errorf("addressed %+v, want the bound filled in document order, with both", got)
	}
}

// The read walk and the mutation reader agree on which a request is: nothing
// the write audit records is also reported as a read.
func TestParseGraphQLRead_NeverNamesAWrite(t *testing.T) {
	for _, query := range []string{
		`mutation { repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } } }`,
		`mutation { repository(owner:"octo", name:"repo") { pullRequests(headRefName: "feature") { totalCount } } }`,
		`query A { repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } pullRequests(headRefName: "feature") { totalCount } } } mutation B { mergePullRequest(input: {pullRequestId: "PR_x"}) { clientMutationId } }`,
	} {
		for _, operationName := range []string{"", "B"} {
			body := readEnvelope(t, query, operationName, nil)
			_, isWrite := ParseGraphQLRequest(body)
			if got := ParseGraphQLRead(body); isWrite && !got.Empty() {
				t.Errorf("a request the write audit records (%q, operation %q) also addressed %+v", query, operationName, got)
			}
		}
	}
}

// FuzzParseGraphQLRead holds the read walk to what FuzzParseGraphQLRequest
// holds the mutation reader to: it terminates and does not panic on any body,
// and whatever it names is within its bounds and safe to put in the path or
// the query of the read the injector then makes with the run's credential.
func FuzzParseGraphQLRead(f *testing.F) {
	f.Add([]byte(`{"query":"{repository(owner:\"octo\",name:\"repo\"){pullRequest(number:42){id}}}"}`))
	f.Add([]byte(`{"query":"query($o:String!,$n:Int!){repository(owner:$o,name:\"r\"){issue(number:$n){id}}}","variables":{"o":"octo","n":7}}`))
	f.Add([]byte(`{"query":"query A{repository(owner:\"a\",name:\"b\"){x:issueOrPullRequest(number:1){id}}} query B{viewer{login}}","operationName":"A"}`))
	f.Add([]byte(`{"query":"{repository(owner:\"..\",name:\"b/c\"){pullRequest(number:-1){id}}}"}`))
	f.Add([]byte(`{"query":"{repository(owner:\"a\",name:\"b\"){pullRequest(number:1){"}`))
	f.Add([]byte(`{"query":"query($h:String!){repository(owner:\"octo\",name:\"repo\"){pullRequests(headRefName:$h,first:30){nodes{number}}}}","variables":{"h":"feat/x"}}`))
	f.Add([]byte(`{"query":"{repository(owner:\"octo\",name:\"repo\"){pullRequests(headRefName:\"a..b\"){totalCount}}}"}`))
	f.Add([]byte(`repository`))

	f.Fuzz(func(t *testing.T, body []byte) {
		got := ParseGraphQLRead(body)
		if n := len(got.Objects) + len(got.Branches); n > maxAddressedReads {
			t.Fatalf("addressed %d objects and branches, past the %d bound", n, maxAddressedReads)
		}
		for _, ref := range got.Objects {
			if !validRepoPart(ref.Owner) || !validRepoPart(ref.Repo) || ref.Number <= 0 {
				t.Fatalf("addressed %+v, which names no repository's object", ref)
			}
		}
		for _, ref := range got.Branches {
			if !validRepoPart(ref.Owner) || !validRepoPart(ref.Repo) || !validBranchName(ref.Branch) {
				t.Fatalf("addressed %+v, which names no repository's branch", ref)
			}
		}
		if _, isWrite := ParseGraphQLRequest(body); isWrite && !got.Empty() {
			t.Fatalf("a request the write audit records also addressed %+v", got)
		}
	})
}
