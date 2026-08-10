package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
)

// TestCapture_GHWriteDenied_RecordsEveryRefusal pins the refusal row: one per
// refused request (no dedup — a model that retries a merge three ways leaves
// three rows, and that pattern is itself the signal), the act named where
// anything could name it, and the reason always.
//
// The reason is the load-bearing field. "We refused a merge" is the system
// working as designed and needs no attention; "we refused something we could
// not read" is either a real client this broke or someone probing the boundary,
// and a log that renders them alike cannot tell an operator which happened.
func TestCapture_GHWriteDenied_RecordsEveryRefusal(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	client := NewLocal(stores, info)
	ctx := context.Background()

	merge := ghwrite.Request{Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge"}
	mergeRef, _ := ghwrite.Gate(merge)
	client.RecordGHWriteDenied(ctx, merge, mergeRef)
	client.RecordGHWriteDenied(ctx, merge, mergeRef)

	unreadable := ghwrite.Request{
		Method:  "POST",
		Path:    "/api/graphql",
		GraphQL: &ghwrite.GraphQLFacts{Unreadable: ghwrite.GraphQLOverCap},
	}
	unreadableRef, _ := ghwrite.Gate(unreadable)
	client.RecordGHWriteDenied(ctx, unreadable, unreadableRef)

	acts := listExternalActions(t, stores)
	if len(acts) != 3 {
		t.Fatalf("want 3 rows (every refusal is its own event), got %d: %+v", len(acts), acts)
	}

	var sawUnreadable bool
	for _, a := range acts {
		if a.Action != domain.ActionGHWriteDenied {
			t.Errorf("action = %q, want gh_write_denied", a.Action)
		}
		if a.Provider != domain.ArtifactProviderGitHub || a.Credential != domain.CredentialGitHubApp {
			t.Errorf("refusal row mismatch: %+v", a)
		}
		if a.ConversationID != info.RunID {
			t.Errorf("conversation = %q, want the run's own %q", a.ConversationID, info.RunID)
		}
		if a.DedupKey == "" {
			t.Error("refusals must not collapse — each attempt is its own event")
		}

		var detail map[string]any
		if err := json.Unmarshal([]byte(a.DetailJSON), &detail); err != nil {
			t.Fatalf("detail_json did not parse (%v): %s", err, a.DetailJSON)
		}
		if detail["reason"] == nil {
			t.Errorf("detail = %s, want the refusal family", a.DetailJSON)
		}
		if detail["unreadable"] != nil {
			sawUnreadable = true
			if detail["unreadable"] != ghwrite.GraphQLOverCap {
				t.Errorf("unreadable = %v, want %q", detail["unreadable"], ghwrite.GraphQLOverCap)
			}
			if detail["refused"] != nil {
				t.Errorf("detail = %s, want no act named — naming it is what failed", a.DetailJSON)
			}
			// A GraphQL request names no repository, so the endpoint path is
			// all there is to file it under. Anything repo-shaped here would be
			// invented.
			if a.Target != "/api/graphql" {
				t.Errorf("target = %q, want the endpoint path", a.Target)
			}
			continue
		}
		if detail["refused"] != domain.ActionPRMerged {
			t.Errorf("detail = %s, want the refused act named", a.DetailJSON)
		}
		// Repo-level, never owner/repo#N: the number came from the agent and
		// was verified by nothing, so an entity-shaped target would mint a stub
		// for an object that may not exist.
		if a.Target != "acme/widgets" {
			t.Errorf("target = %q, want the repository only", a.Target)
		}
		if !strings.Contains(a.DetailJSON, "/pulls/7/merge") {
			t.Errorf("detail = %s, want the full path", a.DetailJSON)
		}
	}
	if !sawUnreadable {
		t.Error("the unreadable refusal left no row")
	}
}

// TestGHWriteDeniedAction_GraphQLKeepsWhatWasRead: a refused GraphQL request
// still discloses an operation name, the fields that were read, and the node id
// its variables named. All three go in the row, because a refusal with no
// coordinates is a row nobody can follow up on — and the node id is the only
// thing a GraphQL request ever says about its target.
func TestGHWriteDeniedAction_GraphQLKeepsWhatWasRead(t *testing.T) {
	req := ghwrite.Request{
		Method: "POST",
		Path:   "/api/graphql",
		GraphQL: &ghwrite.GraphQLFacts{
			Operation: "MergePullRequest",
			Fields:    []string{"mergePullRequest"},
			Subject:   "PR_kwABC",
		},
	}
	ref, gated := ghwrite.Gate(req)
	if !gated {
		t.Fatal("a merge mutation was not gated")
	}

	act := GHWriteDeniedAction(req, ref)
	if act.Target != "PR_kwABC" {
		t.Errorf("target = %q, want the node id the variables disclosed", act.Target)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(act.DetailJSON), &detail); err != nil {
		t.Fatalf("detail_json did not parse: %v", err)
	}
	if detail["mutation"] != "mergePullRequest" || detail["operation"] != "MergePullRequest" {
		t.Errorf("detail = %s, want the mutation and operation names", act.DetailJSON)
	}
	if detail["refused"] != domain.ActionPRMerged {
		t.Errorf("detail = %s, want the act named in the audit vocabulary", act.DetailJSON)
	}
}

// TestRelay_AuthorizeGHWrite is the multi-mode decision path end to end: the
// sidecar asks, the orchestrator re-derives the answer from the shared table,
// writes the refusal row, and replies. The re-derivation is the point — the
// sidecar's recognition decides which requests get ASKED about, and this side
// decides what the answer is.
func TestRelay_AuthorizeGHWrite(t *testing.T) {
	tests := []struct {
		name        string
		args        agentproc.AuthorizeGHWriteArgs
		wantAllowed bool
	}{
		{
			name: "a REST merge",
			args: agentproc.AuthorizeGHWriteArgs{Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge"},
		},
		{
			name: "a GraphQL merge",
			args: agentproc.AuthorizeGHWriteArgs{
				Method:  "POST",
				Path:    "/api/graphql",
				GraphQL: &agentproc.GraphQLWriteFacts{Mutations: []string{"mergePullRequest"}},
			},
		},
		{
			name: "a request whose act could not be read",
			args: agentproc.AuthorizeGHWriteArgs{
				Method:  "POST",
				Path:    "/api/graphql",
				GraphQL: &agentproc.GraphQLWriteFacts{Unreadable: ghwrite.GraphQLAmbiguousOperation},
			},
		},
		{
			// The arm that exists because this side is authoritative: a request
			// the sidecar asked about but this reading finds ungated goes
			// through, and leaves no refusal row.
			name: "a write this side finds ungated",
			args: agentproc.AuthorizeGHWriteArgs{
				Method:  "POST",
				Path:    "/api/graphql",
				GraphQL: &agentproc.GraphQLWriteFacts{Mutations: []string{"addComment"}},
			},
			wantAllowed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stores, info := newCaptureStores(t, true)
			s := NewRelayServer(stores, info, nil)

			args, _ := json.Marshal(tc.args)
			raw, err := s.DispatchCall(context.Background(), agentproc.RelayNamespaceCore,
				agentproc.OpAuthorizeGHWrite, args)
			if err != nil {
				t.Fatalf("DispatchCall: %v", err)
			}
			var reply agentproc.AuthorizeGHWriteReply
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatalf("decode reply: %v", err)
			}
			if reply.Allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v", reply.Allowed, tc.wantAllowed)
			}

			acts := listExternalActions(t, stores)
			if tc.wantAllowed {
				if len(acts) != 0 {
					t.Fatalf("an allowed request left %d refusal rows: %+v", len(acts), acts)
				}
				return
			}
			if len(acts) != 1 || acts[0].Action != domain.ActionGHWriteDenied {
				t.Fatalf("rows = %+v, want exactly one gh_write_denied", acts)
			}
		})
	}
}
