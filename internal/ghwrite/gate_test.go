package ghwrite

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGate_REST pins the policy's REST half against the shapes that reach it,
// and — the half that matters more day to day — against everything adjacent to
// them that must not be touched. A gate that refuses a read, or a write nobody
// decided to refuse, costs more than it saves.
func TestGate_REST(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantReason string
		wantAction string
	}{
		// --- refused ---
		{"a review post", "POST", "/repos/acme/widgets/pulls/7/reviews", GateReasonReview, domain.ActionReviewSubmitted},
		{"a review behind the GHE prefix", "POST", "/api/v3/repos/acme/widgets/pulls/7/reviews", GateReasonReview, domain.ActionReviewSubmitted},
		{"a review with a trailing slash", "POST", "/repos/acme/widgets/pulls/7/reviews/", GateReasonReview, domain.ActionReviewSubmitted},
		{
			// Normalized before it is matched, because a router that resolves
			// this and a scan that does not is exactly one bypass.
			"a review reached through a dot-dot segment", "POST",
			"/repos/acme/widgets/pulls/7/x/../reviews", GateReasonReview, domain.ActionReviewSubmitted,
		},
		{"the personal repo collection", "POST", "/user/repos", GateReasonRepoCreate, domain.ActionRepoCreated},
		{"the org repo collection", "POST", "/orgs/acme/repos", GateReasonRepoCreate, domain.ActionRepoCreated},
		{"the org collection behind the GHE prefix", "POST", "/api/v3/orgs/acme/repos", GateReasonRepoCreate, domain.ActionRepoCreated},
		{"the template endpoint", "POST", "/repos/acme/tmpl/generate", GateReasonRepoCreate, domain.ActionRepoCreated},

		// --- untouched ---
		//
		// Merging leads this list because its absence is a decision rather than
		// an omission: it is gated by the delegated runtime's merge question,
		// which promises that a re-issue proceeds, and this is what keeps that
		// promise true.
		{name: "a merge", method: "PUT", path: "/repos/acme/widgets/pulls/7/merge"},
		{name: "reading a PR", method: "GET", path: "/repos/acme/widgets/pulls/7"},
		{name: "asking whether a PR is merged", method: "GET", path: "/repos/acme/widgets/pulls/7/merge"},
		{name: "listing repositories", method: "GET", path: "/user/repos"},
		{name: "listing an org's repositories", method: "GET", path: "/orgs/acme/repos"},
		// Different resources that merely read alike: /merges combines two
		// branches and /merge-upstream syncs a fork. Neither is the act.
		{name: "the branch-merge endpoint", method: "POST", path: "/repos/acme/widgets/merges"},
		{name: "syncing a fork", method: "POST", path: "/repos/acme/widgets/merge-upstream"},
		{name: "editing a PR", method: "PATCH", path: "/repos/acme/widgets/pulls/7"},
		{name: "posting a comment", method: "POST", path: "/repos/acme/widgets/issues/7/comments"},
		{name: "replying on a review thread", method: "POST", path: "/repos/acme/widgets/pulls/7/comments/5/replies"},
		{name: "opening a PR", method: "POST", path: "/repos/acme/widgets/pulls"},
		{name: "updating a PR branch", method: "PUT", path: "/repos/acme/widgets/pulls/7/update-branch"},
		// A repository whose name is literally "repos", under an owner named
		// "user": a repo-scoped path, not the collection that mints one.
		{name: "a repo-scoped path that reads like the collection", method: "POST", path: "/repos/user/repos/issues"},
		{name: "editing a repository", method: "PATCH", path: "/repos/acme/widgets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, gated := Gate(Request{Method: tc.method, Path: tc.path})
			if gated != (tc.wantReason != "") {
				t.Fatalf("Gate(%s %s) gated = %v, want %v (%+v)", tc.method, tc.path, gated, tc.wantReason != "", ref)
			}
			if ref.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", ref.Reason, tc.wantReason)
			}
			if ref.Action != tc.wantAction {
				t.Errorf("action = %q, want %q", ref.Action, tc.wantAction)
			}
		})
	}
}

// TestGate_GraphQL pins the mutation half, including the two members whose
// gating does not come from the classifier at all: addPullRequestReviewThread,
// which the audit table deliberately cannot name, and every request whose act
// could not be established.
func TestGate_GraphQL(t *testing.T) {
	tests := []struct {
		name       string
		facts      GraphQLFacts
		wantReason string
		wantAction string
	}{
		{
			name: "submitting a review", facts: GraphQLFacts{Fields: []string{"addPullRequestReview"}},
			wantReason: GateReasonReview, wantAction: domain.ActionReviewSubmitted,
		},
		{
			name: "submitting a pending review", facts: GraphQLFacts{Fields: []string{"submitPullRequestReview"}},
			wantReason: GateReasonReview, wantAction: domain.ActionReviewSubmitted,
		},
		{
			// Gated by name, and by name only: it stages a thread on a pending
			// review, so the audit table has no verb for it and inventing one
			// would assert an act that has not happened.
			name: "staging a review thread", facts: GraphQLFacts{Fields: []string{"addPullRequestReviewThread"}},
			wantReason: GateReasonReview,
		},
		{
			name: "creating a repository", facts: GraphQLFacts{Fields: []string{"createRepository"}},
			wantReason: GateReasonRepoCreate, wantAction: domain.ActionRepoCreated,
		},
		{
			name: "cloning a template", facts: GraphQLFacts{Fields: []string{"cloneTemplateRepository"}},
			wantReason: GateReasonRepoCreate, wantAction: domain.ActionRepoCreated,
		},
		{
			// Every top-level field is checked, not just the single-field case
			// the audit builder needs. A document selecting a comment AND a
			// review performs the review, and the row it would take is beside
			// the point.
			name:       "a review hidden among several fields",
			facts:      GraphQLFacts{Fields: []string{"addComment", "updateIssue", "addPullRequestReview"}},
			wantReason: GateReasonReview, wantAction: domain.ActionReviewSubmitted,
		},
		// The merge family, ungated by decision — see the file comment on
		// gate.go. Arming one follows performing one: gating the cautious
		// spelling while the direct one transits would be backwards.
		{name: "a merge", facts: GraphQLFacts{Fields: []string{"mergePullRequest"}}},
		{name: "arming a merge", facts: GraphQLFacts{Fields: []string{"enablePullRequestAutoMerge"}}},
		{name: "disarming a merge", facts: GraphQLFacts{Fields: []string{"disablePullRequestAutoMerge"}}},
		{name: "a comment", facts: GraphQLFacts{Fields: []string{"addComment"}}},
		{name: "closing a PR", facts: GraphQLFacts{Fields: []string{"closePullRequest"}}},
		{name: "opening a PR", facts: GraphQLFacts{Fields: []string{"createPullRequest"}}},
		{name: "a mutation outside the table", facts: GraphQLFacts{Fields: []string{"followUser"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := tc.facts
			ref, gated := Gate(Request{Method: "POST", Path: "/api/graphql", GraphQL: &facts})
			if gated != (tc.wantReason != "") {
				t.Fatalf("gated = %v, want %v (%+v)", gated, tc.wantReason != "", ref)
			}
			if ref.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", ref.Reason, tc.wantReason)
			}
			if ref.Action != tc.wantAction {
				t.Errorf("action = %q, want %q", ref.Action, tc.wantAction)
			}
		})
	}
}

// TestGate_EveryUnreadableReasonIsRefused is the resolution of R3's ambiguity
// for this transport, in one assertion: whether a request is provably a
// mutation or merely undecidable, if its act cannot be established it is
// refused. Leaving any of these open makes the way past the gate a property of
// the request's size or syntax rather than of what it does.
func TestGate_EveryUnreadableReasonIsRefused(t *testing.T) {
	reasons := []string{
		GraphQLOverCap,
		GraphQLRequestUnread,
		GraphQLMalformed,
		GraphQLAmbiguousOperation,
		GraphQLUnresolvedFragment,
		GraphQLTooManyFields,
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			facts := GraphQLFacts{Unreadable: reason}
			ref, gated := Gate(Request{Method: "POST", Path: "/api/graphql", GraphQL: &facts})
			if !gated {
				t.Fatalf("an unreadable request (%s) was not refused", reason)
			}
			if ref.Reason != GateReasonUnreadable || ref.Unreadable != reason {
				t.Errorf("refusal = %+v, want the unreadable band carrying %q", ref, reason)
			}
		})
	}
}

// TestGate_UnreadableWinsOverAnyFieldsRead: a partial reading names fields, and
// naming SOME of what a document selects is not the same as knowing it selects
// nothing gated. The unreadable reason has to decide first, or a document that
// hides its merge behind an undefined fragment while selecting a harmless
// comment in the open would read as a comment.
func TestGate_UnreadableWinsOverAnyFieldsRead(t *testing.T) {
	facts := GraphQLFacts{Fields: []string{"addComment"}, Unreadable: GraphQLUnresolvedFragment}
	ref, gated := Gate(Request{Method: "POST", Path: "/api/graphql", GraphQL: &facts})
	if !gated || ref.Reason != GateReasonUnreadable {
		t.Fatalf("refusal = %+v, gated = %v; want the unreadable band to decide", ref, gated)
	}
}

// TestExplain_SaysWhatToDoInstead: a refusal a model cannot act on is just
// pressure to retry creatively, so every family names the act, states the rule
// as admitting no exception, and gives a next step.
func TestExplain_SaysWhatToDoInstead(t *testing.T) {
	refusals := []Refusal{
		{Reason: GateReasonReview, Action: domain.ActionReviewSubmitted},
		{Reason: GateReasonRepoCreate, Action: domain.ActionRepoCreated},
		{Reason: GateReasonUnreadable, Unreadable: GraphQLOverCap},
	}
	for _, ref := range refusals {
		t.Run(ref.Reason, func(t *testing.T) {
			e := ref.Explain()
			if e.Reason != ref.Reason {
				t.Errorf("reason = %q, want %q", e.Reason, ref.Reason)
			}
			if e.Policy == "" || e.NextStep == "" {
				t.Fatalf("explanation = %+v, want both a policy and a next step", e)
			}
			if e.Refused != ref.Action {
				t.Errorf("refused = %q, want the act %q", e.Refused, ref.Action)
			}
		})
	}

	// The mutation stands in as the act's name where the classifier resolves
	// none, so a refusal is never anonymous when something CAN name it.
	staged := Refusal{Reason: GateReasonReview, Mutation: "addPullRequestReviewThread"}
	if got := staged.Explain().Refused; got != "addPullRequestReviewThread" {
		t.Errorf("refused = %q, want the mutation name to stand in for the missing verb", got)
	}

	// The one case where nothing can be named — which is the whole reason it
	// is refused, and the explanation says so rather than leaving the model to
	// guess which of its calls was the problem.
	unreadable := Refusal{Reason: GateReasonUnreadable, Unreadable: GraphQLOverCap}.Explain()
	if unreadable.Refused != "" || unreadable.Unreadable != GraphQLOverCap {
		t.Errorf("explanation = %+v, want no act and the reason it could not be named", unreadable)
	}
	if !strings.Contains(unreadable.NextStep, "size") {
		t.Errorf("next step = %q, want it to say a readable request is not refused for its size",
			unreadable.NextStep)
	}
}

// TestGate_EveryGatedActionIsInTheClassifier keeps the two tables honest about
// each other. An act named here that the classifier never produces would gate
// nothing at all, and would look for all the world like it did.
func TestGate_EveryGatedActionIsInTheClassifier(t *testing.T) {
	produced := map[string]bool{
		// The REST half, by the one shape each act is reached through.
		domain.ActionReviewSubmitted: true,
	}
	for _, entry := range graphQLMutations {
		produced[entry.action] = true
	}
	for action := range gatedActions {
		if !produced[action] {
			t.Errorf("gated action %q is produced by neither table — it gates nothing", action)
		}
	}
	// And the reverse for the by-name arm: a mutation gated by name must NOT be
	// in the classifier, or the act-keyed arm already covers it and the special
	// case is dead weight pretending to be policy.
	for name := range gatedMutations {
		if _, known := graphQLMutations[name]; known {
			t.Errorf("mutation %q is gated by name but the classifier resolves it; gate the act instead", name)
		}
	}
}
