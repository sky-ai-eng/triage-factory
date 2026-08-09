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
// The fallback is for shapes this table does not know, and for nothing else.
// Recording what happened is unconditional and separate from deciding what may
// happen: an act nobody has yet decided the policy for still lands its verb the
// moment it succeeds, and when a policy does arrive it arrives through the gate
// machinery, which reads this same table. A shape held out of the table because
// its governance is unsettled would leave the log unable to answer the very
// question that governance is being decided from.
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
	"net/url"
	"slices"
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

	// Subject is the node id a GraphQL request named for the object it acted on,
	// standing in for the coordinates a REST path would have carried. Empty for
	// every REST shape, and for a GraphQL request whose response located the
	// object properly (see resolveGraphQL).
	Subject string

	// CreatesObject marks a shape whose response body names an object that did
	// not exist before the request — a posted comment or reply, an opened pull
	// request. The sidecar parses those bodies for the new object's id and
	// html_url so the audit row reaches parity with the equivalent exec verb's
	// row; every other shape is fully described by its path and needs no body
	// at all.
	CreatesObject bool

	// NumberFromObjectURL marks a create that posts to a collection, so the
	// number it will be addressed by is assigned by GitHub and appears only in
	// the response. Resolve fills Number from the created object's own url.
	NumberFromObjectURL bool

	// CarriesProvenance marks a shape whose audit row keeps the raw method and
	// path in its detail. It is set where "which channel performed this act" is
	// itself a fact the reader needs: opening a pull request and submitting a
	// review both have a governed verb path, and whether an autonomously-opened
	// PR came through that path or a hand-written `gh api` call is exactly the
	// question the policy work downstream of this table has to answer. The
	// reply and comment shapes leave it clear on purpose — their rows are
	// required to read identically to their verb's, and their detail carries
	// the act's own context instead.
	CarriesProvenance bool
}

// Target renders the shape's resource key for the audit row: owner/repo#N when
// the path carries a number, owner/repo otherwise. The '#N' form is what makes
// a raw-gh write resolve to an entity (see domain.EntityRefForExternal), so it
// is deliberately preferred wherever the path supplies one.
//
// A GraphQL write that disclosed only a node id falls back to it. That target
// resolves to no entity and links to nothing, which is the honest rendering of
// what such a request actually said about its object — better than a repo the
// row would be guessing at.
func (s Shape) Target() string {
	if s.Owner == "" || s.Repo == "" {
		return s.Subject
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

	// GraphQL, when set, marks this as a write performed through the GraphQL
	// endpoint and carries what the request envelope disclosed. Method and Path
	// are still filled in, but they say only "a POST to /graphql" — the act is
	// named in these facts instead, which is the whole reason the request is
	// read at all.
	GraphQL *GraphQLFacts

	// Errored marks a response that reported the write failed even though the
	// transport succeeded — GraphQL's convention, where a refused mutation is a
	// 200 carrying an errors array. It is the GraphQL spelling of a non-2xx and
	// folds into Succeeded for exactly that reason.
	Errored bool

	// ResponseUnread marks a GraphQL write whose response could not be read at
	// all — past the buffering cap, or broken mid-stream. Kept separate from
	// Errored because they are different facts: one says the server refused the
	// write, the other says nobody knows. Both stop the row short of claiming a
	// success, which is the safe direction — an act recorded as attempted when
	// it landed understates the log, while the reverse asserts a write nothing
	// observed.
	ResponseUnread bool
}

// Classify resolves this observation's shape: from the request envelope for a
// GraphQL write, from method and path for a REST one. Most callers want
// Resolve, which also folds in what the response said.
func (o Observation) Classify() (Shape, bool) {
	if o.GraphQL != nil {
		return classifyGraphQL(*o.GraphQL)
	}
	return Classify(o.Method, o.Path)
}

// Resolve is Classify completed against the response's own facts: the created
// object's id, and — for a create that posts to a collection — the number
// GitHub assigned it, which no part of the request could have known.
//
// A pull request is the awkward case and the reason this exists. Its response
// id is the REST database id, while every surface in the product addresses a PR
// by its NUMBER, so the number is taken from the created object's url and the
// database id is dropped rather than filed under a field that means something
// else. A url that didn't parse (a body past the injector's cap, an unexpected
// shape) leaves the row on its repo with no id: the act is still recorded, and
// nothing is asserted that wasn't seen.
func Resolve(obs Observation) (Shape, bool) {
	if obs.GraphQL != nil {
		return resolveGraphQL(obs)
	}
	s, ok := Classify(obs.Method, obs.Path)
	if !ok {
		return Shape{}, false
	}
	if obs.ExternalID != "" {
		s.ExternalID = obs.ExternalID
	}
	if s.NumberFromObjectURL {
		// Either collection's url: a pull request and an issue are both
		// addressed by the number their own link carries.
		_, _, number, parsed := ParseObjectURL(obs.URL)
		if !parsed {
			s.ExternalID = ""
			return s, true
		}
		s.Number = number
		s.ExternalID = strconv.Itoa(number)
	}
	return s, true
}

// Succeeded reports whether the upstream accepted the write. Only a successful
// write earns its semantic verb: a 404'd merge is not a merge, and neither is a
// GraphQL merge the server answered 200 with an errors array — the two are the
// same refusal spelled in different transports.
func (o Observation) Succeeded() bool {
	return o.Status >= 200 && o.Status < 300 && !o.Errored && !o.ResponseUnread
}

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

// ParsePullRequestURL pulls the coordinates out of a pull request's html url,
// whose shape is {scheme}://{host}/{owner}/{repo}/pull/{n} on dotcom and GHES
// alike. The scan takes the first "pull" segment with two segments before it
// and a positive integer after it, so a repository literally named "pull" still
// resolves.
//
// It lives here rather than beside either caller because both the write
// classifier and the injector's GraphQL observation read a PR's identity out of
// the same url, and two parsers would eventually disagree about what a PR url
// is.
func ParsePullRequestURL(raw string) (owner, repo string, number int, ok bool) {
	return parseObjectURL(raw, "pull")
}

// ParseObjectURL is ParsePullRequestURL widened to an issue's url as well. A
// GraphQL write discloses its target only as whatever object url the response
// happened to carry, and gh's comment mutations answer with one that may point
// at either — a pull request and an issue share GitHub's comment machinery, and
// nothing in the request says which was addressed.
func ParseObjectURL(raw string) (owner, repo string, number int, ok bool) {
	return parseObjectURL(raw, "pull", "issues")
}

// ParseRepoURL pulls owner and repo out of a repository's own html url, whose
// whole path is the two segments. Separate from ParseObjectURL because a
// repository is located by having nothing under it — there is no collection
// segment to anchor on, so the shape is "exactly two segments" and a url with a
// third is some object inside the repository rather than the repository itself.
//
// Only the repository-family mutations use it, and only for the two that create:
// every other mutation names its target in the request, while a repository that
// did not exist when the request was sent can be located only by what came back.
func ParseRepoURL(raw string) (owner, repo string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", "", false
	}
	return segs[0], segs[1], true
}

// parseObjectURL scans for the first of the given collection segments with two
// segments before it and a positive integer after it. Any fragment on the url
// (a comment anchor, which is what several of these carry) is already split off
// by url.Parse, so it never reaches the scan.
func parseObjectURL(raw string, collections ...string) (owner, repo string, number int, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, seg := range segs {
		if !slices.Contains(collections, seg) || i < 2 || i+1 >= len(segs) {
			continue
		}
		n, err := strconv.Atoi(segs[i+1])
		if err != nil || n <= 0 || segs[i-2] == "" || segs[i-1] == "" {
			continue
		}
		return segs[i-2], segs[i-1], n, true
	}
	return "", "", 0, false
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
	if !ok {
		return Shape{}, false
	}
	s := Shape{Owner: owner, Repo: repo}

	// The repository itself, with nothing addressed under it. Like its pull
	// request and issue twins, the URL says only that the object changed — a
	// rename, a visibility flip, and a description edit are one shape with
	// different bodies — so the verb goes no finer than the path does.
	if len(rest) == 0 {
		switch method {
		case "PATCH":
			s.Action = domain.ActionRepoEdited
		case "DELETE":
			s.Action = domain.ActionRepoDeleted
		default:
			return Shape{}, false
		}
		return s, true
	}

	switch rest[0] {
	case "pulls":
		return classifyPulls(s, method, rest[1:])
	case "issues":
		return classifyIssues(s, method, rest[1:])
	case "actions":
		return classifyActions(s, method, rest[1:])
	case "labels":
		return classifyLabels(s, method, rest[1:])
	case "releases":
		return classifyReleases(s, method, rest[1:])
	case "forks":
		if method == "POST" && len(rest) == 1 {
			// The path names the repository that was COPIED, so that is the row's
			// target — the act was performed on it. Where the copy landed is in
			// the response and deliberately not lifted out: the fact worth
			// recording is that the org credential forked this repository, and
			// the copy is reachable from there.
			s.Action = domain.ActionRepoForked
			return s, true
		}
	case "topics":
		if method == "PUT" && len(rest) == 1 {
			// Topics are repository settings that happen to have their own
			// endpoint, which `gh repo edit --add-topic` writes to alongside the
			// ordinary settings mutation. Same act, so the same verb: a reader
			// filtering for repository edits wants both.
			s.Action = domain.ActionRepoEdited
			return s, true
		}
	}
	return Shape{}, false
}

// classifyPulls handles /repos/{o}/{r}/pulls/... — the pull request itself, the
// review comments addressed under it, and the two creates that also produce an
// artifact.
//
// Those two are here for the same reason merge is: recording what happened is
// unconditional, and whether the act should have been permitted is a separate
// decision made by a separate mechanism reading this same table. An artifact
// answers "what exists", which is not the question the actions log answers — a
// PR create that landed a pull_request artifact and an opaque audit row is
// invisible to anyone filtering the log for PR creations, which is precisely
// the asymmetry between the verb path and the raw path this table exists to
// remove.
func classifyPulls(s Shape, method string, rest []string) (Shape, bool) {
	// The collection itself: opening a pull request.
	if len(rest) == 0 {
		if method != "POST" {
			return Shape{}, false
		}
		s.Action = domain.ActionPRCreated
		s.CreatesObject = true
		s.NumberFromObjectURL = true
		s.CarriesProvenance = true
		return s, true
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
	case len(rest) == 2 && rest[1] == "update-branch" && method == "PUT":
		s.Action = domain.ActionPRBranchUpdated
	case len(rest) == 2 && rest[1] == "reviews" && method == "POST":
		// Also the only way inline review comments reach GitHub by hand: they
		// ride the review's own body.
		s.Action = domain.ActionReviewSubmitted
		s.CreatesObject = true
		s.CarriesProvenance = true
	case len(rest) == 4 && rest[1] == "comments" && rest[3] == "replies" && method == "POST":
		reply, err := strconv.Atoi(rest[2])
		if err != nil || reply <= 0 {
			return Shape{}, false
		}
		s.Action = domain.ActionCommentPosted
		s.InReplyTo = reply
		s.CreatesObject = true
	case len(rest) == 2 && rest[1] == "requested_reviewers" && method == "POST":
		// `gh pr edit --add-reviewer` sends this alongside its GraphQL edit, so a
		// single user command legitimately produces two rows — two requests, two
		// acts. Naming this one is what keeps the second from being anonymous.
		s.Action = domain.ActionReviewRequested
	case len(rest) == 2 && rest[1] == "requested_reviewers" && method == "DELETE":
		s.Action = domain.ActionReviewRequestRemoved
	default:
		return Shape{}, false
	}
	return s, true
}

// classifyIssues handles /repos/{o}/{r}/issues/... — the issue itself, its
// top-level comments and reactions, and the two things GitHub models on the
// issue even when they are performed on a pull request: its labels and its
// conversation lock. That last part is why this function covers more of the
// pull-request surface than its name suggests — a PR's labels and lock are
// addressed here, and only its diff-level objects live under /pulls.
func classifyIssues(s Shape, method string, rest []string) (Shape, bool) {
	// The collection itself: opening an issue.
	if len(rest) == 0 {
		if method != "POST" {
			return Shape{}, false
		}
		s.Action = domain.ActionIssueCreated
		s.CreatesObject = true
		s.NumberFromObjectURL = true
		return s, true
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
	case len(rest) == 1 && method == "PATCH":
		// Like its pull-request twin: the URL says the issue changed, and
		// whether that was a retitle or a close lives in a body nothing reads.
		s.Action = domain.ActionIssueUpdated
	case len(rest) == 2 && rest[1] == "comments" && method == "POST":
		s.Action = domain.ActionCommentPosted
		s.CreatesObject = true
	case len(rest) == 2 && rest[1] == "reactions" && method == "POST":
		s.Action = domain.ActionReactionAdded
	case len(rest) == 3 && rest[1] == "reactions" && method == "DELETE":
		s.Action = domain.ActionReactionRemoved
	case len(rest) == 2 && rest[1] == "labels" && method == "POST":
		s.Action = domain.ActionLabelAdded
	case (len(rest) == 2 || len(rest) == 3) && rest[1] == "labels" && method == "DELETE":
		// One label by name, or the whole set at once. Both are "labels came off
		// this issue", and the label is a parameter of the act rather than the
		// object it was performed on — which is why neither form fills ExternalID.
		s.Action = domain.ActionLabelRemoved
	case len(rest) == 2 && rest[1] == "lock" && method == "PUT":
		s.Action = domain.ActionConversationLocked
	case len(rest) == 2 && rest[1] == "lock" && method == "DELETE":
		s.Action = domain.ActionConversationUnlocked
	default:
		return Shape{}, false
	}
	return s, true
}

// classifyLabels handles /repos/{o}/{r}/labels/... — the set of labels the
// repository OFFERS. Not to be confused with the labels ON an issue, which
// GitHub addresses under /issues and this package classifies there: the two
// families read alike and are not alike at all, since deleting a definition
// strips that label from every issue carrying it.
//
// GitHub addresses a label by its name rather than by a number, so the name is
// what fills ExternalID. It arrives percent-encoded (labels may contain spaces
// and colons) and is decoded for the row, because an audit log is read by people
// and "needs%20triage" names the label less well than the label's own name does.
func classifyLabels(s Shape, method string, rest []string) (Shape, bool) {
	if len(rest) == 0 {
		if method != "POST" {
			return Shape{}, false
		}
		s.Action = domain.ActionLabelDefined
		return s, true
	}
	if len(rest) != 1 || rest[0] == "" {
		return Shape{}, false
	}
	name := rest[0]
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	s.ExternalID = name
	switch method {
	case "PATCH":
		s.Action = domain.ActionLabelDefinitionEdited
	case "DELETE":
		s.Action = domain.ActionLabelDefinitionDeleted
	default:
		return Shape{}, false
	}
	return s, true
}

// classifyReleases handles /repos/{o}/{r}/releases/... — the release itself,
// and nothing under it.
//
// A release's ASSETS are the deliberate omission. Their uploads go to an
// absolute url the release payload carries, which addresses GitHub directly
// rather than this channel's base, so the request never transits the injector
// and no classification here could see it. Those paths fall to the fallback
// like any unknown shape, which is the right outcome for a shape that will
// almost never arrive: naming them would advertise coverage this channel
// structurally does not have.
func classifyReleases(s Shape, method string, rest []string) (Shape, bool) {
	if len(rest) == 0 {
		if method != "POST" {
			return Shape{}, false
		}
		s.Action = domain.ActionReleaseCreated
		s.CreatesObject = true
		return s, true
	}
	if len(rest) != 1 {
		return Shape{}, false
	}
	if id, err := strconv.Atoi(rest[0]); err != nil || id <= 0 {
		return Shape{}, false
	}
	s.ExternalID = rest[0]
	switch method {
	case "PATCH":
		s.Action = domain.ActionReleaseEdited
	case "DELETE":
		s.Action = domain.ActionReleaseDeleted
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
