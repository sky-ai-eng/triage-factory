// Package ghwrite is the shared classification table for writes an agent makes
// through the real-`gh` credential channel: given the HTTP method and the
// upstream path of a request the injector forwarded, it names the semantic act
// — "posted a review-thread reply", "merged the pull request" — that the audit
// log records.
//
// It exists because three consumers must agree on that mapping and they live in
// different processes. The per-run credential sidecar reads it to decide which
// responses carry a created object worth parsing; the orchestrator reads it to
// build the audit row (it alone holds the DB and the domain vocabulary); and the
// conformance test reads it to prove the table still matches what the pinned
// `gh` actually emits. A second copy of the table anywhere is a version skew
// waiting to mislabel a write.
//
// # What is classified, and what is not
//
// Only shapes an agent actually reaches for. Everything else — an endpoint
// nobody has hit, a shape whose semantics can't be read off the URL — is
// deliberately unclassified, and its write is recorded under the opaque
// fallback rather than guessed at. A wrong verb in an audit log is worse than
// an honest "a write happened here".
//
// # Method and path only
//
// The classifier never sees a request body, because the injector never reads
// one (see internal/ghinjector). That bounds what can be known: a PATCH on a
// pull request is an edit, and a PATCH that closes one is the same request
// shape with `state: closed` in a body nothing on this path may parse. So the
// table names the act the URL identifies and stops there. The response body is
// a different matter — it is already parsed for artifact observation, and the
// create shapes flagged CreatesObject extend that to pick up the id and link of
// the object that was just made.
package ghwrite

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Shape is one classified REST write: the semantic act, and the coordinates the
// path names it against.
type Shape struct {
	// Action is the audit discriminator — one of the domain.Action* consts.
	Action string

	// Owner / Repo are the repository the path addresses.
	Owner string
	Repo  string

	// Number is the pull-request / issue number the path carries, or 0 when it
	// addresses an object by its own id instead (a comment edit names a comment,
	// not the PR it hangs off).
	Number int

	// ExternalID is the provider-native id the PATH names for the object acted
	// on — the comment being edited, the workflow being dispatched. Empty for a
	// create, whose id exists only in the response (see CreatesObject). For a
	// reaction it is the object the reaction lands ON, not the reaction itself:
	// the log's question is what the run did to which object.
	ExternalID string

	// InReplyTo is the review comment a reply threads under, from the path. 0
	// for every other shape.
	InReplyTo int

	// CreatesObject marks a shape whose response body names an object that did
	// not exist before the request — a posted comment or reply. The sidecar
	// parses those bodies for the new object's id and html_url so the audit row
	// reaches parity with the equivalent exec verb's row; every other shape is
	// fully described by its path and needs no body at all.
	CreatesObject bool
}

// Target renders the shape's resource key for the audit row: owner/repo#N when
// the path carries a number, owner/repo otherwise. The '#N' form is what makes
// a raw-gh write resolve to an entity (see domain.EntityRefForExternal), so it
// is deliberately preferred wherever the path supplies one.
func (s Shape) Target() string {
	if s.Owner == "" || s.Repo == "" {
		return ""
	}
	if s.Number > 0 {
		return fmt.Sprintf("%s/%s#%d", s.Owner, s.Repo, s.Number)
	}
	return s.Owner + "/" + s.Repo
}

// Observation is one forwarded REST write as the injector saw it: the wire
// facts, and nothing derived. Method/Path/Status come off the response's own
// request record; ExternalID/URL are read from the response body, and only for
// the CreatesObject shapes.
//
// The sidecar fills this in and the orchestrator turns it into a row — the
// split that keeps row-building where the DB and the domain vocabulary are.
type Observation struct {
	// Method is the HTTP method, one of the mutating set (POST/PATCH/PUT/DELETE).
	Method string
	// Path is the upstream request path (post-rewrite, so a GHES /api/v3 prefix
	// is already resolved to the org's real API shape).
	Path string
	// Status is the upstream response code. A non-2xx is reported too: a refused
	// write is exactly the outcome an audit log must not omit.
	Status int
	// ExternalID / URL are the created object's id and html url, read from the
	// response body of a CreatesObject shape. Empty when the shape creates
	// nothing, when the body was past the injector's buffering cap, or when it
	// didn't parse — the act is still recorded, only its deep link degrades.
	ExternalID string
	URL        string
}

// Classify resolves this observation's shape.
func (o Observation) Classify() (Shape, bool) { return Classify(o.Method, o.Path) }

// Succeeded reports whether the upstream accepted the write. Only a successful
// write earns its semantic verb: a 404'd merge is not a merge.
func (o Observation) Succeeded() bool { return o.Status >= 200 && o.Status < 300 }

// reposSegment is the path anchor every repository-scoped REST endpoint carries.
const reposSegment = "/repos/"

// RepoPath pulls "owner/repo" out of a REST path shaped
// .../repos/{owner}/{repo}/..., or returns empty when the path names no repo (a
// user- or org-level endpoint). Classification-independent: the fallback row
// files an unclassified write under its repo too.
func RepoPath(path string) string {
	owner, repo, _, ok := splitRepoPath(path)
	if !ok {
		return ""
	}
	return owner + "/" + repo
}

// splitRepoPath separates a repository-scoped path into its owner, repo, and
// the segments addressed under them. The anchor is the first "/repos/", which
// absorbs both the dotcom shape and the GHES /api/v3 prefix.
func splitRepoPath(path string) (owner, repo string, rest []string, ok bool) {
	i := strings.Index(path, reposSegment)
	if i < 0 {
		return "", "", nil, false
	}
	segs := strings.Split(strings.Trim(path[i+len(reposSegment):], "/"), "/")
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" {
		return "", "", nil, false
	}
	return segs[0], segs[1], segs[2:], true
}

// Classify maps one mutating REST request onto the act it performs. ok is false
// for anything outside the table, which the caller records under the opaque
// fallback rather than labelling.
//
// The dispatch reads as the URL does: the collection the path enters, then how
// it addresses a member of it. The two ambiguous pairs — /issues/{n}/comments
// against /issues/comments/{id}, and the same under /pulls — are separated by
// position, never by guessing which of two segments is numeric.
func Classify(method, path string) (Shape, bool) {
	owner, repo, rest, ok := splitRepoPath(path)
	if !ok || len(rest) == 0 {
		return Shape{}, false
	}
	s := Shape{Owner: owner, Repo: repo}

	switch rest[0] {
	case "pulls":
		return classifyPulls(s, method, rest[1:])
	case "issues":
		return classifyIssues(s, method, rest[1:])
	case "actions":
		return classifyActions(s, method, rest[1:])
	}
	return Shape{}, false
}

// classifyPulls handles /repos/{o}/{r}/pulls/... — the pull request itself, and
// the review comments addressed under it.
//
// A PR create (POST .../pulls) is deliberately absent: it already produces a
// pull_request artifact through the injector's separate observation path, and
// what an autonomously-opened PR should record beyond that is a governance
// decision of its own. It keeps the fallback row until that decision lands.
func classifyPulls(s Shape, method string, rest []string) (Shape, bool) {
	if len(rest) == 0 {
		return Shape{}, false
	}

	// Review comments addressed by their own id: /pulls/comments/{id}[/...].
	if rest[0] == "comments" {
		if len(rest) < 2 || rest[1] == "" {
			return Shape{}, false
		}
		s.ExternalID = rest[1]
		switch {
		case len(rest) == 2 && method == "PATCH":
			s.Action = domain.ActionReviewCommentEdited
		case len(rest) == 2 && method == "DELETE":
			s.Action = domain.ActionReviewCommentDeleted
		case len(rest) == 3 && rest[2] == "reactions" && method == "POST":
			s.Action = domain.ActionReactionAdded
		case len(rest) == 4 && rest[2] == "reactions" && method == "DELETE":
			s.Action = domain.ActionReactionRemoved
		default:
			return Shape{}, false
		}
		return s, true
	}

	// Everything else under /pulls addresses a numbered pull request.
	n, err := strconv.Atoi(rest[0])
	if err != nil || n <= 0 {
		return Shape{}, false
	}
	s.Number = n
	switch {
	case len(rest) == 1 && method == "PATCH":
		// The URL says "this pull request changed"; whether the change was a
		// title edit or a close lives in the body, which is never read.
		s.Action = domain.ActionPREdited
	case len(rest) == 2 && rest[1] == "merge" && method == "PUT":
		s.Action = domain.ActionPRMerged
	case len(rest) == 4 && rest[1] == "comments" && rest[3] == "replies" && method == "POST":
		reply, err := strconv.Atoi(rest[2])
		if err != nil || reply <= 0 {
			return Shape{}, false
		}
		s.Action = domain.ActionCommentPosted
		s.InReplyTo = reply
		s.CreatesObject = true
	default:
		return Shape{}, false
	}
	return s, true
}

// classifyIssues handles /repos/{o}/{r}/issues/... — top-level comments and
// reactions. GitHub serves a pull request's conversation comments here too, so
// this covers `gh api` posting on a PR as well as on an issue.
func classifyIssues(s Shape, method string, rest []string) (Shape, bool) {
	if len(rest) == 0 {
		return Shape{}, false
	}

	// Comments addressed by their own id: /issues/comments/{id}[/...].
	if rest[0] == "comments" {
		if len(rest) < 2 || rest[1] == "" {
			return Shape{}, false
		}
		s.ExternalID = rest[1]
		switch {
		case len(rest) == 2 && method == "PATCH":
			s.Action = domain.ActionCommentEdited
		case len(rest) == 2 && method == "DELETE":
			s.Action = domain.ActionCommentDeleted
		case len(rest) == 3 && rest[2] == "reactions" && method == "POST":
			s.Action = domain.ActionReactionAdded
		case len(rest) == 4 && rest[2] == "reactions" && method == "DELETE":
			s.Action = domain.ActionReactionRemoved
		default:
			return Shape{}, false
		}
		return s, true
	}

	n, err := strconv.Atoi(rest[0])
	if err != nil || n <= 0 {
		return Shape{}, false
	}
	s.Number = n
	switch {
	case len(rest) == 2 && rest[1] == "comments" && method == "POST":
		s.Action = domain.ActionCommentPosted
		s.CreatesObject = true
	case len(rest) == 2 && rest[1] == "reactions" && method == "POST":
		s.Action = domain.ActionReactionAdded
	case len(rest) == 3 && rest[1] == "reactions" && method == "DELETE":
		s.Action = domain.ActionReactionRemoved
	default:
		return Shape{}, false
	}
	return s, true
}

// classifyActions handles /repos/{o}/{r}/actions/... — dispatching a workflow
// and cancelling a run. Both spend CI minutes under the org credential, so both
// belong in the log by name.
func classifyActions(s Shape, method string, rest []string) (Shape, bool) {
	if method != "POST" || len(rest) != 3 {
		return Shape{}, false
	}
	if rest[1] == "" {
		return Shape{}, false
	}
	s.ExternalID = rest[1]
	switch {
	case rest[0] == "workflows" && rest[2] == "dispatches":
		s.Action = domain.ActionWorkflowDispatched
	case rest[0] == "runs" && rest[2] == "cancel":
		s.Action = domain.ActionWorkflowRunCancelled
	default:
		return Shape{}, false
	}
	return s, true
}
