package ghinjector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghpin"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
)

// The conformance gate for the write-governance invariant: every write the
// pinned `gh` performs through this channel ends as a semantic audit row or as
// a refusal, and never as an unclassified one.
//
// It is the proof rather than the mechanism. The classification table
// (internal/ghwrite) was inferred from what the pinned binary sends, so the
// pinned binary is what has to drive it: an enumeration of the porcelain's
// write commands, each run against a fake upstream through a real injector,
// each asserted to leave exactly the rows it should. An unclassified row for an
// enumerated command fails the build — that is the whole point, and it is why
// the enumeration is a table rather than a handful of examples.
//
// Everything here is offline. The fake upstream answers each command's resolve
// traffic (a repository lookup, a pull request, a label id) well enough that gh
// reaches its write, which is the part a fixture usually gets wrong: a command
// that bails at resolution sends nothing at all and would pass a test that only
// asked "did anything unclassified happen?". The expectation is therefore the
// rows themselves, so a command that never wrote fails on a count of zero.
//
// Two families are deliberately out of the enumeration and have their own tests
// below: the release commands that follow an absolute url out of a response
// body, which never transit this channel at all, and repository creation, which
// is refused rather than named. Both are asserted rather than skipped — a list
// of what this channel cannot see is worth nothing unless something checks it is
// still true.

// The fixture's repository, and the object catalog inside it. A porcelain
// command reads its target's state before it writes and refuses to send
// anything when the state does not admit the act — `pr reopen` on an open pull
// request, `pr unlock` on an unlocked one — so the battery needs several
// objects in several states rather than one object it mutates. Nothing here
// mutates: the fake answers from a fixed catalog, and each command is handed a
// number whose state admits it.
const (
	confOwner = "octo"
	confRepo  = "repo"
	// The organization whose name makes `gh repo create` take its REST arm (it
	// asks the users endpoint which kind of account owns the target).
	confOrg = "bigorg"
	// The repository the archive fixture reports as already archived, so
	// `repo archive` and `repo unarchive` can both reach their mutation.
	confArchivedRepo = "archived"
)

// fixtureObject is one pull request or issue the fake serves, in the state its
// commands need to find it in.
type fixtureObject struct {
	nodeID string
	// typename is what the issueOrPullRequest finder reports. The lock verbs
	// address a pull request through the issue finder, so the two number spaces
	// share one lookup and it has to answer for both.
	typename   string
	state      string
	draft      bool
	locked     bool
	pinned     bool
	mergeState string
	url        string
}

// confObjects is the catalog, keyed by the number a command addresses. The two
// number spaces do not overlap, so the shared issueOrPullRequest finder can
// answer for either from one lookup.
var confObjects = map[int]fixtureObject{
	// Open, mergeable, unlocked, published: the object most of the battery acts
	// on.
	42: {nodeID: "PR_open", typename: "PullRequest", state: "OPEN", mergeState: "CLEAN",
		url: "https://ghe.test/octo/repo/pull/42"},
	43: {nodeID: "PR_closed", typename: "PullRequest", state: "CLOSED", mergeState: "CLEAN",
		url: "https://ghe.test/octo/repo/pull/43"},
	44: {nodeID: "PR_draft", typename: "PullRequest", state: "OPEN", draft: true, mergeState: "CLEAN",
		url: "https://ghe.test/octo/repo/pull/44"},
	45: {nodeID: "PR_locked", typename: "PullRequest", state: "OPEN", locked: true, mergeState: "CLEAN",
		url: "https://ghe.test/octo/repo/pull/45"},
	46: {nodeID: "PR_merged", typename: "PullRequest", state: "MERGED", mergeState: "CLEAN",
		url: "https://ghe.test/octo/repo/pull/46"},
	// Not immediately mergeable, which is the whole condition for the auto-merge
	// arm: gh merges outright when the pull request is already mergeable and
	// arms auto-merge only when it is not.
	47: {nodeID: "PR_blocked", typename: "PullRequest", state: "OPEN", mergeState: "BLOCKED",
		url: "https://ghe.test/octo/repo/pull/47"},

	7:  {nodeID: "I_open", typename: "Issue", state: "OPEN", url: "https://ghe.test/octo/repo/issues/7"},
	8:  {nodeID: "I_closed", typename: "Issue", state: "CLOSED", url: "https://ghe.test/octo/repo/issues/8"},
	9:  {nodeID: "I_locked", typename: "Issue", state: "OPEN", locked: true, url: "https://ghe.test/octo/repo/issues/9"},
	10: {nodeID: "I_pinned", typename: "Issue", state: "OPEN", pinned: true, url: "https://ghe.test/octo/repo/issues/10"},
}

// The comment the fixture hangs on every pull request and issue, authored by
// the viewer so `--edit-last` and `--delete-last` have something to act on.
const (
	confCommentNodeID = "IC_last"
	confCommentURL    = "https://ghe.test/octo/repo/pull/42#issuecomment-7"
)

// The release the release commands resolve, and the label the label commands
// address. Both are looked up before the write.
const (
	confReleaseID  = 9
	confReleaseTag = "v1.0.0"
	confLabelName  = "bug"
	confAssetName  = "asset.txt"
)

// upstreamRequest is one request the fake received, kept so the battery can
// assert on the documents gh actually sent — and on the requests it did NOT
// send, which is how the out-of-scope shapes are held honest.
type upstreamRequest struct {
	Method string
	Path   string
	Body   []byte
	// Query and Operation are the GraphQL envelope's, empty for REST.
	Query     string
	Operation string
}

// fakeUpstream is the offline GitHub the battery drives gh against, plus the
// off-channel host the release payloads point at — see offChannel.
type fakeUpstream struct {
	server *httptest.Server

	// offChannel stands in for the hosts a release payload's absolute urls name
	// (uploads.github.com, and the API root itself). Those urls address GitHub
	// directly rather than this channel's base, so a command that follows one
	// leaves the injector entirely; giving that traffic somewhere real to land
	// is what lets the battery prove it went off-channel rather than merely
	// observing that it failed.
	offChannel     *httptest.Server
	offChannelSeen struct {
		mu   sync.Mutex
		seen []upstreamRequest
	}

	mu   sync.Mutex
	seen []upstreamRequest
}

func (u *fakeUpstream) record(r upstreamRequest) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen = append(u.seen, r)
}

func (u *fakeUpstream) requests() []upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamRequest(nil), u.seen...)
}

func (u *fakeUpstream) recordOffChannel(r upstreamRequest) {
	u.offChannelSeen.mu.Lock()
	defer u.offChannelSeen.mu.Unlock()
	u.offChannelSeen.seen = append(u.offChannelSeen.seen, r)
}

func (u *fakeUpstream) offChannelRequests() []upstreamRequest {
	u.offChannelSeen.mu.Lock()
	defer u.offChannelSeen.mu.Unlock()
	return append([]upstreamRequest(nil), u.offChannelSeen.seen...)
}

// newFakeUpstream stands up the fixture endpoint. It is deliberately separate
// from fakeGHE, which answers only what its own two tests drive: that one is
// readable precisely because it is minimal, and this one has to hold every
// object state the enumeration needs.
func newFakeUpstream(t testing.TB) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.offChannel = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.recordOffChannel(upstreamRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":1,"name":"asset.txt","state":"uploaded"}`))
	}))
	t.Cleanup(u.offChannel.Close)

	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := upstreamRequest{Method: r.Method, Path: r.URL.Path, Body: body}

		var status int
		var response string
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			var envelope struct {
				Query         string `json:"query"`
				OperationName string `json:"operationName"`
			}
			_ = json.Unmarshal(body, &envelope)
			req.Query, req.Operation = envelope.Query, envelope.OperationName
			status, response = graphQLFixture(envelope.Query, body)
		} else {
			status, response = u.restFixture(r.Method, strings.TrimPrefix(r.URL.Path, restPrefix))
		}
		u.record(req)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(u.server.Close)
	return u
}

// graphQLFixture answers one GraphQL request. The dispatch is on the operation
// gh names, which is stable across the fields a given command asks for: gh
// tailors the selection set per command, so keying on the operation and
// answering the union is what keeps one fixture serving twenty commands.
func graphQLFixture(query string, body []byte) (int, string) {
	switch {
	case strings.Contains(query, "PullRequestByNumber"):
		return 200, prObjectJSON(objectFromVariables(body, "pr_number"))
	case strings.Contains(query, "IssueByNumber"):
		return 200, `{"data":{"repository":{"hasIssuesEnabled":true,"issue":` +
			issueObjectJSON(objectFromVariables(body, "number")) + `}}}`
	case strings.Contains(query, "IssueRepositoryInfo"):
		return 200, `{"data":{"repository":{"id":"R_repo","name":"` + confRepo + `","owner":{"login":"` + confOwner +
			`"},"hasIssuesEnabled":true,"viewerPermission":"WRITE"}}}`
	case strings.Contains(query, "RepositoryInfo"):
		return 200, repositoryInfoJSON(query, stringVariable(body, "name"))
	case strings.Contains(query, "RepositoryLabelList"):
		return 200, `{"data":{"repository":{"labels":{"nodes":[{"id":"LA_bug","name":"` + confLabelName + `"}],` +
			`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`
	case strings.Contains(query, "RepositoryAssignableUsers"):
		return 200, `{"data":{"repository":{"assignableUsers":{"nodes":[{"id":"U_someone","login":"someone","name":"Some One"}],` +
			`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`
	case strings.Contains(query, "PullRequestProjectItems"):
		return 200, `{"data":{"repository":{"pullRequest":{"projectItems":{"totalCount":0,"nodes":[],` +
			`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`
	case strings.Contains(query, "IssueProjectItems"):
		return 200, `{"data":{"repository":{"issue":{"projectItems":{"totalCount":0,"nodes":[],` +
			`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`
	case strings.Contains(query, "ComparePullRequestBaseBranchWith"):
		return 200, `{"data":{"repository":{"pullRequest":{"baseRef":{"compare":` +
			`{"aheadBy":1,"behindBy":3,"status":"BEHIND"}}}}}}`
	case strings.Contains(query, "LinkedBranchFeature"):
		return 200, `{"data":{"LinkedBranch":{"fields":[{"name":"id"},{"name":"ref"}]}}}`
	case strings.Contains(query, "FindRepoBranchID"):
		return 200, `{"data":{"repository":{"id":"R_repo","defaultBranchRef":{"target":{"oid":"deadbeef"}},` +
			`"ref":{"target":{"oid":"deadbeef"}}}}}`
	case strings.Contains(query, "RepositoryReleaseByTag"):
		return 200, `{"data":{"repository":{"release":{"databaseId":` + strconv.Itoa(confReleaseID) + `,"isDraft":false}}}}`
	case strings.Contains(query, "UserCurrent"):
		return 200, `{"data":{"viewer":{"id":"U_viewer"}}}`

	// The mutations. Their payloads matter only where the audit row reads one:
	// a create's response is where the object it made is located, and every
	// other mutation's row is complete from the request alone.
	case strings.Contains(query, "createPullRequest"):
		return 200, `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_new",` +
			`"url":"https://ghe.test/octo/repo/pull/99"}}}}`
	case strings.Contains(query, "createIssue"):
		return 200, `{"data":{"createIssue":{"issue":{"id":"I_new","url":"https://ghe.test/octo/repo/issues/98"}}}}`
	case strings.Contains(query, "revertPullRequest"):
		return 200, `{"data":{"revertPullRequest":{"pullRequest":{"id":"PR_merged"},` +
			`"revertPullRequest":{"id":"PR_revert","number":97,"url":"https://ghe.test/octo/repo/pull/97"}}}}`
	case strings.Contains(query, "addComment"):
		return 200, `{"data":{"addComment":{"commentEdge":{"node":{"url":"` + confCommentURL + `"}}}}}`
	case strings.Contains(query, "createLinkedBranch"):
		return 200, `{"data":{"createLinkedBranch":{"linkedBranch":{"id":"LB_1","ref":{"name":"7-i"}}}}}`
	case strings.Contains(query, "transferIssue"):
		return 200, `{"data":{"transferIssue":{"issue":{"url":"https://ghe.test/octo/other/issues/1"}}}}`
	// Unlock is matched first because its mutation name contains lock's, and gh
	// rejects a payload keyed under a field it did not select.
	case strings.Contains(query, "unlockLockable"):
		return 200, `{"data":{"unlockLockable":{"unlockedRecord":{"locked":false}}}}`
	case strings.Contains(query, "lockLockable"):
		return 200, `{"data":{"lockLockable":{"lockedRecord":{"locked":true}}}}`
	}
	// Every remaining mutation selects only ids or a clientMutationId, and gh
	// reads none of it.
	return 200, `{"data":{}}`
}

// prObjectJSON renders one pull request for the PullRequestByNumber finder.
// The field set is the union of what the battery's commands ask for; gh decodes
// this arm leniently, so a field a given command did not select is ignored.
func prObjectJSON(number int, o fixtureObject) string {
	return `{"data":{"repository":{"pullRequest":{` +
		`"id":"` + o.nodeID + `","number":` + strconv.Itoa(number) + `,"url":"` + o.url + `",` +
		`"title":"T","body":"B","state":"` + o.state + `","isDraft":` + strconv.FormatBool(o.draft) + `,` +
		`"locked":` + strconv.FormatBool(o.locked) + `,"activeLockReason":null,` +
		`"baseRefName":"main","headRefName":"feature","headRefOid":"deadbeef","isCrossRepository":false,` +
		`"mergeStateStatus":"` + o.mergeState + `","mergeable":"MERGEABLE",` +
		`"headRepositoryOwner":{"id":"U_octo","login":"` + confOwner + `"},` +
		`"author":{"login":"someone"},"labels":{"nodes":[],"totalCount":0},` +
		`"assignees":{"nodes":[],"totalCount":0},"reviewRequests":{"nodes":[]},"milestone":null,` +
		`"commits":{"totalCount":1,"nodes":[{"commit":{"oid":"deadbeef"}}]},` +
		`"comments":{"totalCount":1,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[` +
		lastCommentJSON() + `]}}}}}`
}

// lastCommentJSON is the viewer's own comment, which is what `--edit-last` and
// `--delete-last` select before they write.
func lastCommentJSON() string {
	return `{"id":"` + confCommentNodeID + `","author":{"login":"someone","id":"U_someone","name":"Some One"},` +
		`"authorAssociation":"OWNER","body":"prior","createdAt":"2026-01-01T00:00:00Z",` +
		`"includesCreatedEdit":false,"isMinimized":false,"minimizedReason":"","reactionGroups":[],` +
		`"url":"` + confCommentURL + `","viewerDidAuthor":true}`
}

// issueObjectJSON renders one object for the issueOrPullRequest finder, which
// serves the issue commands and the lock verbs on both kinds.
func issueObjectJSON(number int, o fixtureObject) string {
	return `{"__typename":"` + o.typename + `","id":"` + o.nodeID + `","number":` + strconv.Itoa(number) +
		`,"url":"` + o.url + `","title":"T","body":"B","state":"` + o.state + `",` +
		`"locked":` + strconv.FormatBool(o.locked) + `,"activeLockReason":null,` +
		`"isPinned":` + strconv.FormatBool(o.pinned) + `,"author":{"login":"someone"},` +
		`"labels":{"nodes":[],"totalCount":0},"assignees":{"nodes":[],"totalCount":0},"milestone":null,` +
		`"comments":{"totalCount":1,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[` +
		lastCommentJSON() + `]}}`
}

// repositoryInfoJSON answers the repository finder. Only the archived flag
// varies, and it varies by name because archive and unarchive need opposite
// answers and one battery runs both.
func repositoryInfoJSON(query, name string) string {
	if name == "" {
		name = confRepo
	}
	// gh decodes some of these responses strictly, so the owner carries its node
	// id only for the commands that selected one.
	owner := `{"login":"` + confOwner + `"}`
	if strings.Contains(strings.ReplaceAll(query, " ", ""), "owner{id") {
		owner = `{"id":"U_octo","login":"` + confOwner + `"}`
	}
	return `{"data":{"repository":{"id":"R_repo","name":"` + name + `","owner":` + owner + `,` +
		`"description":"d","isArchived":` + strconv.FormatBool(name == confArchivedRepo) + `,` +
		`"defaultBranchRef":{"name":"main"},"viewerPermission":"WRITE","hasIssuesEnabled":true,"hasWikiEnabled":true,` +
		`"mergeCommitAllowed":true,"rebaseMergeAllowed":true,"squashMergeAllowed":true,"visibility":"PUBLIC"}}}`
}

// objectFromVariables resolves the catalog entry a request addressed, so one
// fixture serves every state the battery needs. An unknown number falls back to
// the open pull request rather than failing: a command that asked for something
// the catalog does not hold will fail its own assertion, and it fails more
// legibly on a missing audit row than on a decode error.
func objectFromVariables(body []byte, key string) (int, fixtureObject) {
	number := intVariable(body, key)
	if o, ok := confObjects[number]; ok {
		return number, o
	}
	return number, confObjects[42]
}

func intVariable(body []byte, key string) int {
	var envelope struct {
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	var n int
	if err := json.Unmarshal(envelope.Variables[key], &n); err != nil {
		return 0
	}
	return n
}

func stringVariable(body []byte, key string) string {
	var envelope struct {
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(envelope.Variables[key], &s); err != nil {
		return ""
	}
	return s
}

// restFixture answers one REST request, on the upstream path (the GHE prefix gh
// emits is already stripped). Writes answer with the shape the audit reads: a
// create's response carries the new object's id and html url, and everything
// else needs only a status.
func (u *fakeUpstream) restFixture(method, path string) (int, string) {
	releasePath := "/repos/" + confOwner + "/" + confRepo + "/releases"
	switch {
	case path == "/meta":
		return 200, `{"verifiable_password_authentication":false}`

	// Which kind of account owns a repository, which is how `gh repo create`
	// picks between the user and the organization collection.
	case path == "/users/"+confOrg:
		return 200, `{"login":"` + confOrg + `","type":"Organization","id":2}`
	case strings.HasPrefix(path, "/users/"):
		return 200, `{"login":"` + confOwner + `","type":"User","id":1}`

	case path == releasePath+"/tags/"+confReleaseTag:
		return 200, u.releaseJSON()
	case path == releasePath && method == "POST":
		return 201, u.releaseJSON()
	case strings.HasPrefix(path, releasePath+"/"+strconv.Itoa(confReleaseID)):
		return 200, u.releaseJSON()

	case strings.HasSuffix(path, "/topics") && method == "GET":
		return 200, `{"names":["existing"]}`
	case strings.HasSuffix(path, "/topics") && method == "PUT":
		return 200, `{"names":["existing","t1"]}`

	case strings.HasSuffix(path, "/labels") && method == "POST":
		return 201, `{"id":11,"name":"triage","color":"5319E7","url":"https://ghe.test/api/v3/repos/octo/repo/labels/triage"}`
	case strings.Contains(path, "/labels/"):
		return 200, `{"id":12,"name":"` + confLabelName + `","color":"5319E7"}`

	case strings.HasSuffix(path, "/forks"):
		return 202, `{"id":2,"name":"` + confRepo + `","full_name":"someone/repo","owner":{"login":"someone"},` +
			`"html_url":"https://ghe.test/someone/repo","created_at":"2026-01-01T00:00:00Z"}`

	case strings.HasSuffix(path, "/requested_reviewers"):
		return 201, `{"id":3,"number":42,"html_url":"https://ghe.test/octo/repo/pull/42"}`

	// The two collections a repository is POSTed into. They are answered rather
	// than 404'd so that a failure to refuse them would show up as a successful
	// create, not as an upstream error the gate could be mistaken for.
	case path == "/user/repos" || strings.HasSuffix(path, "/repos") && strings.HasPrefix(path, "/orgs/"):
		return 201, `{"id":5,"name":"newrepo","full_name":"` + confOwner + `/newrepo",` +
			`"owner":{"login":"` + confOwner + `"},"html_url":"https://ghe.test/octo/newrepo"}`

	case strings.HasPrefix(path, "/repos/"):
		return 200, `{"id":4,"name":"` + confRepo + `","full_name":"` + confOwner + `/` + confRepo + `",` +
			`"owner":{"login":"` + confOwner + `"},"html_url":"https://ghe.test/octo/repo",` +
			`"node_id":"R_repo","number":42,"names":["existing","t1"]}`
	}
	return 404, `{"message":"no fixture for ` + method + " " + path + `"}`
}

// releaseJSON is the release payload, whose three absolute urls are the whole
// reason the asset commands are unobservable here: gh follows them verbatim,
// and they name a host rather than this channel's base.
func (u *fakeUpstream) releaseJSON() string {
	id := strconv.Itoa(confReleaseID)
	off := u.offChannel.URL
	return `{"id":` + id + `,"tag_name":"` + confReleaseTag + `","draft":false,"prerelease":false,` +
		`"html_url":"https://ghe.test/octo/repo/releases/tag/` + confReleaseTag + `",` +
		`"url":"` + off + `/repos/octo/repo/releases/` + id + `",` +
		`"upload_url":"` + off + `/repos/octo/repo/releases/` + id + `/assets{?name,label}",` +
		`"assets":[{"id":1,"name":"` + confAssetName + `","label":"","state":"uploaded",` +
		`"url":"` + off + `/repos/octo/repo/releases/assets/1"}]}`
}

// fmtRequest renders a recorded upstream request for a failure message.
func fmtRequest(r upstreamRequest) string {
	if r.Query != "" {
		return fmt.Sprintf("%s %s (%s)", r.Method, r.Path, strings.Join(strings.Fields(r.Query), " "))
	}
	return fmt.Sprintf("%s %s", r.Method, r.Path)
}

// wireExpect is one record a command must leave: a classified act, or a
// refusal. There is no third form, which is the invariant this file exists to
// hold — an unclassified record renders as a string no expectation can match
// and fails the case by name.
type wireExpect struct {
	// mutation is the GraphQL field the request selected; method and path name
	// a REST write instead. Exactly one of the two forms is set.
	mutation string
	method   string
	path     string

	// action is the act the classifier named (a domain.Action* const), or
	// refusal is the family the gate refused it under (a ghwrite.GateReason*).
	// Exactly one is set.
	action  string
	refusal string
}

func (e wireExpect) String() string {
	subject := e.mutation
	if subject == "" {
		subject = e.method + " " + e.path
	}
	if e.refusal != "" {
		return "refused(" + e.refusal + ") " + subject
	}
	return e.action + " " + subject
}

// wireRecords collects everything one command left behind, in the two places a
// record can land: the audit callback for a write that transited, and the
// gate's decision hook for one that did not.
type wireRecords struct {
	mu   sync.Mutex
	rows []string
	// documents is what the injector read out of every GraphQL request,
	// refused ones included, and sent is the raw body of each one that was
	// forwarded. Together they carry the properties the refuse-the-unreadable
	// decision rests on — the facts say whether a document could be named, the
	// bodies say how big it was and what it was made of.
	documents []ghwrite.GraphQLFacts
	sent      [][]byte
	// unaccepted names any audited write the upstream did not accept, which is
	// a fixture fault rather than a classification one and needs saying
	// separately: a row for a failed write is correct behaviour, but it means
	// the command under test never actually performed its act.
	unaccepted []string
}

func (r *wireRecords) audit(_ context.Context, w ObservedWrite) {
	r.mu.Lock()
	defer r.mu.Unlock()

	subject := restSubject(w.Method, w.Path)
	if w.GraphQL != nil {
		subject = graphQLSubject(*w.GraphQL)
		r.documents = append(r.documents, *w.GraphQL)
	}
	shape, ok := ghwrite.Resolve(w)
	if !ok {
		// The fourth state: a write happened and nothing named it. Rendered so
		// the failure message says which request, since that is the only thing
		// the reader needs to fix it.
		r.rows = append(r.rows, "UNCLASSIFIED "+subject)
		return
	}
	if !w.Succeeded() {
		r.unaccepted = append(r.unaccepted, fmt.Sprintf("%s (status %d, errored=%v)", subject, w.Status, w.Errored))
	}
	r.rows = append(r.rows, shape.Action+" "+subject)
}

// authorize is the gate's decision hook. It records the refusal and returns
// false, which is the production posture: nothing is ever authorized.
func (r *wireRecords) authorize(_ context.Context, req ghwrite.Request, ref ghwrite.Refusal) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	subject := restSubject(req.Method, req.Path)
	if req.GraphQL != nil {
		subject = graphQLSubject(*req.GraphQL)
		r.documents = append(r.documents, *req.GraphQL)
	}
	r.rows = append(r.rows, "refused("+ref.Reason+") "+subject)
	return false
}

func (r *wireRecords) snapshot() ([]string, []string, []ghwrite.GraphQLFacts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.rows...), append([]string(nil), r.unaccepted...),
		append([]ghwrite.GraphQLFacts(nil), r.documents...)
}

// keep folds one command's documents into the battery-wide collection the
// measurements below are taken over.
func (r *wireRecords) keep(documents []ghwrite.GraphQLFacts, sent [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.documents = append(r.documents, documents...)
	r.sent = append(r.sent, sent...)
}

// restSubject renders a REST write the way the expectations spell it: the
// upstream path with the GHE prefix gh emits stripped, so an audit row's path
// and a refusal's read alike (the gate strips it, the audit does not).
func restSubject(method, path string) string {
	return method + " " + strings.TrimPrefix(path, restPrefix)
}

// graphQLSubject renders a GraphQL write by the mutation it selected, or — for
// a document that named anything other than one known field — by what little
// was established about it, which is what an expectation would have to match.
func graphQLSubject(facts ghwrite.GraphQLFacts) string {
	if m := facts.Mutation(); m != "" {
		return m
	}
	if facts.Unreadable != "" {
		return "unreadable:" + facts.Unreadable
	}
	return "fields[" + strings.Join(facts.Fields, ",") + "]"
}

// wireCase is one enumerated command and the records it must leave.
type wireCase struct {
	name string
	args []string
	want []wireExpect
}

// repoFlag is the coordinate every command in the battery is given explicitly,
// so no case depends on repository inference from a worktree.
var repoFlag = []string{"-R", confOwner + "/" + confRepo}

func ghArgs(args ...string) []string { return append(args, repoFlag...) }

// porcelainWriteCases enumerates the pinned porcelain's write commands.
//
// The list is the coverage sweep's, and it is deliberately exhaustive rather
// than illustrative: a gate that proves the table against a handful of examples
// proves nothing about the shapes nobody thought to include. Each case names
// the wire form it expects, so a gh upgrade that moves a command to a different
// mutation fails here rather than silently landing in the fallback.
//
// The numbers are the fixture catalog's: a command whose target must be closed,
// draft, locked, merged or unmergeable is pointed at the object in that state.
func porcelainWriteCases() []wireCase {
	return []wireCase{
		// The pull request itself.
		{
			name: "pr create",
			args: ghArgs("pr", "create", "--head", "feature", "--base", "main", "--title", "T", "--body", "B"),
			want: []wireExpect{{mutation: "createPullRequest", action: domain.ActionPRCreated}},
		},
		{
			name: "pr comment",
			args: ghArgs("pr", "comment", "42", "--body", "a reply"),
			want: []wireExpect{{mutation: "addComment", action: domain.ActionCommentPosted}},
		},
		{
			name: "pr comment --edit-last",
			args: ghArgs("pr", "comment", "42", "--edit-last", "--body", "revised"),
			want: []wireExpect{{mutation: "updateIssueComment", action: domain.ActionCommentEdited}},
		},
		{
			name: "pr comment --delete-last",
			args: ghArgs("pr", "comment", "42", "--delete-last", "--yes"),
			want: []wireExpect{{mutation: "deleteIssueComment", action: domain.ActionCommentDeleted}},
		},
		{
			name: "pr close",
			args: ghArgs("pr", "close", "42"),
			want: []wireExpect{{mutation: "closePullRequest", action: domain.ActionPRClosed}},
		},
		{
			name: "pr reopen",
			args: ghArgs("pr", "reopen", "43"),
			want: []wireExpect{{mutation: "reopenPullRequest", action: domain.ActionPRReopened}},
		},
		{
			name: "pr edit",
			args: ghArgs("pr", "edit", "42", "--title", "X"),
			want: []wireExpect{{mutation: "updatePullRequest", action: domain.ActionPREdited}},
		},
		{
			// The label rides its own mutation rather than the edit's input, so
			// this is one act and one row — not the edit's.
			name: "pr edit --add-label",
			args: ghArgs("pr", "edit", "42", "--add-label", confLabelName),
			want: []wireExpect{{mutation: "addLabelsToLabelable", action: domain.ActionLabelAdded}},
		},
		{
			name: "pr edit --remove-label",
			args: ghArgs("pr", "edit", "42", "--remove-label", confLabelName),
			want: []wireExpect{{mutation: "removeLabelsFromLabelable", action: domain.ActionLabelRemoved}},
		},
		{
			// An assignee, unlike a label, rides the edit mutation's own input,
			// so the act the log can name is the edit.
			name: "pr edit --add-assignee",
			args: ghArgs("pr", "edit", "42", "--add-assignee", "someone"),
			want: []wireExpect{{mutation: "updatePullRequest", action: domain.ActionPREdited}},
		},
		{
			// The one porcelain command that sends two requests: a REST reviewer
			// request and the GraphQL edit. Both must land a row, which is the
			// case that first showed a single command can be half-audited.
			name: "pr edit --add-reviewer",
			args: ghArgs("pr", "edit", "42", "--add-reviewer", "someone"),
			want: []wireExpect{
				{method: "POST", path: "/repos/octo/repo/pulls/42/requested_reviewers",
					action: domain.ActionReviewRequested},
				{mutation: "updatePullRequest", action: domain.ActionPREdited},
			},
		},
		{
			name: "pr ready",
			args: ghArgs("pr", "ready", "44"),
			want: []wireExpect{{mutation: "markPullRequestReadyForReview", action: domain.ActionPRMarkedReady}},
		},
		{
			name: "pr ready --undo",
			args: ghArgs("pr", "ready", "42", "--undo"),
			want: []wireExpect{{mutation: "convertPullRequestToDraft", action: domain.ActionPRConvertedToDraft}},
		},
		{
			name: "pr merge",
			args: ghArgs("pr", "merge", "42", "--merge"),
			want: []wireExpect{{mutation: "mergePullRequest", action: domain.ActionPRMerged}},
		},
		{
			// Against a pull request that is not immediately mergeable, which is
			// the only condition under which gh arms auto-merge instead of
			// merging. It is a distinct mutation, so the merge family's coverage
			// never implied this one's.
			name: "pr merge --auto",
			args: ghArgs("pr", "merge", "47", "--merge", "--auto"),
			want: []wireExpect{{mutation: "enablePullRequestAutoMerge", action: domain.ActionPRAutoMergeEnabled}},
		},
		{
			name: "pr merge --disable-auto",
			args: ghArgs("pr", "merge", "42", "--disable-auto"),
			want: []wireExpect{{mutation: "disablePullRequestAutoMerge", action: domain.ActionPRAutoMergeDisabled}},
		},
		{
			// Refused, not classified: the run's own review verbs dominate this
			// spelling, so the channel does not carry it.
			name: "pr review",
			args: ghArgs("pr", "review", "42", "--comment", "--body", "looks fine"),
			want: []wireExpect{{mutation: "addPullRequestReview", refusal: ghwrite.GateReasonReview}},
		},
		{
			name: "pr lock",
			args: ghArgs("pr", "lock", "42"),
			want: []wireExpect{{mutation: "lockLockable", action: domain.ActionConversationLocked}},
		},
		{
			name: "pr unlock",
			args: ghArgs("pr", "unlock", "45"),
			want: []wireExpect{{mutation: "unlockLockable", action: domain.ActionConversationUnlocked}},
		},
		{
			// A revert opens a pull request, and the row resolves to the one it
			// opened rather than the one it undid.
			name: "pr revert",
			args: ghArgs("pr", "revert", "46", "--title", "Revert", "--body", "B"),
			want: []wireExpect{{mutation: "revertPullRequest", action: domain.ActionPRReverted}},
		},
		{
			name: "pr update-branch",
			args: ghArgs("pr", "update-branch", "42"),
			want: []wireExpect{{mutation: "updatePullRequestBranch", action: domain.ActionPRBranchUpdated}},
		},

		// The issue family.
		{
			name: "issue create",
			args: ghArgs("issue", "create", "--title", "T", "--body", "B"),
			want: []wireExpect{{mutation: "createIssue", action: domain.ActionIssueCreated}},
		},
		{
			name: "issue comment",
			args: ghArgs("issue", "comment", "7", "--body", "hi"),
			want: []wireExpect{{mutation: "addComment", action: domain.ActionCommentPosted}},
		},
		{
			name: "issue close",
			args: ghArgs("issue", "close", "7"),
			want: []wireExpect{{mutation: "closeIssue", action: domain.ActionIssueClosed}},
		},
		{
			name: "issue reopen",
			args: ghArgs("issue", "reopen", "8"),
			want: []wireExpect{{mutation: "reopenIssue", action: domain.ActionIssueReopened}},
		},
		{
			name: "issue delete",
			args: ghArgs("issue", "delete", "7", "--yes"),
			want: []wireExpect{{mutation: "deleteIssue", action: domain.ActionIssueDeleted}},
		},
		{
			name: "issue edit",
			args: ghArgs("issue", "edit", "7", "--title", "X"),
			want: []wireExpect{{mutation: "updateIssue", action: domain.ActionIssueUpdated}},
		},
		{
			name: "issue pin",
			args: ghArgs("issue", "pin", "7"),
			want: []wireExpect{{mutation: "pinIssue", action: domain.ActionIssuePinned}},
		},
		{
			name: "issue unpin",
			args: ghArgs("issue", "unpin", "10"),
			want: []wireExpect{{mutation: "unpinIssue", action: domain.ActionIssueUnpinned}},
		},
		{
			name: "issue transfer",
			args: ghArgs("issue", "transfer", "7", confOwner+"/other"),
			want: []wireExpect{{mutation: "transferIssue", action: domain.ActionIssueTransferred}},
		},
		{
			name: "issue develop",
			args: ghArgs("issue", "develop", "7"),
			want: []wireExpect{{mutation: "createLinkedBranch", action: domain.ActionLinkedBranchCreated}},
		},
		{
			name: "issue lock",
			args: ghArgs("issue", "lock", "7"),
			want: []wireExpect{{mutation: "lockLockable", action: domain.ActionConversationLocked}},
		},
		{
			name: "issue unlock",
			args: ghArgs("issue", "unlock", "9"),
			want: []wireExpect{{mutation: "unlockLockable", action: domain.ActionConversationUnlocked}},
		},

		// The repository's label SET, which is a different act from applying a
		// label to an issue: deleting a definition strips it from everything
		// carrying it.
		{
			name: "label create",
			args: ghArgs("label", "create", "triage", "--description", "d"),
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/labels", action: domain.ActionLabelDefined}},
		},
		{
			name: "label edit",
			args: ghArgs("label", "edit", confLabelName, "--description", "d2"),
			want: []wireExpect{{method: "PATCH", path: "/repos/octo/repo/labels/" + confLabelName,
				action: domain.ActionLabelDefinitionEdited}},
		},
		{
			name: "label delete",
			args: ghArgs("label", "delete", confLabelName, "--yes"),
			want: []wireExpect{{method: "DELETE", path: "/repos/octo/repo/labels/" + confLabelName,
				action: domain.ActionLabelDefinitionDeleted}},
		},

		// Releases. Only the two whose requests this channel is in front of —
		// see the asset commands' own test for the ones that are not.
		{
			name: "release create",
			args: ghArgs("release", "create", "v2.0.0", "--notes", "n"),
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/releases", action: domain.ActionReleaseCreated}},
		},
		{
			name: "release edit",
			args: ghArgs("release", "edit", confReleaseTag, "--draft=false"),
			want: []wireExpect{{method: "PATCH", path: "/repos/octo/repo/releases/9", action: domain.ActionReleaseEdited}},
		},

		// The repository. Its four creation spellings are refused rather than
		// classified; the settings verbs are ordinary writes.
		{
			name: "repo create",
			args: []string{"repo", "create", confOwner + "/newrepo", "--public"},
			want: []wireExpect{{mutation: "createRepository", refusal: ghwrite.GateReasonRepoCreate}},
		},
		{
			name: "repo create --template",
			args: []string{"repo", "create", confOwner + "/newrepo", "--template", confOwner + "/" + confRepo, "--public"},
			want: []wireExpect{{mutation: "cloneTemplateRepository", refusal: ghwrite.GateReasonRepoCreate}},
		},
		{
			// The REST arm, which gh takes when the new repository needs
			// initializing. It posts outside /repos/{owner}/{repo}/, so the
			// classifier cannot name it and the gate matches it by path instead.
			name: "repo create --add-readme",
			args: []string{"repo", "create", confOwner + "/newrepo", "--public", "--add-readme"},
			want: []wireExpect{{method: "POST", path: "/user/repos", refusal: ghwrite.GateReasonRepoCreate}},
		},
		{
			name: "repo create --add-readme (org)",
			args: []string{"repo", "create", confOrg + "/newrepo", "--public", "--add-readme"},
			want: []wireExpect{{method: "POST", path: "/orgs/" + confOrg + "/repos", refusal: ghwrite.GateReasonRepoCreate}},
		},
		{
			name: "repo edit",
			args: []string{"repo", "edit", confOwner + "/" + confRepo, "--description", "d2"},
			want: []wireExpect{{method: "PATCH", path: "/repos/octo/repo", action: domain.ActionRepoEdited}},
		},
		{
			// Topics have their own endpoint but are the same act, so the row
			// reads as an edit and a reader filtering for edits finds it.
			name: "repo edit --add-topic",
			args: []string{"repo", "edit", confOwner + "/" + confRepo, "--add-topic", "t1"},
			want: []wireExpect{{method: "PUT", path: "/repos/octo/repo/topics", action: domain.ActionRepoEdited}},
		},
		{
			name: "repo archive",
			args: []string{"repo", "archive", confOwner + "/" + confRepo, "--yes"},
			want: []wireExpect{{mutation: "archiveRepository", action: domain.ActionRepoArchived}},
		},
		{
			name: "repo unarchive",
			args: []string{"repo", "unarchive", confOwner + "/" + confArchivedRepo, "--yes"},
			want: []wireExpect{{mutation: "unarchiveRepository", action: domain.ActionRepoUnarchived}},
		},
		{
			name: "repo fork",
			args: []string{"repo", "fork", confOwner + "/" + confRepo, "--clone=false"},
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/forks", action: domain.ActionRepoForked}},
		},
		{
			name: "repo delete",
			args: []string{"repo", "delete", confOwner + "/" + confRepo, "--yes"},
			want: []wireExpect{{method: "DELETE", path: "/repos/octo/repo", action: domain.ActionRepoDeleted}},
		},
	}
}

// handWrittenWriteCases are the `gh api` spellings an agent reaches for when
// the porcelain has no verb, or when it is writing raw calls of its own.
//
// Two jobs. Some of these are the only way a table entry is reachable at all —
// dismissing a review, deleting a release, reacting to anything — so without
// them the enumeration would silently skip acts the table names, which is the
// drift this gate exists to catch. The rest are the raw shapes the epic's
// original finding came in as: a reply posted straight to the REST endpoint,
// which landed on GitHub and left nothing recognizable behind.
func handWrittenWriteCases() []wireCase {
	const prNode = "PR_open"
	graphql := func(document string) []string {
		return []string{"api", "graphql", "-f", "query=" + document}
	}
	return []wireCase{
		{
			name: "api reply to a review thread",
			args: []string{"api", "--method", "POST", "repos/octo/repo/pulls/42/comments/6/replies", "-f", "body=ack"},
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/pulls/42/comments/6/replies",
				action: domain.ActionCommentPosted}},
		},
		{
			name: "api comment on an issue",
			args: []string{"api", "--method", "POST", "repos/octo/repo/issues/7/comments", "-f", "body=hi"},
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/issues/7/comments",
				action: domain.ActionCommentPosted}},
		},
		{
			name: "api edit a comment",
			args: []string{"api", "--method", "PATCH", "repos/octo/repo/issues/comments/5", "-f", "body=revised"},
			want: []wireExpect{{method: "PATCH", path: "/repos/octo/repo/issues/comments/5",
				action: domain.ActionCommentEdited}},
		},
		{
			name: "api delete a review comment",
			args: []string{"api", "--method", "DELETE", "repos/octo/repo/pulls/comments/6"},
			want: []wireExpect{{method: "DELETE", path: "/repos/octo/repo/pulls/comments/6",
				action: domain.ActionReviewCommentDeleted}},
		},
		{
			// No porcelain path reaches this: there is no `gh pr review
			// --dismiss`, and `gh pr review` itself is refused.
			name: "api dismiss a review",
			args: []string{"api", "--method", "PUT", "repos/octo/repo/pulls/42/reviews/3/dismissals",
				"-f", "message=stale", "-f", "event=DISMISS"},
			want: []wireExpect{{method: "PUT", path: "/repos/octo/repo/pulls/42/reviews/3/dismissals",
				action: domain.ActionReviewDismissed}},
		},
		{
			name: "api merge over REST",
			args: []string{"api", "--method", "PUT", "repos/octo/repo/pulls/42/merge", "-f", "merge_method=merge"},
			want: []wireExpect{{method: "PUT", path: "/repos/octo/repo/pulls/42/merge", action: domain.ActionPRMerged}},
		},
		{
			// The porcelain's own delete follows the release payload's absolute
			// url and leaves the channel; this spelling is built from the base
			// and is the one that arrives here.
			name: "api delete a release",
			args: []string{"api", "--method", "DELETE", "repos/octo/repo/releases/9"},
			want: []wireExpect{{method: "DELETE", path: "/repos/octo/repo/releases/9",
				action: domain.ActionReleaseDeleted}},
		},
		{
			name: "api dispatch a workflow",
			args: []string{"api", "--method", "POST", "repos/octo/repo/actions/workflows/ci.yml/dispatches",
				"-f", "ref=main"},
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/actions/workflows/ci.yml/dispatches",
				action: domain.ActionWorkflowDispatched}},
		},
		{
			name: "api react over REST",
			args: []string{"api", "--method", "POST", "repos/octo/repo/issues/7/reactions", "-f", "content=+1"},
			want: []wireExpect{{method: "POST", path: "/repos/octo/repo/issues/7/reactions",
				action: domain.ActionReactionAdded}},
		},
		{
			// The REST collection a repository is posted into, spelled by hand
			// rather than reached through `repo create`. It is refused by path,
			// since there are no repository coordinates in it to classify.
			name: "api create a repository over REST",
			args: []string{"api", "--method", "POST", "user/repos", "-f", "name=newrepo"},
			want: []wireExpect{{method: "POST", path: "/user/repos", refusal: ghwrite.GateReasonRepoCreate}},
		},

		// The GraphQL half. These are written as anonymous inline documents,
		// which is what an agent composing a raw call actually sends — and the
		// shape the porcelain never produces.
		{
			name: "api graphql react",
			args: graphql(`mutation{addReaction(input:{subjectId:"` + prNode + `",content:THUMBS_UP}){clientMutationId}}`),
			want: []wireExpect{{mutation: "addReaction", action: domain.ActionReactionAdded}},
		},
		{
			name: "api graphql unreact",
			args: graphql(`mutation{removeReaction(input:{subjectId:"` + prNode + `",content:THUMBS_UP}){clientMutationId}}`),
			want: []wireExpect{{mutation: "removeReaction", action: domain.ActionReactionRemoved}},
		},
		{
			// gh reaches this one only after creating a repository, which the
			// gate makes impossible, so by hand is the only way it arrives.
			name: "api graphql update a repository",
			args: graphql(`mutation{updateRepository(input:{repositoryId:"R_repo",description:"d"}){clientMutationId}}`),
			want: []wireExpect{{mutation: "updateRepository", action: domain.ActionRepoEdited}},
		},
		{
			// The review mutation the porcelain never sends. It names the same
			// act, so it meets the same refusal.
			name: "api graphql submit a review",
			args: graphql(`mutation{submitPullRequestReview(input:{pullRequestId:"` + prNode +
				`",event:APPROVE}){clientMutationId}}`),
			want: []wireExpect{{mutation: "submitPullRequestReview", refusal: ghwrite.GateReasonReview}},
		},
		{
			// Refused by name rather than by act: it stages a thread on a review
			// that has not been submitted, which no audit verb describes.
			name: "api graphql stage a review thread",
			args: graphql(`mutation{addPullRequestReviewThread(input:{pullRequestId:"` + prNode +
				`",path:"a.go",body:"x"}){clientMutationId}}`),
			want: []wireExpect{{mutation: "addPullRequestReviewThread", refusal: ghwrite.GateReasonReview}},
		},
	}
}

// driveGH runs one command against a fresh injector standing in front of the
// fixture, with both record sinks wired, and returns what gh printed and how it
// exited. Exit status is part of the evidence: a refusal the agent's tool
// reports as success is a gate the run has no reason to believe.
func driveGH(t *testing.T, upstream *fakeUpstream, records *wireRecords, args []string) ([]byte, error) {
	t.Helper()
	env := ghEnvWithGate(t, upstream.server.URL, nil, records.audit, records.authorize)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghBinary(t), args...)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	return cmd.CombinedOutput()
}

// runWireCase drives one command and asserts the records it left.
func runWireCase(t *testing.T, tc wireCase, shared *wireRecords) {
	t.Helper()
	upstream := newFakeUpstream(t)
	var records wireRecords
	out, runErr := driveGH(t, upstream, &records, tc.args)

	got, unaccepted, documents := records.snapshot()
	want := make([]string, 0, len(tc.want))
	refused := false
	for _, e := range tc.want {
		want = append(want, e.String())
		refused = refused || e.refusal != ""
	}
	sort.Strings(got)
	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Errorf("gh %s left %v, want %v\ngh output:\n%s\nrequests:\n%s",
			strings.Join(tc.args, " "), got, want, out, fmtRequests(upstream.requests()))
	}
	for _, row := range got {
		if strings.HasPrefix(row, "UNCLASSIFIED ") {
			t.Errorf("gh %s left an unclassified row (%s) — an enumerated command must resolve to an act or "+
				"be refused, so either the classification table needs this shape or the enumeration is wrong",
				strings.Join(tc.args, " "), strings.TrimPrefix(row, "UNCLASSIFIED "))
		}
	}
	if len(unaccepted) > 0 {
		t.Errorf("gh %s recorded writes the fixture did not accept (%v) — the row is right but the command "+
			"never performed its act, so this case is proving less than it looks",
			strings.Join(tc.args, " "), unaccepted)
	}
	// A refusal must reach the agent as a failure. If gh exits cleanly through
	// a refusal, the run's model has no reason to believe the act did not
	// happen, which is the failure mode a silent gate produces.
	if refused && runErr == nil {
		t.Errorf("gh %s exited 0 through a refusal, want a failure the agent can see\n%s",
			strings.Join(tc.args, " "), out)
	}
	if !refused && runErr != nil {
		t.Errorf("gh %s failed: %v\n%s\nrequests:\n%s", strings.Join(tc.args, " "), runErr, out,
			fmtRequests(upstream.requests()))
	}

	if shared != nil {
		var sent [][]byte
		for _, r := range upstream.requests() {
			if r.Query != "" {
				sent = append(sent, r.Body)
			}
		}
		shared.keep(documents, sent)
	}
}

func fmtRequests(requests []upstreamRequest) string {
	lines := make([]string, 0, len(requests))
	for _, r := range requests {
		lines = append(lines, "  "+fmtRequest(r))
	}
	return strings.Join(lines, "\n")
}

// TestGHConformance_EveryPorcelainWriteClassifiesOrRefuses is the gate. Each
// enumerated command is driven against the fake upstream through a real
// injector, and must leave exactly the rows named — a semantic act, or a
// refusal. There is no third outcome for known traffic.
func TestGHConformance_EveryPorcelainWriteClassifiesOrRefuses(t *testing.T) {
	ghBinary(t)
	var battery wireRecords
	for _, tc := range porcelainWriteCases() {
		t.Run(tc.name, func(t *testing.T) { runWireCase(t, tc, &battery) })
	}
	assertDocumentsStayReadable(t, &battery)
}

// TestGHConformance_HandWrittenWritesClassifyOrRefuse is the same gate for the
// raw `gh api` surface, which is where the acts no porcelain command reaches
// arrive from.
func TestGHConformance_HandWrittenWritesClassifyOrRefuse(t *testing.T) {
	ghBinary(t)
	for _, tc := range handWrittenWriteCases() {
		// No document collection: the measurements below are claims about what
		// the pinned binary composes, and these documents were composed here.
		t.Run(tc.name, func(t *testing.T) { runWireCase(t, tc, nil) })
	}
}

// maxPinnedDocument is the headroom claim the refusal policy rests on. The
// injector refuses a GraphQL request it could not read, on the ground that no
// real client comes near the buffering cap; the largest document the pinned gh
// sends is measured in hundreds of bytes, and this bound is deliberately far
// below the cap and far above the measurement. A pin bump that crossed it would
// mean the band is no longer empty, which is a decision to re-take rather than a
// number to raise.
const maxPinnedDocument = 8 << 10

// assertDocumentsStayReadable pins the two properties the refuse-the-unreadable
// decision was taken on: every mutation the pinned gh sends resolves to exactly
// one named field, and none of them is anywhere near the cap. If a gh release
// starts spreading a fragment into a mutation's top level or packing two
// operations into a document, the refusal band stops being empty and this fails
// rather than letting real traffic start meeting a 403.
func assertDocumentsStayReadable(t *testing.T, records *wireRecords) {
	t.Helper()
	records.mu.Lock()
	documents := append([]ghwrite.GraphQLFacts(nil), records.documents...)
	sent := append([][]byte(nil), records.sent...)
	records.mu.Unlock()

	if len(documents) == 0 {
		t.Fatal("no GraphQL documents were read — the battery drove nothing, so its assertions proved nothing")
	}
	for _, facts := range documents {
		if facts.Unreadable != "" {
			t.Errorf("the pinned gh sent a document the injector could not read (%s, operation %q) — "+
				"the refusal band is meant to contain no legitimate traffic", facts.Unreadable, facts.Operation)
		}
		if len(facts.Fields) != 1 {
			t.Errorf("operation %q selected %d top-level fields %v, want exactly one — a document performing "+
				"several acts cannot be described by one audit row", facts.Operation, len(facts.Fields), facts.Fields)
		}
	}

	// The size and spread measurements are taken over the bodies that were
	// forwarded, which is every document except the handful the gate refused
	// before they left; those are covered by the facts above, which is the half
	// the policy actually turns on.
	largest, largestMutation := 0, 0
	for _, body := range sent {
		largest = max(largest, len(body))
		// Screened exactly as production screens it, so the split below is the
		// one the injector actually pays: a document on the read side of this
		// call never reaches the parser.
		if _, isWrite := ghwrite.ParseGraphQLRequest(body); !isWrite {
			continue
		}
		largestMutation = max(largestMutation, len(body))
		if bytes.Contains(body, []byte("...")) {
			t.Errorf("a mutation document the pinned gh sends now spreads a fragment (%s) — the refusal band "+
				"was measured empty on the premise that none does, and that premise needs re-taking",
				strings.Join(strings.Fields(string(body)), " "))
		}
	}
	t.Logf("largest document the pinned gh sent: %d bytes (%d for a mutation), against a %d byte buffering cap",
		largest, largestMutation, maxRequestBody)
	if largest > maxPinnedDocument {
		t.Errorf("the pinned gh now sends a %d byte document, past this file's %d byte bound — the headroom the "+
			"refuse-the-unreadable decision assumes is no longer what was measured", largest, maxPinnedDocument)
	}
}

// TestGHConformance_EveryClassifiedMutationIsDriven closes the enumeration
// against the table itself. A mutation the table names but nothing drives is
// exactly the drift this gate exists to catch — it would keep passing while
// asserting nothing about that entry — so every name in the table must be
// reached by some spelling above, porcelain or hand-written.
//
// The reverse direction is the battery's own job: a case whose mutation the
// table does not know leaves an unclassified row and fails there.
func TestGHConformance_EveryClassifiedMutationIsDriven(t *testing.T) {
	driven := map[string]bool{}
	for _, tc := range append(porcelainWriteCases(), handWrittenWriteCases()...) {
		for _, e := range tc.want {
			if e.mutation != "" {
				driven[e.mutation] = true
			}
		}
	}
	var missing []string
	for _, name := range ghwrite.KnownMutations() {
		if !driven[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the classification table names mutations nothing in this file drives: %v — add a case that "+
			"reaches each (a porcelain command where one exists, a `gh api` call where none does), or take the "+
			"entry out of the table", missing)
	}
}

// TestGHConformance_ReleaseAssetsNeverTransitTheChannel is the out-of-scope
// list, asserted rather than assumed.
//
// GH_HOST redirects the base URL, so this channel reaches only the requests gh
// BUILDS from that base. These three commands take an absolute url out of a
// response body and follow it — the upload endpoint, an asset's own url, the
// release's api url — which addresses GitHub directly and never this listener.
// They are unobservable here by construction, not merely unclassified, so
// enumerating them above would fail forever.
//
// The assertion is two-sided on purpose: the write must land on the off-channel
// host AND leave no record here. Asserting only the absence would keep passing
// if the command stopped writing at all, and this list has to stay honest if gh
// changes what it follows.
func TestGHConformance_ReleaseAssetsNeverTransitTheChannel(t *testing.T) {
	ghBinary(t)
	// A name the fixture's release does not already carry, so the upload is a
	// genuine one rather than a clobber gh refuses before it writes.
	fresh := filepath.Join(t.TempDir(), "fresh.txt")
	if err := os.WriteFile(fresh, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	for _, tc := range []struct {
		name       string
		args       []string
		wantMethod string
	}{
		{
			name:       "release upload",
			args:       ghArgs("release", "upload", confReleaseTag, fresh),
			wantMethod: http.MethodPost,
		},
		{
			name:       "release delete-asset",
			args:       ghArgs("release", "delete-asset", confReleaseTag, confAssetName, "--yes"),
			wantMethod: http.MethodDelete,
		},
		{
			// Measured into this list rather than assumed into it: gh deletes
			// through the release payload's own api url, so the porcelain's
			// delete escapes exactly as the asset commands do. The REST shape
			// keeps its place in the classification table because a
			// hand-written `gh api` delete is built from the base and does
			// arrive here — the enumeration above drives that spelling.
			name:       "release delete",
			args:       ghArgs("release", "delete", confReleaseTag, "--yes"),
			wantMethod: http.MethodDelete,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newFakeUpstream(t)
			var records wireRecords
			out, err := driveGH(t, upstream, &records, tc.args)
			if err != nil {
				t.Fatalf("gh %s failed: %v\n%s", strings.Join(tc.args, " "), err, out)
			}

			rows, _, _ := records.snapshot()
			if len(rows) != 0 {
				t.Errorf("gh %s left %v at the injector — if this command now transits the channel it belongs "+
					"in the enumeration instead of here", strings.Join(tc.args, " "), rows)
			}
			var landed []string
			for _, r := range upstream.offChannelRequests() {
				landed = append(landed, r.Method)
			}
			if !slices.Contains(landed, tc.wantMethod) {
				t.Errorf("gh %s sent %v off-channel, want a %s — the write this test claims is unobservable "+
					"did not happen at all, so its absence here proves nothing",
					strings.Join(tc.args, " "), landed, tc.wantMethod)
			}
		})
	}
}

// TestGHConformance_RepositoryCreationNeverArrivesUnclassified is the second
// shape this channel cannot name, and the reason it is a test rather than a
// note: the porcelain does not always create a repository through GraphQL, as
// was assumed before this was driven. Asked for a gitignore, a license or a
// readme, gh posts to /user/repos or /orgs/{org}/repos instead — outside the
// /repos/{owner}/{repo}/ anchor the classifier hangs off, so those two shapes
// genuinely cannot be named.
//
// They are not the fourth state, though, because the gate matches them by path:
// the enumeration above expects a refusal for each. What this test adds is the
// property behind that expectation — a repository creation never reaches the
// upstream, whichever of the four spellings it arrives in.
func TestGHConformance_RepositoryCreationNeverArrivesUnclassified(t *testing.T) {
	ghBinary(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"graphql", []string{"repo", "create", confOwner + "/newrepo", "--public"}},
		{"template", []string{"repo", "create", confOwner + "/newrepo",
			"--template", confOwner + "/" + confRepo, "--public"}},
		{"rest user", []string{"repo", "create", confOwner + "/newrepo", "--public", "--add-readme"}},
		{"rest org", []string{"repo", "create", confOrg + "/newrepo", "--public", "--add-readme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newFakeUpstream(t)
			var records wireRecords
			_, _ = driveGH(t, upstream, &records, tc.args)

			for _, r := range upstream.requests() {
				if r.Method == http.MethodPost && (strings.HasSuffix(r.Path, "/user/repos") ||
					strings.HasSuffix(r.Path, "/repos") && strings.Contains(r.Path, "/orgs/")) {
					t.Errorf("a repository creation reached the upstream (%s) — the gate is meant to refuse it "+
						"before the credential is resolved", fmtRequest(r))
				}
				if r.Operation != "" && strings.Contains(r.Query, "createRepository") {
					t.Errorf("a repository creation reached the upstream (%s)", fmtRequest(r))
				}
			}
		})
	}
}

// porcelainMutations are the mutation names the enumeration expects the
// porcelain to send, which is a claim about the binary and not about GitHub.
func porcelainMutations() []string {
	seen := map[string]bool{}
	var names []string
	for _, tc := range porcelainWriteCases() {
		for _, e := range tc.want {
			if e.mutation != "" && !seen[e.mutation] {
				seen[e.mutation] = true
				names = append(names, e.mutation)
			}
		}
	}
	sort.Strings(names)
	return names
}

// handWrittenOnlyMutations are the table entries no porcelain path emits. They
// are asserted ABSENT from the binary, which is what makes "hand-written only"
// a measured claim rather than a comment: a gh release that starts sending one
// of these gains a porcelain path the enumeration does not cover, and the
// enumeration has to grow before the pin moves.
var handWrittenOnlyMutations = []string{
	"submitPullRequestReview",
	"addPullRequestReviewThread",
	"addReaction",
	"removeReaction",
}

// TestGHConformance_BinaryMatchesPin ties this file to internal/ghpin the way
// the Dockerfile drift test does, and for the same reason: the table describes
// ONE binary's behaviour, so a pin bump has to re-drive it rather than inherit
// its conclusions. CI stages the pinned release, so a bump that lands without
// the table keeping up fails here — the version check catches a stale binary,
// and the name checks catch a release that moved a command to a mutation the
// enumeration does not expect.
//
// The name checks read the binary's own string literals, which needs no network
// and no run: gh spells every mutation it can send as a literal in its query
// documents, so a name that is absent cannot be sent.
func TestGHConformance_BinaryMatchesPin(t *testing.T) {
	binary := ghBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("gh --version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), ghpin.Version) {
		t.Fatalf("the binary under test reports %q, want the pinned %s — this file's expectations describe the "+
			"pinned release and nothing else", strings.TrimSpace(string(out)), ghpin.Version)
	}

	image, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read %s: %v", binary, err)
	}
	for _, name := range porcelainMutations() {
		if !bytes.Contains(image, []byte(name)) {
			t.Errorf("the pinned gh contains no %q literal, but the enumeration expects a command to send it — "+
				"the pin moved and that command now writes some other way", name)
		}
	}
	for _, name := range handWrittenOnlyMutations {
		if bytes.Contains(image, []byte(name)) {
			t.Errorf("the pinned gh contains a %q literal, so the porcelain may now reach an act the "+
				"enumeration only drives by hand — find the command that sends it and enumerate it", name)
		}
	}
}

// BenchmarkGraphQLCapturePinnedRead measures the one hot-path cost the
// envelope audit adds, against the documents the pinned gh actually sends.
//
// It is the sibling of BenchmarkGraphQLCapture, which screens a hand-written
// document of the right shape; this one screens the real thing, and the real
// thing is several times larger — a finder query drags a comments connection
// and a reviewer list behind it. Reads are what this number is for: every
// porcelain command runs one or more before it writes, and they never reach the
// parser, so the cost is a copy and a scan.
func BenchmarkGraphQLCapturePinnedRead(b *testing.B) {
	read, mutation := capturePinnedDocuments(b)
	srv := &Server{cfg: Config{ObserveWrite: func(context.Context, ObservedWrite) {}}}
	for _, tc := range []struct {
		name  string
		body  []byte
		write bool
	}{
		{name: "read", body: read},
		{name: "mutation", body: mutation, write: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				req, _ := http.NewRequest(http.MethodPost, "https://example.test"+graphqlPath,
					bytes.NewReader(tc.body))
				if got := srv.captureGraphQLWrite(req) != nil; got != tc.write {
					b.Fatalf("captured write = %v, want %v", got, tc.write)
				}
			}
		})
	}
}

// capturePinnedDocuments drives one read and one write against the fixture and
// returns the request bodies gh sent, so the benchmark screens documents nobody
// wrote by hand.
func capturePinnedDocuments(b *testing.B) (read, mutation []byte) {
	b.Helper()
	upstream := newFakeUpstream(b)
	env := ghEnvWithGate(b, upstream.server.URL, nil, nil, nil)
	for _, args := range [][]string{ghArgs("pr", "view", "42"), ghArgs("pr", "close", "42")} {
		runGH(b, env, args...)
	}
	for _, r := range upstream.requests() {
		if r.Query == "" {
			continue
		}
		if _, isWrite := ghwrite.ParseGraphQLRequest(r.Body); isWrite {
			mutation = r.Body
		} else if len(r.Body) > len(read) {
			read = r.Body
		}
	}
	if len(read) == 0 || len(mutation) == 0 {
		b.Fatal("no documents captured — the fixture answered nothing gh asked for")
	}
	return read, mutation
}
