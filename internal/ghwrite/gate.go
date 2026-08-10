package ghwrite

import (
	"path"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The refusal policy, keyed on the same table the audit builder reads.
//
// It lives beside the classifier rather than in the proxy that enforces it for
// one reason: the question "which act is this?" must have exactly one answer in
// this system. A second table — a list of paths in the proxy, a list of
// mutation names in a policy file — would drift from this one, and the drift
// would show up as a merge that audits as a merge and gates as nothing.
//
// # What the gate refuses, and why those
//
// Three families, and one band that is not a family at all.
//
// MERGE. Merging is irreversible, no human is watching a delegated run, and a
// run that merges its own work has removed the review step the product is built
// around. Arming a merge is gated in its own right: enablePullRequestAutoMerge
// is a distinct mutation that performs no merge now and lands one later with
// nobody present, which is the same act with the supervision removed rather
// than a lesser one. Disarming is not gated — dropping a queued merge is the
// safe direction.
//
// REVIEW. The `gh pr` review verbs Triage Factory provides strictly dominate a
// review posted through this channel: they anchor comments to lines, badge them
// by severity, stage the review as a draft, and route the verdict through human
// approval. A review posted raw has none of that and cannot be revised or
// withdrawn.
//
// REPOSITORY CREATION. The tracked repository set is chosen before a run starts
// and is what scopes polling, task routing, and the run's own credential. A
// repository that appears mid-run belongs to none of it.
//
// The band is the GraphQL requests that cannot be named at all — see gateGraphQL.
//
// # No authorization, deliberately
//
// There is no exemption: no per-run bit, no trigger field, no override. A
// mission that legitimately needs a gated act cannot perform it, and finishes
// by saying so. That is the policy, not a placeholder for one — an exemption
// path built before anyone has asked for it gets used because it exists, and
// the refusal it was built around stops holding.
const (
	// GateReasonMerge covers performing a merge and arming one to happen later.
	GateReasonMerge = "merge"
	// GateReasonReview covers submitting a review and staging a thread on one.
	GateReasonReview = "review"
	// GateReasonRepoCreate covers minting a repository, by any of the four
	// spellings GitHub offers.
	GateReasonRepoCreate = "repo_create"
	// GateReasonUnreadable covers a GraphQL request whose act could not be
	// established. It is not a family — nobody knows what was refused, which is
	// precisely why it is refused (see gateGraphQL).
	GateReasonUnreadable = "unreadable"
)

// Request is one request as the gate sees it: before it is forwarded, so there
// is no status and no response body. GraphQL is non-nil exactly when the
// request went to the GraphQL endpoint and its document parsed far enough to
// say something — including saying only that it could not be read.
type Request struct {
	Method string
	Path   string

	GraphQL *GraphQLFacts
}

// Refusal is one gate decision against a request. Reason is always set; the
// rest say as much as could be established about what was reached for, which
// for the unreadable band is nothing but the reason it could not be read.
type Refusal struct {
	// Reason is the family, one of the GateReason consts.
	Reason string

	// Action is the classifier's verb for the refused act, empty when the
	// request named an act the table does not resolve.
	Action string

	// Mutation is the GraphQL field the request selected, empty for REST.
	Mutation string

	// Unreadable carries the classifier's own reason (the GraphQL* consts) when
	// the refusal is for a request that could not be named at all.
	Unreadable string
}

// gatedActions maps the classified act onto the family that refuses it. Keying
// on the ACT rather than on a path or a mutation name is what makes the gate
// cover spellings nobody enumerated: a merge performed through `gh pr merge`,
// through `gh api`, and through a hand-written GraphQL document are one entry
// here because the classifier already resolves all three to one verb.
//
// TODO(TFAC-784): pr_created is absent pending that ticket's decision on
// whether opening a pull request is governed here, through the verb path's
// approval queue, or by draft-until-ready. Until it lands, a run opens pull
// requests through this channel unrefused.
var gatedActions = map[string]string{
	domain.ActionPRMerged:           GateReasonMerge,
	domain.ActionPRAutoMergeEnabled: GateReasonMerge,
	domain.ActionReviewSubmitted:    GateReasonReview,
	domain.ActionRepoCreated:        GateReasonRepoCreate,
}

// gatedMutations are the GraphQL mutations gated by NAME because the classifier
// resolves them to no act.
//
// One entry, and it is not an oversight in either table. addPullRequestReviewThread
// stages a thread on a PENDING review: no porcelain path emits it, and nothing
// it does is published until the review is submitted, so every verb the audit
// vocabulary owns would overstate it. It gets no verb here either, and needs
// none — the table names acts that HAPPENED, and this gate refuses this mutation
// unconditionally, so a successful one is not a thing that occurs. A verb minted
// for it would describe an event the system cannot produce.
var gatedMutations = map[string]string{
	"addPullRequestReviewThread": GateReasonReview,
}

// Gate reports whether this request is a shape a run may not perform. ok is
// false for everything else, which is nearly all traffic: every read, and every
// write outside the three families.
func Gate(req Request) (Refusal, bool) {
	if req.GraphQL != nil {
		return gateGraphQL(*req.GraphQL)
	}
	return gateREST(req.Method, req.Path)
}

// gateREST decides a REST request from its method and path.
//
// The path is normalized before it is matched. A gate that matched the raw path
// would be walked past by any spelling the upstream router resolves and this
// scan does not — `/repos/o/r/pulls/7/x/../merge` being the cheap one — and
// normalizing can only ever refuse more, never less.
func gateREST(method, requestPath string) (Refusal, bool) {
	clean := path.Clean(requestPath)
	// The two repository collections that mint one are checked first and by
	// path, because they are the gated set's only members the classifier cannot
	// name: they post OUTSIDE /repos/{owner}/{repo}/, which is the anchor the
	// whole REST table hangs off, and there are no coordinates in them to
	// classify against.
	if method == "POST" && mintsRepository(clean) {
		return Refusal{Reason: GateReasonRepoCreate, Action: domain.ActionRepoCreated}, true
	}
	shape, ok := Classify(method, clean)
	if !ok {
		return Refusal{}, false
	}
	reason, gated := gatedActions[shape.Action]
	if !gated {
		return Refusal{}, false
	}
	return Refusal{Reason: reason, Action: shape.Action}, true
}

// gateGraphQL decides a GraphQL request from what its document disclosed.
//
// # The unreadable band
//
// A gate keyed on the act's name is walked through by anything that stops the
// act being named, and on this transport the act lives in the body — so "which
// shape is this?" and "can we name it at all?" are different questions, and only
// the first has an answer here. Every request in the second case is refused.
//
// Two of those cases are strictly correct and cost nothing: an unresolvable
// fragment spread and an ambiguous operation both arise only AFTER an operation
// was parsed and selected as a mutation, so refusing them touches no read
// whatsoever.
//
// The rest — an over-cap body, a read that broke mid-stream, a document this
// reader could not parse — are undecidable rather than provably writes, and are
// refused as a deliberate trade. The band contains no real reads: a read sends a
// small document and receives a large response, and the response direction is
// uncapped for delivery. Against a megabyte ceiling, the largest document the
// pinned `gh` sends is measured in hundreds of bytes. Leaving the band open
// would make the buffer size the bypass, which is the whole reason a gated shape
// fails closed on an unreadable body in the first place.
//
// # Why every field, not the single-field case
//
// The audit builder names an act only when the document selected exactly one
// top-level field, because no single row honestly describes several. The gate
// has no such luxury: a document selecting a comment AND a merge performs the
// merge.
func gateGraphQL(facts GraphQLFacts) (Refusal, bool) {
	if facts.Unreadable != "" {
		return Refusal{Reason: GateReasonUnreadable, Unreadable: facts.Unreadable}, true
	}
	for _, field := range facts.Fields {
		if reason, gated := gatedMutations[field]; gated {
			return Refusal{Reason: reason, Mutation: field}, true
		}
		entry, known := graphQLMutations[field]
		if !known {
			continue
		}
		if reason, gated := gatedActions[entry.action]; gated {
			return Refusal{Reason: reason, Action: entry.action, Mutation: field}, true
		}
	}
	return Refusal{}, false
}

// mintsRepository reports whether a path is one of the two collections a
// repository is POSTed into. The template-instantiation endpoint is not here:
// it addresses a repository, so the classifier names it repo_created and the
// act-keyed arm above covers it.
//
// The optional /api/v3 prefix is stripped rather than tolerated loosely,
// because these shapes are matched by their whole path: /user/repos and
// /repos/{owner}/{repo} both end in two segments, and only the leading
// collection tells them apart.
func mintsRepository(requestPath string) bool {
	trimmed := strings.TrimPrefix(requestPath, gheRESTPrefix)
	segs := strings.Split(strings.Trim(trimmed, "/"), "/")
	switch {
	case len(segs) == 2 && segs[0] == "user" && segs[1] == "repos":
		return true
	case len(segs) == 3 && segs[0] == "orgs" && segs[1] != "" && segs[2] == "repos":
		return true
	}
	return false
}

// gheRESTPrefix is the path prefix `gh` emits for REST when it believes it is
// talking to a GitHub Enterprise host, which is what the injector presents. It
// appears here (and not in the rest of this package) because these two shapes
// are matched whole; every other REST shape is anchored on a segment that
// absorbs the prefix.
const gheRESTPrefix = "/api/v3"

// Explanation is a refusal as the agent reads it: what was refused, why, and
// what to do instead. It is the body of the synthesized 403, and it is
// structured because a refusal a model cannot parse is just pressure to retry
// creatively.
type Explanation struct {
	// Refused names the act, in the audit log's own vocabulary where there is
	// one. Empty for the unreadable band, where naming it is the thing that
	// could not be done.
	Refused string `json:"refused,omitempty"`
	// Reason is the family (a GateReason const).
	Reason string `json:"reason"`
	// Unreadable carries why a request could not be named, for that band only.
	Unreadable string `json:"unreadable,omitempty"`
	// Policy states the rule in full, including that it admits no exception —
	// a model that reads a refusal as a hurdle spends the rest of the run
	// looking for the way around it.
	Policy string `json:"policy"`
	// NextStep is what to do instead. Always populated: for the three families
	// there is a real alternative, and for the unreadable band the alternative
	// is a request that can be read.
	NextStep string `json:"next_step"`
}

// Explain renders the refusal for the agent that hit it.
func (r Refusal) Explain() Explanation {
	e := Explanation{
		Refused:    r.Action,
		Reason:     r.Reason,
		Unreadable: r.Unreadable,
	}
	if e.Refused == "" {
		e.Refused = r.Mutation
	}
	switch r.Reason {
	case GateReasonMerge:
		e.Policy = "Triage Factory refuses merges made with this run's credential — performing one, " +
			"and arming one to happen later. This is unconditional: no mission, instruction, or " +
			"authorization enables it, and re-sending the request by another route will be refused too."
		e.NextStep = "Finish the work and push the branch. Say in your final message that you believe it " +
			"is ready to merge, and leave the merge to a human."
	case GateReasonReview:
		e.Policy = "Triage Factory refuses pull-request reviews submitted through the `gh` credential " +
			"channel. Its own review verbs do everything this call does and more — a comment anchored to " +
			"a line, a severity badge, a draft you can revise, and a verdict that goes through human " +
			"approval — and a review posted here would have none of it."
		e.NextStep = "Use the Triage Factory review verbs instead: `gh pr start-review`, then " +
			"`add-review-comment` for each finding, then `finalize-review`."
	case GateReasonRepoCreate:
		e.Policy = "Triage Factory refuses repository creation with this run's credential. Polling, task " +
			"routing, and this run's own credential are all scoped to a set of repositories chosen before " +
			"the run started, and a repository created now belongs to none of it."
		e.NextStep = "If your mission genuinely needs a repository that does not exist, say so in your " +
			"final message and stop there."
	case GateReasonUnreadable:
		e.Policy = "Triage Factory could not read this GraphQL request, so it could not be shown to be a " +
			"read rather than one of the writes this channel refuses. Requests whose act cannot be " +
			"established are refused."
		e.NextStep = "Send one named operation in a document small enough to read, with every fragment it " +
			"spreads defined in the same document. A read that parses is never refused for its size."
	}
	return e
}
