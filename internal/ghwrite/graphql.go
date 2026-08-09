package ghwrite

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The GraphQL half of the table. Most of what `gh`'s porcelain writes — a
// comment, a close, a merge, a review — never touches a REST path: it is a
// POST to one endpoint whose body names the act. The REST half above can read
// the act off the URL; here the URL says only "GraphQL", so the act has to be
// read out of the request.
//
// That is a real widening of what the injector does, and it is scoped as
// narrowly as the job allows: the envelope's three known members, the top level
// of the operation that runs (plus the fragments that operation spreads into
// it), and a node id out of the variables. Nothing walks the arguments, and no
// text an agent or a stranger wrote is ever interpreted — see gqldoc.go for what
// the reader refuses to look at.
//
// The mutation names below are the vocabulary GitHub's schema defines, mapped
// onto the same domain actions the REST shapes produce. One act, one name,
// whichever pipe it arrived through: `gh pr comment` and a hand-written
// `gh api …/comments` both land comment_posted, and a reader filtering the log
// for merges finds them regardless of how the agent spelled it.

// mutationKeyword is the prescreen. A document that performs a mutation must
// say so — the spec's shorthand form (a bare selection set) is a query — so a
// body without this token cannot be a write, and the reads that make up nearly
// all of gh's traffic never reach the parser.
const mutationKeyword = "mutation"

// Reasons a GraphQL request could not be resolved to one named act. Each one is
// recorded verbatim on the fallback row: "a write happened and here is exactly
// how much we could tell about it" is the honest form of not knowing, and the
// distinctions matter — an over-cap body is an operational fact, an ambiguous
// operation is a malformed client, and an unresolved spread is a document shape
// gh does not emit.
const (
	// GraphQLOverCap: the request body was larger than the injector will buffer.
	GraphQLOverCap = "over_cap"
	// GraphQLRequestUnread: the request body's own read failed partway through,
	// so there was never a whole document to look at. Distinct from over-cap,
	// which is a limit this side chose: one says the caller did not finish
	// sending, the other says we declined to hold it all. Reading them as the
	// same fact would have an operational failure looking like a policy one.
	GraphQLRequestUnread = "request_unread"
	// GraphQLMalformed: the envelope or the query document did not parse.
	GraphQLMalformed = "malformed"
	// GraphQLAmbiguousOperation: several operations and no operationName to
	// choose between them, or an operationName naming none of them. The server
	// rejects both, but which one it rejected is not knowable from here.
	GraphQLAmbiguousOperation = "ambiguous_operation"
	// GraphQLUnresolvedFragment: the mutation's selection reached a fragment the
	// reader could not account for — one the document never defines, or one
	// conditioned on a type other than the mutation root. Spreads it CAN follow
	// are followed, so this now means a genuine dead end rather than a
	// construction declined on sight.
	GraphQLUnresolvedFragment = "unresolved_fragment"
)

// GraphQLFacts is what the injector could read off one GraphQL request. It
// carries names and ids only — never a fragment of the document, never an
// argument value, never a comment body. That bound is deliberate: these facts
// cross a process boundary and land in an audit row, and the request they come
// from is the most attacker-influenced text in the system.
type GraphQLFacts struct {
	// Operation is the operation's name — the one operationName selected, or the
	// sole operation's own name. Empty for an anonymous operation.
	Operation string

	// Fields are the top-level field names the mutation selects, with aliases
	// resolved to the field actually invoked. An alias is the only way a request
	// can hide which mutation it runs, and resolving it here is the reason the
	// request is read at all — the response arrives keyed under the alias, so no
	// amount of response parsing could recover it.
	Fields []string

	// Subject is the node id the variables named for the object being acted on
	// (a pull request, a comment, the subject of a reaction). GitHub's GraphQL
	// addresses objects by opaque node id, so this is usually all the request
	// discloses about its target — no owner, no repo, no number. Recorded as-is
	// rather than resolved: a lookup would spend the run's credential to
	// decorate an audit row, and the row is worth more written promptly.
	Subject string

	// Unreadable, when set, names why this request resolved to no single act
	// (one of the reasons above). It never suppresses the row — an unreadable
	// write is still a write — it only means the row takes the fallback.
	Unreadable string
}

// Mutation returns the single top-level field this request invoked, or empty
// when it named anything other than exactly one. A document selecting several
// top-level mutations performs several acts through one request, which no
// single row can honestly describe, so it takes the fallback with every name
// recorded instead.
func (f GraphQLFacts) Mutation() string {
	if f.Unreadable != "" || len(f.Fields) != 1 {
		return ""
	}
	return f.Fields[0]
}

// graphQLMutations maps GitHub's mutation names onto the audit vocabulary.
// Entries are the acts `gh`'s porcelain performs and an agent might hand-write;
// anything absent records under the GraphQL fallback rather than being guessed
// at, exactly as an unrecognized REST path does.
//
// The creates carry provenance for the same reason their REST twins do:
// whether an autonomously-opened pull request or a submitted review came
// through the governed verb path or a raw call is the question the policy work
// downstream reads this log to answer. The rest deliberately do not, so their
// rows read identically to the equivalent verb's.
var graphQLMutations = map[string]graphQLShape{
	"addComment":                    {action: domain.ActionCommentPosted, creates: true},
	"updateIssueComment":            {action: domain.ActionCommentEdited},
	"deleteIssueComment":            {action: domain.ActionCommentDeleted},
	"createPullRequest":             {action: domain.ActionPRCreated, creates: true, provenance: true, numberFromURL: true},
	"updatePullRequest":             {action: domain.ActionPREdited},
	"closePullRequest":              {action: domain.ActionPRClosed},
	"reopenPullRequest":             {action: domain.ActionPRReopened},
	"mergePullRequest":              {action: domain.ActionPRMerged},
	"markPullRequestReadyForReview": {action: domain.ActionPRMarkedReady},
	"convertPullRequestToDraft":     {action: domain.ActionPRConvertedToDraft},
	"addPullRequestReview":          {action: domain.ActionReviewSubmitted, provenance: true},
	"addReaction":                   {action: domain.ActionReactionAdded},
	"removeReaction":                {action: domain.ActionReactionRemoved},

	// The porcelain sends addPullRequestReview for every review flavour —
	// approve, request-changes, and comment alike — and never this one, so it is
	// here for hand-written calls only. It maps to the same act because it IS the
	// same act: a review reaching GitHub.
	"submitPullRequestReview": {action: domain.ActionReviewSubmitted, provenance: true},

	// Arming a merge rather than performing one. A distinct mutation, not a
	// variant of mergePullRequest, so gating one has never implied the other.
	"enablePullRequestAutoMerge":  {action: domain.ActionPRAutoMergeEnabled},
	"disablePullRequestAutoMerge": {action: domain.ActionPRAutoMergeDisabled},

	// A revert opens a pull request, so it resolves like one — from the url in
	// its own payload. gh selects only the new pull request's url, which is what
	// makes that resolution unambiguous; a hand-written selection that also asked
	// for the reverted PR's url would leave the two indistinguishable here, and
	// the row would name whichever the scan reached first.
	"revertPullRequest": {action: domain.ActionPRReverted, creates: true, provenance: true, numberFromURL: true},

	"updatePullRequestBranch": {action: domain.ActionPRBranchUpdated},

	// Labels and locks address a pull request and an issue through one mutation
	// each, which is why their actions are one pair each rather than two.
	"addLabelsToLabelable":      {action: domain.ActionLabelAdded},
	"removeLabelsFromLabelable": {action: domain.ActionLabelRemoved},
	"lockLockable":              {action: domain.ActionConversationLocked},
	"unlockLockable":            {action: domain.ActionConversationUnlocked},

	// The issue family. GitHub serves issue comments through addComment above,
	// so only the lifecycle needs naming here.
	"createIssue":   {action: domain.ActionIssueCreated, creates: true, numberFromURL: true},
	"updateIssue":   {action: domain.ActionIssueUpdated},
	"closeIssue":    {action: domain.ActionIssueClosed},
	"reopenIssue":   {action: domain.ActionIssueReopened},
	"deleteIssue":   {action: domain.ActionIssueDeleted},
	"pinIssue":      {action: domain.ActionIssuePinned},
	"unpinIssue":    {action: domain.ActionIssueUnpinned},
	"transferIssue": {action: domain.ActionIssueTransferred},

	// It creates a branch, but its payload carries a ref name rather than a url,
	// so there is nothing for a create's response parse to find. The row keeps
	// the issue the branch was linked to, which is the more useful coordinate
	// anyway — a branch with no commits has no page worth linking to.
	"createLinkedBranch": {action: domain.ActionLinkedBranchCreated},

	// TODO(TFAC-788): addPullRequestReviewThread is named in that ticket's gated
	// set and is absent here because naming it needs a decision that ticket owns.
	// No porcelain path emits it, and what it does is stage a thread on a PENDING
	// review — nothing is published until the review is submitted — so every verb
	// above would overstate it, and inventing one would assert an act that has
	// not happened yet.
}

// graphQLShape is one table entry: the act, and the flags the REST shapes carry
// for the same reasons.
type graphQLShape struct {
	action     string
	creates    bool
	provenance bool
	// numberFromURL marks the one create whose new object IS the thing the
	// response's url names, so the number in that url is its id. A posted
	// comment is the counter-case: its url points at the pull request it hangs
	// off, whose number identifies that pull request and not the comment.
	numberFromURL bool
}

// ParseGraphQLRequest reads one GraphQL request body into the facts an audit
// row can be built from. ok is false when the request provably performs no
// write — the document carries no mutation keyword, or its selected operation
// is a query — which is the common case by a wide margin and the one that must
// cost nothing downstream.
//
// A body that cannot be read is not the same as a body that performs no write,
// and the two are kept apart: anything that fails to parse after the prescreen
// matched returns ok WITH an Unreadable reason, so it is recorded rather than
// dropped. Silence is reserved for requests known to be reads.
func ParseGraphQLRequest(body []byte) (GraphQLFacts, bool) {
	if !bytes.Contains(body, []byte(mutationKeyword)) {
		return GraphQLFacts{}, false
	}

	var envelope struct {
		Query         string          `json:"query"`
		OperationName string          `json:"operationName"`
		Variables     json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Query == "" {
		return GraphQLFacts{Unreadable: GraphQLMalformed}, true
	}
	subject := subjectFromVariables(envelope.Variables)

	document, parsed := parseDocument(envelope.Query)
	if !parsed {
		return GraphQLFacts{
			Operation:  envelope.OperationName,
			Subject:    subject,
			Unreadable: GraphQLMalformed,
		}, true
	}

	op, selected := selectOperation(document.ops, envelope.OperationName)
	if !selected {
		// Nothing ran — the server rejects a document it cannot resolve to one
		// operation — but a mutation was reached for, and the attempt is the
		// thing the log is for. A document with no mutation in it at all is an
		// ordinary read that happened to be ambiguous, and stays silent.
		if !containsMutation(document.ops) {
			return GraphQLFacts{}, false
		}
		return GraphQLFacts{
			Operation:  envelope.OperationName,
			Subject:    subject,
			Unreadable: GraphQLAmbiguousOperation,
		}, true
	}
	if op.typ != gqlOperationMutation {
		return GraphQLFacts{}, false
	}

	fields, resolved := resolveTopLevelFields(op.sel, document.fragments)
	facts := GraphQLFacts{Operation: op.name, Fields: fields, Subject: subject}
	switch {
	case !resolved:
		// Some part of the selection could not be accounted for, so the fields
		// that WERE read are not the whole act. They still ride the row — naming
		// what was seen beats naming nothing — but the act stays unclassified,
		// because a partial reading is exactly how a mutation gets labelled as
		// something smaller than it was.
		facts.Unreadable = GraphQLUnresolvedFragment
	case len(fields) == 0:
		// A selection set has to select something, so this document is invalid
		// and the server runs none of it. It is still recorded — a mutation was
		// reached for — but with no field to name, the reason has to stand in
		// for one, or the row would say nothing whatsoever.
		facts.Unreadable = GraphQLMalformed
	}
	return facts, true
}

// selectOperation resolves which operation of a document runs. operationName
// picks it when present; a single operation needs no name. Several operations
// with nothing to choose between them is invalid by the spec, so this reports
// no selection rather than picking the first — guessing which of two mutations
// ran would put a name in the audit log that the server may never have executed.
func selectOperation(ops []gqlOperation, operationName string) (gqlOperation, bool) {
	if operationName != "" {
		for _, op := range ops {
			if op.name == operationName {
				return op, true
			}
		}
		return gqlOperation{}, false
	}
	if len(ops) == 1 {
		return ops[0], true
	}
	return gqlOperation{}, false
}

// containsMutation reports whether any definition in the document is a
// mutation, which is what separates an unresolvable write from an unresolvable
// read.
func containsMutation(ops []gqlOperation) bool {
	for _, op := range ops {
		if op.typ == gqlOperationMutation {
			return true
		}
	}
	return false
}

// graphQLSubjectKeys are the variable names GitHub's mutation inputs use for
// the object being acted on, in the order they are preferred when a request
// carries more than one. The order runs from most specific to least: a create
// names the repository it creates into, but a mutation naming a pull request is
// about that pull request.
var graphQLSubjectKeys = []string{
	"pullRequestId",
	"subjectId",
	"issueId",
	// The interface-typed inputs: a label or a lock names its target by the
	// interface it satisfies rather than by what it is, so these are the only
	// thing those mutations disclose about the object acted on.
	"labelableId",
	"lockableId",
	"commentId",
	"repositoryId",
	"id",
}

// maxSubjectLength bounds what may be lifted out of the variables. A node id is
// tens of characters; this is loose enough for any real one and tight enough
// that a crafted variable cannot turn an audit row's target into a payload.
const maxSubjectLength = 256

// subjectFromVariables lifts the acted-on object's node id out of a request's
// variables — and nothing else. Every mutation gh sends wraps its arguments in
// an `input` object, so that is looked in first, with the top level as the
// fallback for a hand-written request.
//
// Only these specific keys, and only string values: the variables also carry
// the comment bodies and review text an agent composed, which have no business
// in an audit row and are never read.
func subjectFromVariables(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vars); err != nil {
		return ""
	}
	if input, ok := vars["input"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(input, &inner); err == nil {
			if id := pickSubject(inner); id != "" {
				return id
			}
		}
	}
	return pickSubject(vars)
}

func pickSubject(vars map[string]json.RawMessage) string {
	for _, key := range graphQLSubjectKeys {
		raw, ok := vars[key]
		if !ok {
			continue
		}
		var id string
		if err := json.Unmarshal(raw, &id); err != nil || id == "" || len(id) > maxSubjectLength {
			continue
		}
		return id
	}
	return ""
}

// maxResponseDepth bounds the walk for a created object's url. gh's selection
// sets nest two or three levels (`addComment{commentEdge{node{url}}}`); this
// leaves room for a deeper one without letting a pathological response body
// drive an unbounded descent.
const maxResponseDepth = 6

// ReadGraphQLResponse extracts the two things a GraphQL response can add to an
// audit row: whether the write actually failed, and where the object it made
// now lives.
//
// The errors array is the load-bearing half. GraphQL answers a refused mutation
// with 200 and an errors member, so without reading it a merge the server
// rejected would be logged as a merge that happened — the exact failure the
// REST side avoids by reading the status code.
//
// field names the mutation whose payload to search, so the scan starts inside
// the act's own result and cannot wander into some other part of the response.
func ReadGraphQLResponse(body []byte, field string) (errored bool, objectURL string) {
	var payload struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []json.RawMessage          `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, ""
	}
	errored = len(payload.Errors) > 0
	if field == "" {
		return errored, ""
	}
	return errored, findObjectURL(payload.Data[field], maxResponseDepth)
}

// findObjectURL descends a mutation payload for the first "url" member holding
// something that parses as a url. Only that one key is ever read: the payload
// may also carry the body text of whatever was just posted, and an audit row
// has no use for it.
func findObjectURL(raw json.RawMessage, depth int) string {
	if depth <= 0 || len(raw) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		if candidate, ok := object["url"]; ok {
			var value string
			if err := json.Unmarshal(candidate, &value); err == nil && value != "" {
				if u, err := url.Parse(value); err == nil && u.Scheme != "" && u.Host != "" {
					return value
				}
			}
		}
		for _, member := range object {
			if found := findObjectURL(member, depth-1); found != "" {
				return found
			}
		}
		return ""
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return ""
	}
	for _, member := range list {
		if found := findObjectURL(member, depth-1); found != "" {
			return found
		}
	}
	return ""
}

// classifyGraphQL names the act one GraphQL request performed. ok is false for
// a request that resolved to no single known mutation, which the caller records
// under the GraphQL fallback.
func classifyGraphQL(facts GraphQLFacts) (Shape, bool) {
	field := facts.Mutation()
	if field == "" {
		return Shape{}, false
	}
	entry, known := graphQLMutations[field]
	if !known {
		return Shape{}, false
	}
	s := Shape{
		Action:              entry.action,
		Subject:             facts.Subject,
		CreatesObject:       entry.creates,
		CarriesProvenance:   entry.provenance,
		NumberFromObjectURL: entry.numberFromURL,
	}
	if !entry.creates {
		// The act was performed ON the object the variables named, so that node
		// id is its id. A create's id belongs to the object it made, which the
		// request could not have known — it comes from the response or not at
		// all, and "not at all" is the honest answer for the mutations whose
		// selection sets return only a url.
		s.ExternalID = facts.Subject
	}
	return s, true
}

// resolveGraphQL completes a GraphQL shape with what the response disclosed.
//
// The request addressed its target by node id, which names an object without
// locating it. A response that carried the object's url locates it exactly —
// owner, repo, and number — so where one is present it replaces the node id as
// the row's coordinates, which is also what lets a raw-channel write resolve to
// an entity the way its verb-path twin does. Where none is present the node id
// stands, unresolved and honest.
func resolveGraphQL(obs Observation) (Shape, bool) {
	s, ok := classifyGraphQL(*obs.GraphQL)
	if !ok {
		return Shape{}, false
	}
	if owner, repo, number, parsed := ParseObjectURL(obs.URL); parsed {
		s.Owner, s.Repo, s.Number = owner, repo, number
		if s.NumberFromObjectURL {
			s.ExternalID = strconv.Itoa(number)
		}
	}
	return s, true
}
