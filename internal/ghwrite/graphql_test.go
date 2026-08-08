package ghwrite

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// envelope builds the request body shape gh sends: the document, the variables,
// and optionally an operationName.
func envelope(t *testing.T, query, operationName string, variables any) []byte {
	t.Helper()
	body := map[string]any{"query": query}
	if operationName != "" {
		body["operationName"] = operationName
	}
	if variables != nil {
		body["variables"] = variables
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// input wraps variables the way every mutation gh sends does.
func input(fields map[string]any) map[string]any { return map[string]any{"input": fields} }

// TestParseGraphQLRequest_ReadsRecordNothing is the acceptance floor and the
// performance one at once: the endpoint's traffic is overwhelmingly reads, and
// every one of them must resolve to "no write happened" — not to an unnamed row
// someone has to triage.
func TestParseGraphQLRequest_ReadsRecordNothing(t *testing.T) {
	reads := map[string]string{
		"named query": `query PullRequestByNumber($owner:String!){repository(owner:$owner){pullRequest(number:42){id url}}}`,
		"shorthand":   `{viewer{login}}`,
		"anonymous":   `query{repository(owner:"o",name:"r"){id}}`,
		// The prescreen is a substring scan, so a read that merely mentions the
		// word must still resolve to silence once it is actually parsed.
		"the word in a field name": `query{repository(owner:"o",name:"r"){mutationLog{id}}}`,
		"the word in a string":     `query{search(query:"mutation",type:ISSUE,first:1){issueCount}}`,
	}
	for name, query := range reads {
		t.Run(name, func(t *testing.T) {
			if facts, ok := ParseGraphQLRequest(envelope(t, query, "", nil)); ok {
				t.Errorf("read produced facts %+v, want none — no read may ever leave a row", facts)
			}
		})
	}
}

// TestParseGraphQLRequest_GHPorcelain pins the documents the real gh sends for
// the writes an agent reaches for most. These are the wire shapes the library
// gh builds its mutations with emits — anonymous operations, arguments wrapped
// in a single $input — and getting them wrong is getting every porcelain write
// wrong.
func TestParseGraphQLRequest_GHPorcelain(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		variables any
		mutation  string
		action    string
		subject   string
	}{
		{
			name:      "pr comment",
			query:     `mutation CommentCreate($input:AddCommentInput!){addComment(input:$input){commentEdge{node{url}}}}`,
			variables: input(map[string]any{"body": "looks good", "subjectId": "PR_kwComment"}),
			mutation:  "addComment",
			action:    domain.ActionCommentPosted,
			subject:   "PR_kwComment",
		},
		{
			name:      "pr close",
			query:     `mutation($input:ClosePullRequestInput!){closePullRequest(input:$input){pullRequest{id}}}`,
			variables: input(map[string]any{"pullRequestId": "PR_kwClose"}),
			mutation:  "closePullRequest",
			action:    domain.ActionPRClosed,
			subject:   "PR_kwClose",
		},
		{
			name:      "pr merge",
			query:     `mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`,
			variables: input(map[string]any{"pullRequestId": "PR_kwMerge"}),
			mutation:  "mergePullRequest",
			action:    domain.ActionPRMerged,
			subject:   "PR_kwMerge",
		},
		{
			name:      "pr create",
			query:     `mutation PullRequestCreate($input:CreatePullRequestInput!){createPullRequest(input:$input){pullRequest{id url}}}`,
			variables: input(map[string]any{"repositoryId": "R_kwCreate", "title": "T", "body": "B"}),
			mutation:  "createPullRequest",
			action:    domain.ActionPRCreated,
			subject:   "R_kwCreate",
		},
		{
			name:      "pr review",
			query:     `mutation($input:AddPullRequestReviewInput!){addPullRequestReview(input:$input){clientMutationId}}`,
			variables: input(map[string]any{"pullRequestId": "PR_kwReview", "event": "COMMENT"}),
			mutation:  "addPullRequestReview",
			action:    domain.ActionReviewSubmitted,
			subject:   "PR_kwReview",
		},
		{
			name:      "pr ready for review",
			query:     `mutation($input:MarkPullRequestReadyForReviewInput!){markPullRequestReadyForReview(input:$input){pullRequest{id}}}`,
			variables: input(map[string]any{"pullRequestId": "PR_kwReady"}),
			mutation:  "markPullRequestReadyForReview",
			action:    domain.ActionPRMarkedReady,
			subject:   "PR_kwReady",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts, ok := ParseGraphQLRequest(envelope(t, tc.query, "", tc.variables))
			if !ok {
				t.Fatalf("%s produced no facts; a porcelain write must always be recorded", tc.name)
			}
			if facts.Unreadable != "" {
				t.Fatalf("facts = %+v, want a cleanly read mutation", facts)
			}
			if got := facts.Mutation(); got != tc.mutation {
				t.Errorf("mutation = %q, want %q", got, tc.mutation)
			}
			if facts.Subject != tc.subject {
				t.Errorf("subject = %q, want the node id %q the variables named", facts.Subject, tc.subject)
			}
			shape, classified := classifyGraphQL(facts)
			if !classified {
				t.Fatalf("%s did not classify; it would land an unnamed row", tc.mutation)
			}
			if shape.Action != tc.action {
				t.Errorf("action = %q, want %q", shape.Action, tc.action)
			}
			if shape.Target() != tc.subject {
				t.Errorf("target = %q, want the node id %q when nothing located the object", shape.Target(), tc.subject)
			}
		})
	}
}

// TestParseGraphQLRequest_AliasCannotHideTheField is the evasion the response
// side structurally cannot catch: a response keyed under `x` names nothing, so
// if the alias were not resolved in the REQUEST, a merge could be performed
// under any label the caller liked and audited as nothing at all.
func TestParseGraphQLRequest_AliasCannotHideTheField(t *testing.T) {
	query := `mutation Sneaky($input:MergePullRequestInput!){x:mergePullRequest(input:$input){clientMutationId}}`
	facts, ok := ParseGraphQLRequest(envelope(t, query, "", input(map[string]any{"pullRequestId": "PR_kwAlias"})))
	if !ok {
		t.Fatal("an aliased mutation produced no facts")
	}
	if got := facts.Mutation(); got != "mergePullRequest" {
		t.Fatalf("mutation = %q, want the real field mergePullRequest — the alias must not stand in for it", got)
	}
	shape, classified := classifyGraphQL(facts)
	if !classified || shape.Action != domain.ActionPRMerged {
		t.Errorf("aliased merge classified as %+v (ok=%v), want pr_merged", shape, classified)
	}
}

// TestParseGraphQLRequest_OperationNameSelects covers the document that carries
// several operations. Which one ran is a fact only operationName supplies, and
// naming the wrong one would put an act in the log that the server never
// executed.
func TestParseGraphQLRequest_OperationNameSelects(t *testing.T) {
	const doc = `query Look($n:Int!){repository(owner:"o",name:"r"){pullRequest(number:$n){id}}}
	mutation Close($input:ClosePullRequestInput!){closePullRequest(input:$input){pullRequest{id}}}`

	t.Run("selects the mutation", func(t *testing.T) {
		facts, ok := ParseGraphQLRequest(envelope(t, doc, "Close", input(map[string]any{"pullRequestId": "PR_x"})))
		if !ok || facts.Unreadable != "" {
			t.Fatalf("facts = %+v (ok=%v), want the named mutation read cleanly", facts, ok)
		}
		if got := facts.Mutation(); got != "closePullRequest" {
			t.Errorf("mutation = %q, want closePullRequest", got)
		}
	})

	t.Run("selects the query", func(t *testing.T) {
		// The document contains a mutation, but the operation that RAN is the
		// query — so this is a read and must leave nothing behind.
		if facts, ok := ParseGraphQLRequest(envelope(t, doc, "Look", nil)); ok {
			t.Errorf("facts = %+v, want none — operationName selected the query", facts)
		}
	})

	t.Run("no operationName is spec-invalid", func(t *testing.T) {
		facts, ok := ParseGraphQLRequest(envelope(t, doc, "", nil))
		if !ok {
			t.Fatal("an ambiguous document produced no facts; a reached-for mutation must still be recorded")
		}
		if facts.Unreadable != GraphQLAmbiguousOperation {
			t.Errorf("unreadable = %q, want %q — with nothing to choose by, no act may be named",
				facts.Unreadable, GraphQLAmbiguousOperation)
		}
		if facts.Mutation() != "" {
			t.Errorf("mutation = %q, want empty; guessing which operation ran is the failure mode here", facts.Mutation())
		}
	})

	t.Run("operationName naming nothing", func(t *testing.T) {
		facts, ok := ParseGraphQLRequest(envelope(t, doc, "Absent", nil))
		if !ok || facts.Unreadable != GraphQLAmbiguousOperation {
			t.Errorf("facts = %+v (ok=%v), want an ambiguous-operation record", facts, ok)
		}
	})

	t.Run("ambiguous read stays silent", func(t *testing.T) {
		// Two operations, neither a mutation: unresolvable, but a read, and the
		// log is for writes.
		const reads = `query A{viewer{login}} query B{viewer{id}} # mutation`
		if facts, ok := ParseGraphQLRequest(envelope(t, reads, "", nil)); ok {
			t.Errorf("facts = %+v, want none for an ambiguous read", facts)
		}
	})
}

// TestParseGraphQLRequest_FragmentsAreResolved closes the cheapest way to
// perform a write the log cannot name. Moving a mutation behind a fragment is a
// few hundred bytes of ordinary GraphQL that `gh` never emits and a hand-written
// call may, and leaving it unresolved made an unnamed row free to obtain — a far
// easier evasion than padding a request past the buffering cap.
//
// So a top-level spread is followed into its definition and an inline fragment's
// selections are read at the level they belong to, and the act comes out named
// either way.
func TestParseGraphQLRequest_FragmentsAreResolved(t *testing.T) {
	cases := map[string]struct {
		query    string
		mutation string
		action   string
	}{
		"named spread": {
			query: `mutation M($input:MergePullRequestInput!){...Merge}
				fragment Merge on Mutation{mergePullRequest(input:$input){clientMutationId}}`,
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
		},
		"inline fragment": {
			query:    `mutation M($input:MergePullRequestInput!){... on Mutation{mergePullRequest(input:$input){clientMutationId}}}`,
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
		},
		"inline fragment with no type condition": {
			query:    `mutation M($input:MergePullRequestInput!){...{mergePullRequest(input:$input){clientMutationId}}}`,
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
		},
		"spread through a chain": {
			query: `mutation M($input:MergePullRequestInput!){...A}
				fragment A on Mutation{...B}
				fragment B on Mutation{mergePullRequest(input:$input){clientMutationId}}`,
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
		},
		"aliased inside a fragment": {
			query: `mutation M($input:MergePullRequestInput!){...Merge}
				fragment Merge on Mutation{x:mergePullRequest(input:$input){clientMutationId}}`,
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			facts, ok := ParseGraphQLRequest(envelope(t, tc.query, "", nil))
			if !ok {
				t.Fatal("a mutation behind a fragment produced no facts; it is still a write")
			}
			if facts.Unreadable != "" {
				t.Fatalf("facts = %+v, want the spread followed rather than given up on", facts)
			}
			if got := facts.Mutation(); got != tc.mutation {
				t.Errorf("mutation = %q, want %q", got, tc.mutation)
			}
			shape, classified := classifyGraphQL(facts)
			if !classified || shape.Action != tc.action {
				t.Errorf("classified %+v (ok=%v), want %q", shape, classified, tc.action)
			}
		})
	}

	t.Run("spread beside a field is two acts", func(t *testing.T) {
		// Both are real, so both are named, and one row cannot claim to be
		// either — the same rule a request naming two mutations directly gets.
		query := `mutation M($input:AddCommentInput!){addComment(input:$input){clientMutationId} ...Extra}
			fragment Extra on Mutation{mergePullRequest(input:$input){clientMutationId}}`
		facts, ok := ParseGraphQLRequest(envelope(t, query, "", nil))
		if !ok {
			t.Fatal("no facts for a mutation selecting a field and a spread")
		}
		if len(facts.Fields) != 2 || facts.Fields[0] != "addComment" || facts.Fields[1] != "mergePullRequest" {
			t.Errorf("fields = %v, want both acts named", facts.Fields)
		}
		if _, classified := classifyGraphQL(facts); classified {
			t.Error("a two-act request classified as one")
		}
	})
}

// TestParseGraphQLRequest_FragmentsThatCannotBeFollowed pins the other half:
// resolution that cannot complete degrades to the honest unresolved record it
// always produced, never to a partial reading. A document that defeats the
// resolver must not thereby get itself a smaller-sounding name.
func TestParseGraphQLRequest_FragmentsThatCannotBeFollowed(t *testing.T) {
	cases := map[string]string{
		// The fragment is never defined, so the server rejects the document —
		// but a mutation was reached for and the reach is what is recorded.
		"undefined fragment": `mutation M{...Missing}`,
		// Conditioned on a type that is not the mutation root: the server never
		// takes this branch, so reading its fields would name an act that did
		// not happen.
		"fragment on the wrong type": `mutation M($input:MergePullRequestInput!){...Wrong}
			fragment Wrong on Repository{mergePullRequest(input:$input){clientMutationId}}`,
		"inline fragment on the wrong type": `mutation M($input:MergePullRequestInput!){... on Repository{mergePullRequest(input:$input){clientMutationId}}}`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			facts, ok := ParseGraphQLRequest(envelope(t, query, "", nil))
			if !ok {
				t.Fatal("an unfollowable spread produced no facts; a write was still reached for")
			}
			if facts.Unreadable != GraphQLUnresolvedFragment {
				t.Errorf("unreadable = %q, want %q", facts.Unreadable, GraphQLUnresolvedFragment)
			}
			if _, classified := classifyGraphQL(facts); classified {
				t.Error("a document the resolver could not finish classified anyway")
			}
		})
	}
}

// TestParseGraphQLRequest_CyclicFragmentsTerminate covers the shape valid
// GraphQL forbids and a hostile document is not bound by. The resolver visits
// each definition once, so a cycle ends rather than being chased.
//
// It comes out labelled malformed rather than unresolved, and that is the more
// accurate of the two: a document whose fragments form a cycle is invalid by the
// spec and the server runs none of it, so "this document was not well-formed"
// says more than "a spread went unfollowed". What matters either way is that no
// verb is claimed for a request that never executed.
func TestParseGraphQLRequest_CyclicFragmentsTerminate(t *testing.T) {
	cycles := map[string]string{
		"mutual": `mutation M{...A}
			fragment A on Mutation{...B}
			fragment B on Mutation{...A}`,
		"self-referential": `mutation M{...A}
			fragment A on Mutation{...A}`,
	}
	for name, query := range cycles {
		t.Run(name, func(t *testing.T) {
			done := make(chan GraphQLFacts, 1)
			go func() {
				facts, _ := ParseGraphQLRequest(envelope(t, query, "", nil))
				done <- facts
			}()
			select {
			case facts := <-done:
				if facts.Unreadable != GraphQLMalformed {
					t.Errorf("unreadable = %q, want %q for a document the spec rejects",
						facts.Unreadable, GraphQLMalformed)
				}
				if _, classified := classifyGraphQL(facts); classified {
					t.Error("a cyclic document classified; the server executes none of it")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("fragment resolution did not terminate on a cycle")
			}
		})
	}
}

// TestParseGraphQLRequest_FragmentResolutionIsBounded is the reason following
// spreads does not hand a hostile document a lever on this process. Each
// definition is visited at most once, so a fan-out that would expand
// exponentially under naive substitution stays linear in the document.
func TestParseGraphQLRequest_FragmentResolutionIsBounded(t *testing.T) {
	// Each fragment spreads the next twice: naive expansion is 2^depth.
	const depth = 40
	var doc strings.Builder
	doc.WriteString("mutation M{...F0}")
	for i := range depth {
		fmt.Fprintf(&doc, " fragment F%d on Mutation{...F%d ...F%d}", i, i+1, i+1)
	}
	fmt.Fprintf(&doc, " fragment F%d on Mutation{mergePullRequest(input:{}){clientMutationId}}", depth)

	done := make(chan GraphQLFacts, 1)
	go func() {
		facts, _ := ParseGraphQLRequest(envelope(t, doc.String(), "", nil))
		done <- facts
	}()
	select {
	case facts := <-done:
		// Correctness rides along: visiting once still reaches the leaf, so the
		// act at the bottom of the chain is named.
		if got := facts.Mutation(); got != "mergePullRequest" {
			t.Errorf("mutation = %q, want mergePullRequest through the fan-out", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fragment resolution did not terminate on a fan-out document")
	}
}

// TestParseGraphQLRequest_SeveralMutationsInOneRequest: one request performing
// several acts cannot be described by one row, and one row is the rule. Every
// name is kept so the row can still say what was attempted.
func TestParseGraphQLRequest_SeveralMutationsInOneRequest(t *testing.T) {
	query := `mutation M($a:ClosePullRequestInput!,$b:MergePullRequestInput!){
		closePullRequest(input:$a){pullRequest{id}}
		mergePullRequest(input:$b){clientMutationId}}`
	facts, ok := ParseGraphQLRequest(envelope(t, query, "", nil))
	if !ok {
		t.Fatal("a multi-field mutation produced no facts")
	}
	if len(facts.Fields) != 2 || facts.Fields[0] != "closePullRequest" || facts.Fields[1] != "mergePullRequest" {
		t.Errorf("fields = %v, want both mutation names recorded verbatim", facts.Fields)
	}
	if facts.Mutation() != "" {
		t.Error("a two-act request resolved to a single act; no one row can honestly carry both")
	}
	if _, classified := classifyGraphQL(facts); classified {
		t.Error("a two-act request classified; it must take the fallback")
	}
}

// TestParseGraphQLRequest_Malformed: after the prescreen matched, anything the
// reader cannot walk is recorded as unreadable. The direction is the point — a
// document that defeats the parser must not thereby defeat the log.
func TestParseGraphQLRequest_Malformed(t *testing.T) {
	bodies := map[string][]byte{
		"not json":               []byte(`this is a mutation but not an envelope`),
		"query is not a string":  []byte(`{"query":{"mutation":true}}`),
		"unterminated selection": envelope(t, `mutation M{mergePullRequest(input:$i){clientMutationId}`, "", nil),
		"unterminated string":    envelope(t, `mutation M{addComment(input:{body:"unclosed}){clientMutationId}}`, "", nil),
		"unknown definition":     envelope(t, `schema{mutation:Mutation}`, "", nil),
		// A selection set that selects nothing. Invalid by the spec and run by
		// no server, but it reached for a mutation, and a row with neither a
		// field nor a reason would say nothing at all.
		"empty selection set": envelope(t, `mutation{}`, "", nil),
		// A batched request: GitHub's endpoint does not accept one, but the log
		// must still show that something reached for a mutation.
		"batched": []byte(`[{"query":"mutation M{mergePullRequest(input:$i){clientMutationId}}"}]`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			facts, ok := ParseGraphQLRequest(body)
			if !ok {
				t.Fatal("an unreadable body produced no facts; that is exactly the silence this must not have")
			}
			if facts.Unreadable != GraphQLMalformed {
				t.Errorf("unreadable = %q, want %q", facts.Unreadable, GraphQLMalformed)
			}
		})
	}
}

// TestParseGraphQLRequest_ReadsNothingButNamesAndIDs pins the privacy bound:
// the request carries the agent's composed prose, and none of it may end up in
// an audit row. Only the node id is lifted, and only from a known key.
func TestParseGraphQLRequest_ReadsNothingButNamesAndIDs(t *testing.T) {
	const secret = "the body text an agent composed, quoting a stranger"
	query := `mutation($input:AddCommentInput!){addComment(input:$input){commentEdge{node{url}}}}`
	facts, ok := ParseGraphQLRequest(envelope(t, query, "", input(map[string]any{
		"body":      secret,
		"subjectId": "PR_kwPrivacy",
	})))
	if !ok {
		t.Fatal("no facts for a comment mutation")
	}
	rendered, err := json.Marshal(facts)
	if err != nil {
		t.Fatalf("marshal facts: %v", err)
	}
	if strings.Contains(string(rendered), secret) {
		t.Errorf("facts carry the comment body: %s", rendered)
	}
	if facts.Subject != "PR_kwPrivacy" {
		t.Errorf("subject = %q, want the node id", facts.Subject)
	}
}

// TestParseGraphQLRequest_SubjectFromTopLevelVariables covers the hand-written
// request: `gh api graphql` callers pass variables flat rather than wrapped in
// the input object gh's own mutations use.
func TestParseGraphQLRequest_SubjectFromTopLevelVariables(t *testing.T) {
	query := `mutation($id:ID!){closePullRequest(input:{pullRequestId:$id}){pullRequest{id}}}`
	facts, ok := ParseGraphQLRequest(envelope(t, query, "", map[string]any{"pullRequestId": "PR_kwFlat"}))
	if !ok || facts.Subject != "PR_kwFlat" {
		t.Errorf("facts = %+v (ok=%v), want the node id from the top-level variables", facts, ok)
	}
}

// TestParseGraphQLRequest_DocumentOddities: comments, block strings, and
// directives all appear in legitimate documents, and each is a way a naive
// scan could lose track of where the top level is.
func TestParseGraphQLRequest_DocumentOddities(t *testing.T) {
	query := "# closePullRequest — a comment naming another mutation\n" +
		`mutation M($input:AddCommentInput!)@someDirective(if:true){` +
		"\n\t# mergePullRequest in a comment inside the selection\n" +
		`addComment(input:$input,note:"""a block string with } and { in it"""){clientMutationId}}`
	facts, ok := ParseGraphQLRequest(envelope(t, query, "", nil))
	if !ok {
		t.Fatal("no facts for a document with comments and directives")
	}
	if got := facts.Mutation(); got != "addComment" {
		t.Errorf("mutation = %q, want addComment — names in comments and strings are not fields", got)
	}
}

// TestResolveGraphQL_ResponseLocatesTheObject: a node id identifies an object
// without saying where it lives. When the response carried its url, the row
// gets real coordinates — which is also what lets it resolve to an entity the
// way the equivalent verb's row does.
func TestResolveGraphQL_ResponseLocatesTheObject(t *testing.T) {
	obs := Observation{
		Method: "POST",
		Path:   "/graphql",
		Status: 200,
		URL:    "https://github.com/acme/widgets/pull/841#issuecomment-99",
		GraphQL: &GraphQLFacts{
			Fields:  []string{"addComment"},
			Subject: "PR_kwLocate",
		},
	}
	shape, ok := Resolve(obs)
	if !ok {
		t.Fatal("a known mutation did not resolve")
	}
	if shape.Target() != "acme/widgets#841" {
		t.Errorf("target = %q, want acme/widgets#841 from the response url", shape.Target())
	}
	// The comment's own id is not in gh's selection set, and the number in that
	// url identifies the pull request the comment hangs off — not the comment.
	// Empty is the honest answer; the number would be a wrong one.
	if shape.ExternalID != "" {
		t.Errorf("external id = %q, want empty — that url's number is the PR's, not the comment's", shape.ExternalID)
	}

	// A created pull request IS the object its url names, so there the number is
	// the id, matching what the REST create records and what every other surface
	// addresses a PR by.
	created, ok := Resolve(Observation{
		Status:  200,
		URL:     "https://github.com/acme/widgets/pull/900",
		GraphQL: &GraphQLFacts{Fields: []string{"createPullRequest"}, Subject: "R_kwRepo"},
	})
	if !ok || created.ExternalID != "900" || created.Target() != "acme/widgets#900" {
		t.Errorf("created PR = %+v (ok=%v), want it keyed by number 900", created, ok)
	}

	// Without a url, the node id stands: unresolved, and honest about it.
	obs.URL = ""
	shape, ok = Resolve(obs)
	if !ok || shape.Target() != "PR_kwLocate" {
		t.Errorf("shape = %+v (ok=%v), want the node id as the target", shape, ok)
	}
}

// TestObservation_GraphQLErrorsAreNotSuccesses: the endpoint refuses a mutation
// with a 200 and an errors array, so a row that read only the status line would
// record a merge that never happened.
func TestObservation_GraphQLErrorsAreNotSuccesses(t *testing.T) {
	obs := Observation{
		Status:  200,
		GraphQL: &GraphQLFacts{Fields: []string{"mergePullRequest"}, Subject: "PR_x"},
		Errored: true,
	}
	if obs.Succeeded() {
		t.Error("a 200 carrying errors reported success; a refused merge is not a merge")
	}
	// The act it reached for is still knowable, which is what the attempt row says.
	shape, ok := obs.Classify()
	if !ok || shape.Action != domain.ActionPRMerged {
		t.Errorf("classify = %+v (ok=%v), want the attempted merge named", shape, ok)
	}
}

// TestReadGraphQLResponse covers both things the response contributes: whether
// the write failed, and where the created object went.
func TestReadGraphQLResponse(t *testing.T) {
	t.Run("errors", func(t *testing.T) {
		body := []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"Pull request is not mergeable"}]}`)
		errored, url := ReadGraphQLResponse(body, "mergePullRequest")
		if !errored {
			t.Error("an errors array did not register as a failure")
		}
		if url != "" {
			t.Errorf("url = %q, want none", url)
		}
	})

	t.Run("created object url", func(t *testing.T) {
		body := []byte(`{"data":{"addComment":{"commentEdge":{"node":{"url":"https://github.com/acme/widgets/pull/841#issuecomment-7"}}}}}`)
		errored, url := ReadGraphQLResponse(body, "addComment")
		if errored {
			t.Error("a clean response registered as a failure")
		}
		if url != "https://github.com/acme/widgets/pull/841#issuecomment-7" {
			t.Errorf("url = %q, want the created comment's link", url)
		}
	})

	t.Run("scans only the named mutation's payload", func(t *testing.T) {
		// A url sitting under some other member is not this act's object.
		body := []byte(`{"data":{"closePullRequest":{"pullRequest":{"id":"PR_x"}},"other":{"url":"https://github.com/a/b/pull/1"}}}`)
		_, url := ReadGraphQLResponse(body, "closePullRequest")
		if url != "" {
			t.Errorf("url = %q, want none — the scan must not wander outside the act's payload", url)
		}
	})

	t.Run("unparseable body", func(t *testing.T) {
		if errored, url := ReadGraphQLResponse([]byte("not json"), "addComment"); errored || url != "" {
			t.Errorf("errored=%v url=%q, want the zero reading for a body that did not parse", errored, url)
		}
	})
}

// TestGraphQLTable_ActionsAreKnownVocabulary keeps the table from inventing a
// verb. Every action it produces must be one the rest of the product already
// renders and filters on; a name only this map knows would show up in the log
// as an unlabelled row.
func TestGraphQLTable_ActionsAreKnownVocabulary(t *testing.T) {
	known := map[string]bool{
		domain.ActionCommentPosted:      true,
		domain.ActionCommentEdited:      true,
		domain.ActionCommentDeleted:     true,
		domain.ActionPRCreated:          true,
		domain.ActionPREdited:           true,
		domain.ActionPRClosed:           true,
		domain.ActionPRReopened:         true,
		domain.ActionPRMerged:           true,
		domain.ActionPRMarkedReady:      true,
		domain.ActionPRConvertedToDraft: true,
		domain.ActionReviewSubmitted:    true,
		domain.ActionReactionAdded:      true,
		domain.ActionReactionRemoved:    true,
	}
	for mutation, shape := range graphQLMutations {
		if !known[shape.action] {
			t.Errorf("%s maps to %q, which is not in the audit vocabulary", mutation, shape.action)
		}
	}
}

// FuzzParseGraphQLRequest exists because of where this code runs: the process
// holding the run's live GitHub credential, reading a document an agent
// assembled out of pull-request text that anyone on the internet may have
// written. The reader must terminate and must not panic on anything, however
// malformed — a crash here takes the credential sidecar with it.
//
// Correctness is not what this asserts; the table tests do that. This asserts
// only that no input escapes the contract's two outcomes.
func FuzzParseGraphQLRequest(f *testing.F) {
	f.Add([]byte(`{"query":"mutation($input:AddCommentInput!){addComment(input:$input){clientMutationId}}"}`))
	f.Add([]byte(`{"query":"mutation{...F}fragment F on Mutation{mergePullRequest(input:{}){id}}"}`))
	f.Add([]byte(`{"query":"query A{x} mutation B{y}","operationName":"B"}`))
	f.Add([]byte(`{"query":"mutation{a:b(c:\"}{\"){d}}","variables":{"input":{"pullRequestId":"PR_x"}}}`))
	f.Add([]byte(`{"query":"mutation{"`))
	f.Add([]byte(`mutation`))

	f.Fuzz(func(t *testing.T, body []byte) {
		facts, ok := ParseGraphQLRequest(body)
		if !ok {
			// "No write here" must be unambiguous: nothing may ride along with it.
			if facts.Mutation() != "" || facts.Unreadable != "" || facts.Subject != "" {
				t.Fatalf("ok=false carried facts %+v; a read must resolve to nothing at all", facts)
			}
			return
		}
		// A recorded request either names exactly one act or says why it cannot.
		if facts.Mutation() == "" && facts.Unreadable == "" && len(facts.Fields) < 2 {
			t.Fatalf("facts %+v name no act and give no reason", facts)
		}
		if len(facts.Fields) > maxTopLevelFields {
			t.Fatalf("collected %d field names, past the %d bound", len(facts.Fields), maxTopLevelFields)
		}
		if len(facts.Subject) > maxSubjectLength {
			t.Fatalf("subject is %d bytes, past the %d bound", len(facts.Subject), maxSubjectLength)
		}
		// Classification must agree with itself: a shape only ever comes from a
		// single named field.
		if shape, classified := classifyGraphQL(facts); classified && facts.Mutation() == "" {
			t.Fatalf("classified %+v from facts naming no single act", shape)
		}
	})
}
