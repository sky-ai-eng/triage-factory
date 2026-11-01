package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The reads the poll's branch watch makes to tell what a base branch's move
// changed, and which pull requests on the base it can have changed the
// mergeability of: the branch's tip, the compare between two tips, and each
// pull request's changed paths.

// BranchTip reads the tip of branch (a name without refs/heads/) through
// GET /repos/{owner}/{repo}/git/ref/heads/{branch}, conditional on etag when
// it is non-empty. A 304 answers notModified with no sha and costs nothing
// against the rate limit; a 200 answers the tip and the response's ETag. A
// branch that does not exist is a 404 *HTTPError.
func (c *Client) BranchTip(ctx context.Context, owner, repo, branch, etag string) (sha, newETag string, notModified bool, err error) {
	segments := strings.Split(branch, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, strings.Join(segments, "/"))
	data, newETag, notModified, err := c.GetConditional(ctx, path, etag)
	if err != nil || notModified {
		return "", "", notModified, err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(data, &ref); err != nil {
		return "", "", false, fmt.Errorf("parse ref %s: %w", branch, err)
	}
	if ref.Object.SHA == "" {
		return "", "", false, fmt.Errorf("ref %s: response carried no sha", branch)
	}
	return ref.Object.SHA, newETag, false, nil
}

// The compare endpoint's limits: it lists at most CompareFileCap files and,
// unpaginated, at most CompareCommitCap commits. A compare listing exactly
// CompareFileCap files may be missing some.
const (
	CompareFileCap   = 300
	CompareCommitCap = 250
)

// Comparison is the slice of GET /repos/{owner}/{repo}/compare/{base}...{head}
// the branch watch reads.
type Comparison struct {
	// Status is ahead, behind, diverged or identical: where head stands
	// against base.
	Status string
	// TotalCommits is how many commits head has that base does not, whether
	// or not Commits lists them all.
	TotalCommits int
	// Files is what the range changed, mapped onto GitHub's PatchStatus
	// vocabulary; Previous is the compare's previous_filename where it gave
	// one.
	Files []domain.ChangedPath
	// Commits is the range's commits as the compare lists them, at most
	// CompareCommitCap.
	Commits []ComparedCommit
}

// ComparedCommit is one commit of a compared range and its parents' shas,
// first parent first.
type ComparedCommit struct {
	SHA     string
	Parents []string
}

// Compare reads base...head. base and head are commit shas or refs. A base or
// head GitHub does not hold is a 404 *HTTPError.
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (*Comparison, error) {
	data, err := c.Get(ctx, comparePath(owner, repo, base, head))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Status       string `json:"status"`
		TotalCommits int    `json:"total_commits"`
		Files        []struct {
			Filename         string `json:"filename"`
			Status           string `json:"status"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
		Commits []struct {
			SHA     string `json:"sha"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse compare %s...%s: %w", base, head, err)
	}
	out := &Comparison{Status: raw.Status, TotalCommits: raw.TotalCommits}
	out.Files = make([]domain.ChangedPath, 0, len(raw.Files))
	for _, f := range raw.Files {
		out.Files = append(out.Files, domain.ChangedPath{Path: f.Filename, Change: patchStatus(f.Status), Previous: f.PreviousFilename})
	}
	out.Commits = make([]ComparedCommit, 0, len(raw.Commits))
	for _, cm := range raw.Commits {
		parents := make([]string, 0, len(cm.Parents))
		for _, p := range cm.Parents {
			parents = append(parents, p.SHA)
		}
		out.Commits = append(out.Commits, ComparedCommit{SHA: cm.SHA, Parents: parents})
	}
	return out, nil
}

// patchStatus maps a REST file status onto the PatchStatus GraphQL reports
// for a pull request's files. unchanged (a mode change, say) and any status
// GitHub adds later read as CHANGED.
func patchStatus(rest string) string {
	switch rest {
	case "added":
		return domain.PathAdded
	case "removed":
		return domain.PathDeleted
	case "renamed":
		return domain.PathRenamed
	case "copied":
		return domain.PathCopied
	case "modified":
		return domain.PathModified
	}
	return domain.PathChanged
}

// MaxPRChangedPaths is the most changed files TF reads of one pull request:
// GitHub lists no more than this through either API.
const MaxPRChangedPaths = 3000

// prChangedPathsQuery reads up to 100 pull requests' changed paths in one
// query, the first page of each; prChangedPathsPageQuery reads one pull
// request's next page. Both are named so their costs are recorded apart.
const prChangedPathsQuery = `
	query PRChangedPaths($ids: [ID!]!) {
		nodes(ids: $ids) {
			... on PullRequest {
				id headRefOid baseRefName changedFiles
				files(first: 100) { pageInfo { hasNextPage endCursor } nodes { path changeType } }
			}
		}
		rateLimit { cost }
	}
`

const prChangedPathsPageQuery = `
	query PRChangedPathsPage($id: ID!, $cursor: String!) {
		node(id: $id) {
			... on PullRequest {
				headRefOid baseRefName
				files(first: 100, after: $cursor) { pageInfo { hasNextPage endCursor } nodes { path changeType } }
			}
		}
		rateLimit { cost }
	}
`

// MaxChangedPathsBatch is how many pull requests one PRChangedPaths query
// reads: GitHub's limit on nodes(ids:).
const MaxChangedPathsBatch = 100

// ChangedPathsRead is one pull request's changed paths as GraphQL listed them.
// GraphQL gives no previous path for a rename or copy; PreviousPaths reads
// those.
type ChangedPathsRead struct {
	// HeadSHA and BaseRef are the head (headRefOid) and base branch
	// (baseRefName) the first page was read at.
	HeadSHA string
	BaseRef string
	// ChangedFiles is how many files GitHub says the pull request changes.
	ChangedFiles int
	Paths        []domain.ChangedPath
	// Complete is true when Paths holds all ChangedFiles, read at one head
	// and base.
	Complete bool
}

// gqlChangedFiles is one page of a pull request's files connection.
type gqlChangedFiles struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []struct {
		Path       string `json:"path"`
		ChangeType string `json:"changeType"`
	} `json:"nodes"`
}

func (f *gqlChangedFiles) appendTo(paths []domain.ChangedPath) []domain.ChangedPath {
	for _, n := range f.Nodes {
		paths = append(paths, domain.ChangedPath{Path: n.Path, Change: n.ChangeType})
	}
	return paths
}

// PRChangedPaths reads the changed paths of up to MaxChangedPathsBatch pull
// requests, keyed by node id, paging each one with more files through
// node(id:) up to MaxPRChangedPaths. requests counts the queries sent. An id
// with no node, or whose node is not a pull request, is absent. A later page
// that fails, or that reads another head or base than the first, ends that
// pull request's list incomplete rather than failing the batch. err is the
// first page's failure, or a rate limit on any page: every request after one
// would be refused the same way, and a GraphQL secondary limit does not stop
// the client's own budget check from sending them, so the batch ends there and
// returns no reads.
func (c *Client) PRChangedPaths(ctx context.Context, nodeIDs []string) (reads map[string]ChangedPathsRead, requests int, err error) {
	if len(nodeIDs) == 0 {
		return nil, 0, nil
	}
	if len(nodeIDs) > MaxChangedPathsBatch {
		return nil, 0, fmt.Errorf("PR changed paths: %d ids, at most %d per query", len(nodeIDs), MaxChangedPathsBatch)
	}
	data, err := c.PostGraphQL(ctx, prChangedPathsQuery, map[string]any{"ids": nodeIDs})
	requests++
	if err != nil {
		return nil, requests, fmt.Errorf("PR changed paths: %w", err)
	}
	var resp struct {
		Data struct {
			Nodes []*struct {
				ID           string           `json:"id"`
				HeadRefOid   string           `json:"headRefOid"`
				BaseRefName  string           `json:"baseRefName"`
				ChangedFiles int              `json:"changedFiles"`
				Files        *gqlChangedFiles `json:"files"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, requests, fmt.Errorf("parse PR changed paths response: %w", err)
	}
	reads = make(map[string]ChangedPathsRead, len(nodeIDs))
	for i, n := range resp.Data.Nodes {
		if i >= len(nodeIDs) || n == nil || n.ID == "" || n.ID != nodeIDs[i] {
			continue
		}
		read := ChangedPathsRead{HeadSHA: n.HeadRefOid, BaseRef: n.BaseRefName, ChangedFiles: n.ChangedFiles}
		// files is null when GitHub could not list them (a field error
		// beside the node), which leaves the list incomplete.
		stable := n.Files != nil
		if n.Files != nil {
			read.Paths = n.Files.appendTo(nil)
			next, cursor := n.Files.PageInfo.HasNextPage, n.Files.PageInfo.EndCursor
			for next && len(read.Paths) < MaxPRChangedPaths {
				page, head, base, err := c.prChangedPathsPage(ctx, n.ID, cursor)
				requests++
				var rl *ErrRateLimited
				if errors.As(err, &rl) {
					return nil, requests, fmt.Errorf("PR changed paths: %w", err)
				}
				if err != nil || page == nil || head != read.HeadSHA || base != read.BaseRef {
					stable = false
					break
				}
				read.Paths = page.appendTo(read.Paths)
				next, cursor = page.PageInfo.HasNextPage, page.PageInfo.EndCursor
			}
		}
		read.Complete = stable && len(read.Paths) == read.ChangedFiles
		reads[n.ID] = read
	}
	return reads, requests, nil
}

// prChangedPathsPage reads one page of a pull request's files after cursor,
// and the head and base it was read at. A nil page is a pull request GitHub
// no longer answers for, or whose files it could not list.
func (c *Client) prChangedPathsPage(ctx context.Context, nodeID, cursor string) (page *gqlChangedFiles, head, base string, err error) {
	data, err := c.PostGraphQL(ctx, prChangedPathsPageQuery, map[string]any{"id": nodeID, "cursor": cursor})
	if err != nil {
		return nil, "", "", err
	}
	var resp struct {
		Data struct {
			Node *struct {
				HeadRefOid  string           `json:"headRefOid"`
				BaseRefName string           `json:"baseRefName"`
				Files       *gqlChangedFiles `json:"files"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, "", "", fmt.Errorf("parse PR changed paths page: %w", err)
	}
	if resp.Data.Node == nil {
		return nil, "", "", nil
	}
	n := resp.Data.Node
	return n.Files, n.HeadRefOid, n.BaseRefName, nil
}

// PreviousPaths maps each renamed or copied file of pull request number to the
// path it came from, read from GET /repos/{owner}/{repo}/pulls/{n}/files,
// paged, up to MaxPRChangedPaths files. requests counts the pages sent.
func (c *Client) PreviousPaths(ctx context.Context, owner, repo string, number int) (previous map[string]string, requests int, err error) {
	previous = map[string]string{}
	const perPage = 100
	for page, seen := 1, 0; seen < MaxPRChangedPaths; page++ {
		data, err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=%d&page=%d", owner, repo, number, perPage, page))
		requests++
		if err != nil {
			return nil, requests, err
		}
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := json.Unmarshal(data, &files); err != nil {
			return nil, requests, fmt.Errorf("parse pull request files: %w", err)
		}
		for _, f := range files {
			if f.PreviousFilename != "" {
				previous[f.Filename] = f.PreviousFilename
			}
		}
		seen += len(files)
		if len(files) < perPage {
			break
		}
	}
	return previous, requests, nil
}
