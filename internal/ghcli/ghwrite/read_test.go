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
// part that names the object. gh_wire_test drives the binary itself; these pin
// the reader against the same shapes without it.
func TestParseGraphQLRead_PorcelainDocuments(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		variables map[string]any
		want      ObjectRef
	}{
		{
			name:      "pr view",
			query:     `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number) {url,number,title,author{login,...on User{id,name}},comments(first: 100){nodes{body}}} } }`,
			variables: map[string]any{"owner": "octo", "repo": "repo", "pr_number": 42},
			want:      ObjectRef{Owner: "octo", Repo: "repo", Number: 42},
		},
		{
			name:      "issue view",
			query:     `query IssueByNumber($owner: String!, $repo: String!, $number: Int!) { repository(owner: $owner, name: $repo) { hasIssuesEnabled issue: issueOrPullRequest(number: $number) { __typename ...on Issue{number,url,title} ...on PullRequest{number,url} } } }`,
			variables: map[string]any{"owner": "octo", "repo": "repo", "number": 7},
			want:      ObjectRef{Owner: "octo", Repo: "repo", Number: 7},
		},
		{
			name:      "project items",
			query:     `query IssueProjectItems($endCursor:String$name:String!$number:Int!$owner:String!){repository(owner: $owner, name: $name){issue(number: $number){projectItems(first: 100, after: $endCursor){totalCount,nodes{id,status:fieldValueByName(name: "Status"){...on ProjectV2ItemFieldSingleSelectValue{name}}}}}}}`,
			variables: map[string]any{"endCursor": nil, "name": "repo", "number": 7, "owner": "octo"},
			want:      ObjectRef{Owner: "octo", Repo: "repo", Number: 7},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGraphQLRead(readEnvelope(t, tc.query, "", tc.variables))
			if !slices.Equal(got, []ObjectRef{tc.want}) {
				t.Errorf("addressed %+v, want %+v", got, tc.want)
			}
		})
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
			name:      "a lookup by branch",
			query:     `query PullRequestForBranch($owner: String!, $repo: String!, $headRefName: String!) { repository(owner: $owner, name: $repo) { pullRequests(headRefName: $headRefName, first: 30) { nodes { number } } } }`,
			variables: map[string]any{"owner": "octo", "repo": "repo", "headRefName": "feature"},
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
			if !slices.Equal(got, tc.want) {
				t.Errorf("addressed %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseGraphQLRead_NotAnEnvelope(t *testing.T) {
	for _, body := range []string{``, `not json`, `{}`, `{"query":""}`, `[]`, `{"query":7}`} {
		if got := ParseGraphQLRead([]byte(body)); got != nil {
			t.Errorf("ParseGraphQLRead(%q) = %+v, want nil", body, got)
		}
	}
	// Variables that are not an object resolve no variable, and cost the
	// literals nothing.
	body := `{"query":"query($n:Int!){repository(owner:\"octo\",name:\"repo\"){a:pullRequest(number:$n){id} b:pullRequest(number:2){id}}}","variables":[1,2]}`
	if got := ParseGraphQLRead([]byte(body)); !slices.Equal(got, []ObjectRef{{Owner: "octo", Repo: "repo", Number: 2}}) {
		t.Errorf("addressed %+v, want the literal alone", got)
	}
}

// One request addresses a bounded number of objects, however many it spells.
func TestParseGraphQLRead_Bounded(t *testing.T) {
	var doc strings.Builder
	doc.WriteString(`{ repository(owner:"octo", name:"repo") {`)
	for i := 1; i <= 3*maxAddressedReads; i++ {
		doc.WriteString(` p` + strconv.Itoa(i) + `: pullRequest(number: ` + strconv.Itoa(i) + `) { id }`)
	}
	doc.WriteString(` } }`)
	got := ParseGraphQLRead(readEnvelope(t, doc.String(), "", nil))
	if len(got) != maxAddressedReads {
		t.Errorf("addressed %d objects, want the bound %d", len(got), maxAddressedReads)
	}
}

// The read walk and the mutation reader agree on which a request is: nothing
// the write audit records is also reported as a read.
func TestParseGraphQLRead_NeverNamesAWrite(t *testing.T) {
	for _, query := range []string{
		`mutation { repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } } }`,
		`query A { repository(owner:"octo", name:"repo") { pullRequest(number: 42) { id } } } mutation B { mergePullRequest(input: {pullRequestId: "PR_x"}) { clientMutationId } }`,
	} {
		for _, operationName := range []string{"", "B"} {
			body := readEnvelope(t, query, operationName, nil)
			_, isWrite := ParseGraphQLRequest(body)
			if got := ParseGraphQLRead(body); isWrite && got != nil {
				t.Errorf("a request the write audit records (%q, operation %q) also addressed %+v", query, operationName, got)
			}
		}
	}
}

// FuzzParseGraphQLRead holds the read walk to what FuzzParseGraphQLRequest
// holds the mutation reader to: it terminates and does not panic on any body,
// and whatever it names is within its bounds and safe to put in the path of
// the read the injector then makes with the run's credential.
func FuzzParseGraphQLRead(f *testing.F) {
	f.Add([]byte(`{"query":"{repository(owner:\"octo\",name:\"repo\"){pullRequest(number:42){id}}}"}`))
	f.Add([]byte(`{"query":"query($o:String!,$n:Int!){repository(owner:$o,name:\"r\"){issue(number:$n){id}}}","variables":{"o":"octo","n":7}}`))
	f.Add([]byte(`{"query":"query A{repository(owner:\"a\",name:\"b\"){x:issueOrPullRequest(number:1){id}}} query B{viewer{login}}","operationName":"A"}`))
	f.Add([]byte(`{"query":"{repository(owner:\"..\",name:\"b/c\"){pullRequest(number:-1){id}}}"}`))
	f.Add([]byte(`{"query":"{repository(owner:\"a\",name:\"b\"){pullRequest(number:1){"}`))
	f.Add([]byte(`repository`))

	f.Fuzz(func(t *testing.T, body []byte) {
		refs := ParseGraphQLRead(body)
		if len(refs) > maxAddressedReads {
			t.Fatalf("addressed %d objects, past the %d bound", len(refs), maxAddressedReads)
		}
		for _, ref := range refs {
			if !validRepoPart(ref.Owner) || !validRepoPart(ref.Repo) || ref.Number <= 0 {
				t.Fatalf("addressed %+v, which names no repository's object", ref)
			}
		}
		if _, isWrite := ParseGraphQLRequest(body); isWrite && len(refs) > 0 {
			t.Fatalf("a request the write audit records also addressed %+v", refs)
		}
	})
}
