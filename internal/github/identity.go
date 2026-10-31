package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// IdentityReader reads what GitHub calls the objects an act names, and caches
// every answer, so the reader an observer holds for a run reads each
// repository, and each pull request or issue, once. A number names one object
// for the life of a repository and an id one repository for the life of its
// name, which a run does not outlast in practice. A nil reader reads without
// caching. Safe for concurrent use.
type IdentityReader struct {
	mu    sync.Mutex
	cache map[string]domain.GitHubIdentity // repoCacheKey and objectCacheKey
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

// repoCacheKey and objectCacheKey fold case: GitHub's names are
// case-insensitive.
func repoCacheKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

func objectCacheKey(owner, repo string, number int) string {
	return repoCacheKey(owner, repo) + "#" + strconv.Itoa(number)
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
