package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// Getter is the one request an identity read makes: a GET of a REST path,
// answering the body, and a *HTTPError for a non-2xx response. *Client is one.
// An observer that holds a credential but no Client builds one with
// NewTokenGetter.
//
// The reads are REST, not GraphQL, because the exec verbs reach GitHub through
// the per-run REST credential proxy in multi mode, and a proxy client refuses
// GraphQL (see NewProxyClient).
type Getter interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

// Querier is the one request a node lookup makes (NodeSubject): a GraphQL
// query document with its variables, answering the response body, and an
// error for a non-2xx response or one with no data. *Client is one. An
// observer that holds a credential but no Client builds one with
// NewTokenQuerier.
//
// REST has no read that takes a node id, so this one is GraphQL, and only an
// observer that reaches GitHub directly can make it: the gh channel's
// injector, which holds the run's credential. The exec verbs' per-run REST
// proxy refuses GraphQL, and no exec verb reads by node id.
type Querier interface {
	PostGraphQL(ctx context.Context, query string, variables map[string]any) ([]byte, error)
}

// IdentityReader reads what GitHub calls the objects an act names, and caches
// every answer about a repository, an object, a comment or a node id, so the
// reader an observer holds for a run reads each of them once. What a branch
// has open is not cached (OpenPullForBranch). A number names one object for
// the life of a repository, a comment sits on one object and a node id names
// one object for their lives, and an id names one repository for the life of
// its name, which a run does not outlast in practice. A nil reader reads
// without caching. Safe for concurrent use.
type IdentityReader struct {
	mu    sync.Mutex
	cache map[string]domain.GitHubIdentity // repoCacheKey and objectCacheKey
	// subjects is the number of the pull request or issue each comment read
	// is on, by commentCacheKey.
	subjects map[string]int
	// nodes is what each node id read names, by the id itself.
	nodes map[string]NodeSubject
}

// NewIdentityReader returns a reader with an empty cache.
func NewIdentityReader() *IdentityReader {
	return &IdentityReader{}
}

// Object reads the pull request or issue numbered number in owner/repo: its
// node id, which of the two it is, and its repository's id and current name.
// GitHub numbers pull requests and issues in one sequence, so the number
// names exactly one of them. The pull request is asked for first, since that
// is what most acts name, and answers the repository in the same response; a
// 404 there asks for the issue, whose repository is read (or taken from the
// cache) beside it. An answer for another object than the one asked for is an
// error, never a value, and so is any failed request.
func (r *IdentityReader) Object(ctx context.Context, g Getter, owner, repo string, number int) (domain.GitHubIdentity, error) {
	if owner == "" || repo == "" || number <= 0 {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s/%s#%d names no pull request or issue", owner, repo, number)
	}
	key := objectCacheKey(owner, repo, number)
	if out, ok := r.cached(key); ok {
		return out, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	body, err := g.Get(ctx, path)
	if err == nil {
		out, err := r.pullIdentity(owner, repo, number, body)
		if err == nil {
			r.remember(key, out)
		}
		return out, err
	}
	if !IsHTTP404(err) {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: read %s/%s#%d: %w", owner, repo, number, err)
	}
	issue, err := r.Issue(ctx, g, owner, repo, number)
	if err != nil {
		return domain.GitHubIdentity{}, err
	}
	out, err := r.Repository(ctx, g, owner, repo)
	if err != nil {
		return domain.GitHubIdentity{}, err
	}
	out.NodeID, out.Kind = issue.NodeID, issue.Kind
	r.remember(key, out)
	return out, nil
}

// Issue reads the issue numbered number in owner/repo: its node id and kind
// issue, without its repository. It is Object's second read, for a caller
// whose pull request read of the number has already answered 404. A number
// GitHub answers as a pull request is an error, as is any failed request.
func (r *IdentityReader) Issue(ctx context.Context, g Getter, owner, repo string, number int) (domain.GitHubIdentity, error) {
	if owner == "" || repo == "" || number <= 0 {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s/%s#%d names no issue", owner, repo, number)
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	body, err := g.Get(ctx, path)
	if err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: read %s/%s#%d: %w", owner, repo, number, err)
	}
	var issue struct {
		Number      int             `json:"number"`
		NodeID      string          `json:"node_id"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &issue); err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: parse %s: %w", path, err)
	}
	if issue.Number != number || issue.NodeID == "" {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s answered #%d with node id %q", path, issue.Number, issue.NodeID)
	}
	if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
		// The pulls endpoint said there is no such pull request; this one says
		// it is one. Neither answer can be recorded.
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s/%s#%d is a pull request the pulls endpoint does not answer for", owner, repo, number)
	}
	return domain.GitHubIdentity{NodeID: issue.NodeID, Kind: domain.EntityKindIssue}, nil
}

// pullIdentity reads a pulls/{number} response. Its base repository is the
// repository the request addressed, under the name GitHub gives it now, so
// it seeds the repository cache too.
func (r *IdentityReader) pullIdentity(owner, repo string, number int, body []byte) (domain.GitHubIdentity, error) {
	var pr struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
		Base   struct {
			Repo repoIdentityJSON `json:"repo"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: parse %s/%s#%d: %w", owner, repo, number, err)
	}
	if pr.Number != number || pr.NodeID == "" {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s/%s#%d answered #%d with node id %q", owner, repo, number, pr.Number, pr.NodeID)
	}
	out, err := pr.Base.Repo.identity()
	if err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s/%s#%d: %w", owner, repo, number, err)
	}
	r.remember(repoCacheKey(owner, repo), out)
	out.NodeID, out.Kind = pr.NodeID, domain.EntityKindPullRequest
	return out, nil
}

// BranchPull is the pull request OpenPullForBranch found open on a branch: its
// number, and what GitHub calls it, as Object reads it. The zero value is the
// answer for a branch with no pull request open on it, or with several.
type BranchPull struct {
	Number   int
	Identity domain.GitHubIdentity
}

// branchPullsPage is how many pull requests OpenPullForBranch asks for: one
// more than the one it can name, which is enough to tell several from one.
const branchPullsPage = 2

// OpenPullForBranch reads the pull request open on branch in owner/repo, from
// GitHub's list of the repository's open pull requests whose head is that
// branch of the repository itself (the head filter owner:branch, with the
// repository's owner), so a fork's branch of the same name is not looked at.
// It answers the one pull request open there, and the zero BranchPull when
// none or several are. The answer is not cached, since a branch gains and
// loses pull requests; the pull request it names, and its repository, are, as
// Object and Repository would have read them. An answer naming another branch,
// or a pull request with no number or node id, is an error, and so is any
// failed request.
func (r *IdentityReader) OpenPullForBranch(ctx context.Context, g Getter, owner, repo, branch string) (BranchPull, error) {
	if owner == "" || repo == "" || branch == "" {
		return BranchPull{}, fmt.Errorf("github identity: %q/%q branch %q names no branch", owner, repo, branch)
	}
	query := url.Values{
		"head":     {owner + ":" + branch},
		"state":    {"open"},
		"per_page": {strconv.Itoa(branchPullsPage)},
	}
	body, err := g.Get(ctx, fmt.Sprintf("/repos/%s/%s/pulls?%s", owner, repo, query.Encode()))
	if err != nil {
		return BranchPull{}, fmt.Errorf("github identity: look up %s/%s branch %q: %w", owner, repo, branch, err)
	}
	var pulls []struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Repo repoIdentityJSON `json:"repo"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &pulls); err != nil {
		return BranchPull{}, fmt.Errorf("github identity: parse %s/%s branch %q: %w", owner, repo, branch, err)
	}
	for _, pr := range pulls {
		if pr.Head.Ref != branch {
			return BranchPull{}, fmt.Errorf("github identity: %s/%s branch %q answered #%d from branch %q", owner, repo, branch, pr.Number, pr.Head.Ref)
		}
	}
	if len(pulls) != 1 {
		return BranchPull{}, nil
	}
	pr := pulls[0]
	if pr.Number <= 0 || pr.NodeID == "" {
		return BranchPull{}, fmt.Errorf("github identity: %s/%s branch %q answered #%d with node id %q", owner, repo, branch, pr.Number, pr.NodeID)
	}
	out, err := pr.Base.Repo.identity()
	if err != nil {
		return BranchPull{}, fmt.Errorf("github identity: %s/%s branch %q: %w", owner, repo, branch, err)
	}
	r.remember(repoCacheKey(owner, repo), out)
	out.NodeID, out.Kind = pr.NodeID, domain.EntityKindPullRequest
	r.remember(objectCacheKey(owner, repo, pr.Number), out)
	return BranchPull{Number: pr.Number, Identity: out}, nil
}

// Repository reads owner/repo's GitHub id and current name, from the cache
// when this reader has read it before. GitHub follows a renamed repository's
// old name to it, so an old name answers the repository's id and the name it
// has now. A failed read is an error, and is not cached.
func (r *IdentityReader) Repository(ctx context.Context, g Getter, owner, repo string) (domain.GitHubIdentity, error) {
	if owner == "" || repo == "" {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %q/%q names no repository", owner, repo)
	}
	if out, ok := r.cached(repoCacheKey(owner, repo)); ok {
		return out, nil
	}
	body, err := g.Get(ctx, fmt.Sprintf("/repos/%s/%s", owner, repo))
	if err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: read %s/%s: %w", owner, repo, err)
	}
	var meta repoIdentityJSON
	if err := json.Unmarshal(body, &meta); err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: parse %s/%s: %w", owner, repo, err)
	}
	out, err := meta.identity()
	if err != nil {
		return domain.GitHubIdentity{}, fmt.Errorf("github identity: %s/%s: %w", owner, repo, err)
	}
	r.remember(repoCacheKey(owner, repo), out)
	return out, nil
}

// commentSubjects is, for each of GitHub's comment numberings, the collection
// its comments are addressed under and the member of a comment that links the
// pull request or issue it is on: a review comment is on a pull request, and
// a conversation comment on an issue or a pull request, which GitHub links
// alike as an issue.
var commentSubjects = map[domain.GitHubCommentSpace]struct{ collection, member, subject string }{
	domain.GitHubReviewComment: {collection: "pulls", member: "pull_request_url", subject: "pulls"},
	domain.GitHubIssueComment:  {collection: "issues", member: "issue_url", subject: "issues"},
}

// CommentSubject reads the comment numbered id in space in owner/repo and
// answers the number of the pull request or issue it is on, from the API url
// of that object the comment carries. The number is the whole answer: Object
// reads what GitHub calls the object. It is cached, since a comment stays on
// one object for the life of its repository, so a reader reads each comment
// once. An answer for another comment than the one asked for, or one whose url
// locates no object, is an error, and so is any failed request, neither of
// which is cached.
func (r *IdentityReader) CommentSubject(ctx context.Context, g Getter, owner, repo string, space domain.GitHubCommentSpace, id int64) (int, error) {
	shape, ok := commentSubjects[space]
	if owner == "" || repo == "" || id <= 0 || !ok {
		return 0, fmt.Errorf("github identity: %s/%s %s comment %d names no comment", owner, repo, space, id)
	}
	key := commentCacheKey(owner, repo, space, id)
	if number, ok := r.cachedSubject(key); ok {
		return number, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/%s/comments/%d", owner, repo, shape.collection, id)
	body, err := g.Get(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("github identity: read %s: %w", path, err)
	}
	var comment map[string]json.RawMessage
	if err := json.Unmarshal(body, &comment); err != nil {
		return 0, fmt.Errorf("github identity: parse %s: %w", path, err)
	}
	var answered int64
	var link string
	if err := json.Unmarshal(comment["id"], &answered); err != nil || answered != id {
		return 0, fmt.Errorf("github identity: %s answered comment %s", path, comment["id"])
	}
	if err := json.Unmarshal(comment[shape.member], &link); err != nil {
		return 0, fmt.Errorf("github identity: %s carries no %s", path, shape.member)
	}
	number, ok := apiObjectNumber(link, shape.subject)
	if !ok {
		return 0, fmt.Errorf("github identity: %s names %s %q, which locates no object", path, shape.member, link)
	}
	r.rememberSubject(key, number)
	return number, nil
}

// apiObjectNumber reads the number out of the API url of a pull request or
// issue, …/repos/{owner}/{repo}/{collection}/{number}, on any host and under
// any prefix (a GHES host's /api/v3). The owner and repository are not read:
// the url spells the repository's name now, which is GitHub's to change.
func apiObjectNumber(link, collection string) (int, bool) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return 0, false
	}
	segs := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(segs) < 5 || segs[len(segs)-5] != "repos" || segs[len(segs)-2] != collection {
		return 0, false
	}
	number, err := strconv.Atoi(segs[len(segs)-1])
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

// NodeSubject is the pull request or issue a node id names, or the one the
// comment, review or review thread it names is on: its repository's owner and
// name as GitHub answers them now, and its number. Zero for a node id that
// names anything else.
type NodeSubject struct {
	Owner  string
	Repo   string
	Number int
}

// nodeSubjectQuery is the document NodeSubject sends. It names its operation
// and selects rateLimit { cost }, as every query document in this package
// does (recordQueryCost). A conversation comment's issue carries the number a
// pull request's conversation shares with it; its pull request is read first
// where the comment is on one. That field is aliased because a conversation
// comment's pullRequest is nullable and every other type's is not, and GraphQL
// refuses a document that selects two types under one response name.
const nodeSubjectQuery = `query NodeSubject($id: ID!) {
  node(id: $id) {
    __typename
    ... on PullRequest { number repository { nameWithOwner } }
    ... on Issue { number repository { nameWithOwner } }
    ... on IssueComment { commentPullRequest: pullRequest { number } issue { number } repository { nameWithOwner } }
    ... on PullRequestReviewComment { pullRequest { number } repository { nameWithOwner } }
    ... on PullRequestReview { pullRequest { number } repository { nameWithOwner } }
    ... on PullRequestReviewThread { pullRequest { number } repository { nameWithOwner } }
  }
  rateLimit { cost }
}`

// NodeSubject asks GitHub what the node id names, through q, and answers the
// pull request or issue it is, or the one the conversation comment, review
// comment, review or review thread it is sits on. The number is the whole answer, with the repository
// it is in: Object reads what GitHub calls the object. A node id that names
// anything else answers the zero NodeSubject. Both answers are cached, since a
// node id names one object for its life. A node GitHub answers null for (one
// that does not exist, or that q's credential cannot read), an answer missing
// what its type carries, and a failed request are errors, none of them cached.
func (r *IdentityReader) NodeSubject(ctx context.Context, q Querier, nodeID string) (NodeSubject, error) {
	if nodeID == "" {
		return NodeSubject{}, errors.New("github identity: an empty node id names nothing")
	}
	if out, ok := r.cachedNode(nodeID); ok {
		return out, nil
	}
	body, err := q.PostGraphQL(ctx, nodeSubjectQuery, map[string]any{"id": nodeID})
	if err != nil {
		return NodeSubject{}, fmt.Errorf("github identity: read node %s: %w", nodeID, err)
	}
	type numbered struct {
		Number int `json:"number"`
	}
	var resp struct {
		Data struct {
			Node *struct {
				Typename           string    `json:"__typename"`
				Number             int       `json:"number"`
				PullRequest        *numbered `json:"pullRequest"`
				CommentPullRequest *numbered `json:"commentPullRequest"`
				Issue              *numbered `json:"issue"`
				Repository         *struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return NodeSubject{}, fmt.Errorf("github identity: parse node %s: %w", nodeID, err)
	}
	node := resp.Data.Node
	if node == nil {
		return NodeSubject{}, fmt.Errorf("github identity: node %s answered null", nodeID)
	}
	var number int
	switch node.Typename {
	case "PullRequest", "Issue":
		number = node.Number
	case "IssueComment":
		switch {
		case node.CommentPullRequest != nil:
			number = node.CommentPullRequest.Number
		case node.Issue != nil:
			number = node.Issue.Number
		}
	case "PullRequestReviewComment", "PullRequestReview", "PullRequestReviewThread":
		if node.PullRequest != nil {
			number = node.PullRequest.Number
		}
	default:
		r.rememberNode(nodeID, NodeSubject{})
		return NodeSubject{}, nil
	}
	var owner, repo string
	if node.Repository != nil {
		owner, repo, _ = strings.Cut(node.Repository.NameWithOwner, "/")
	}
	if number <= 0 || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return NodeSubject{}, fmt.Errorf("github identity: node %s answered a %s with number %d in %q", nodeID, node.Typename, number, owner+"/"+repo)
	}
	out := NodeSubject{Owner: owner, Repo: repo, Number: number}
	r.rememberNode(nodeID, out)
	return out, nil
}

// repoIdentityJSON is the part of a repository object an identity read keeps.
type repoIdentityJSON struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

func (j repoIdentityJSON) identity() (domain.GitHubIdentity, error) {
	if j.ID <= 0 || j.FullName == "" {
		return domain.GitHubIdentity{}, fmt.Errorf("answered repository %q with id %d", j.FullName, j.ID)
	}
	return domain.GitHubIdentity{RepositoryID: strconv.FormatInt(j.ID, 10), Repository: j.FullName}, nil
}

func (r *IdentityReader) cached(key string) (domain.GitHubIdentity, bool) {
	if r == nil {
		return domain.GitHubIdentity{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out, ok := r.cache[key]
	return out, ok
}

func (r *IdentityReader) remember(key string, id domain.GitHubIdentity) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]domain.GitHubIdentity{}
	}
	r.cache[key] = id
}

func (r *IdentityReader) cachedSubject(key string) (int, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	number, ok := r.subjects[key]
	return number, ok
}

func (r *IdentityReader) rememberSubject(key string, number int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subjects == nil {
		r.subjects = map[string]int{}
	}
	r.subjects[key] = number
}

func (r *IdentityReader) cachedNode(nodeID string) (NodeSubject, bool) {
	if r == nil {
		return NodeSubject{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out, ok := r.nodes[nodeID]
	return out, ok
}

func (r *IdentityReader) rememberNode(nodeID string, out NodeSubject) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes == nil {
		r.nodes = map[string]NodeSubject{}
	}
	r.nodes[nodeID] = out
}

// repoCacheKey, objectCacheKey and commentCacheKey fold case: GitHub's names
// are case-insensitive. A node id is case-sensitive, and is its own key.
func repoCacheKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

func objectCacheKey(owner, repo string, number int) string {
	return repoCacheKey(owner, repo) + "#" + strconv.Itoa(number)
}

func commentCacheKey(owner, repo string, space domain.GitHubCommentSpace, id int64) string {
	return repoCacheKey(owner, repo) + "/comments/" + string(space) + "/" + strconv.FormatInt(id, 10)
}

// maxIdentityBody caps the body a TokenGetter reads. An object or repository
// response is a few tens of KB; the cap only guards against a pathological
// upstream.
const maxIdentityBody = 4 << 20

// NewTokenGetter is a Getter for an observer that holds a GitHub credential
// but no Client: restBase is the REST API base ("https://api.github.com" or a
// GHES "{host}/api/v3"), token the credential to send, and transport the
// round tripper to send it through (nil for http.DefaultTransport). It makes
// one attempt per Get and is bounded by ctx alone.
func NewTokenGetter(restBase string, token func(ctx context.Context) (string, error), transport http.RoundTripper) Getter {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return tokenGetter{base: strings.TrimRight(restBase, "/"), token: token, client: &http.Client{Transport: transport}}
}

// NewTokenQuerier is a Querier for an observer that holds a GitHub credential
// but no Client: endpoint is the GraphQL endpoint
// ("https://api.github.com/graphql" or a GHES "{host}/api/graphql"), and token
// and transport are as NewTokenGetter's. It makes one attempt per query and is
// bounded by ctx alone. A response whose data is absent or null is an error,
// carrying the first error GitHub gave; one with data and errors beside it
// answers the data, as PostGraphQL does.
func NewTokenQuerier(endpoint string, token func(ctx context.Context) (string, error), transport http.RoundTripper) Querier {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return tokenQuerier{endpoint: endpoint, token: token, client: &http.Client{Transport: transport}}
}

type tokenQuerier struct {
	endpoint string
	token    func(ctx context.Context) (string, error)
	client   *http.Client
}

func (q tokenQuerier) PostGraphQL(ctx context.Context, query string, variables map[string]any) ([]byte, error) {
	tok, err := q.token(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, errors.New("github: no credential to read with")
	}
	payload, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdentityBody))
	if err != nil {
		return nil, fmt.Errorf("read graphql response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newStatusError("GraphQL", resp, body)
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors gqlErrors       `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(answer.Data) == 0 || string(answer.Data) == "null" {
		if err := answer.Errors.first("graphql"); err != nil {
			return nil, err
		}
		return nil, errors.New("graphql: the response carries no data")
	}
	recordQueryCost(upstream.OperationName(query), body)
	return body, nil
}

type tokenGetter struct {
	base   string
	token  func(ctx context.Context) (string, error)
	client *http.Client
}

func (g tokenGetter) Get(ctx context.Context, path string) ([]byte, error) {
	tok, err := g.token(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, errors.New("github: no credential to read with")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", acceptJSON)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdentityBody))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newStatusError("GET "+path, resp, body)
	}
	return body, nil
}
