package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// RelayServer must satisfy the dispatcher interface the supervisor routes to.
var _ agentproc.RelayDispatcher = (*RelayServer)(nil)

func TestRelayServer_AuthorizeRepoRoutesToGate(t *testing.T) {
	var sawOwner, sawRepo string
	git := &agentproc.GitProxyConfig{
		Authorize: func(_ context.Context, owner, repo string) (gitproxy.Decision, error) {
			sawOwner, sawRepo = owner, repo
			return gitproxy.Decision{Allowed: true, AllowedRefs: []string{"refs/heads/feature"}}, nil
		},
	}
	s := NewRelayServer(db.Stores{}, RunInfo{OrgID: "org1"}, git)

	args, _ := json.Marshal(agentproc.AuthorizeRepoArgs{Owner: "acme", Repo: "widgets"})
	raw, err := s.DispatchCall(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpAuthorizeRepo, args)
	if err != nil {
		t.Fatalf("DispatchCall: %v", err)
	}
	var reply agentproc.AuthorizeRepoReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if !reply.Allowed || len(reply.AllowedRefs) != 1 || reply.AllowedRefs[0] != "refs/heads/feature" {
		t.Fatalf("unexpected reply: %+v", reply)
	}
	if sawOwner != "acme" || sawRepo != "widgets" {
		t.Fatalf("gate saw wrong repo: %s/%s", sawOwner, sawRepo)
	}
}

// TestRelayServer_AuthorizeRepoPropagatesDenyFields pins that a denied gate
// decision's actionable DenyReason/DenyMessage survive the relay reply — the
// wire fields the sandboxed gitproxy needs to emit the specific 403 body +
// audit reason instead of the generic "repo not authorized" fallback.
func TestRelayServer_AuthorizeRepoPropagatesDenyFields(t *testing.T) {
	git := &agentproc.GitProxyConfig{
		Authorize: func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
			return gitproxy.Decision{
				Allowed:     false,
				DenyReason:  "repo-not-materialized",
				DenyMessage: "gitproxy: repo acme/widgets is tracked by this team but not yet materialized in this run; run 'workspace add acme/widgets' to persist it, then retry",
			}, nil
		},
	}
	s := NewRelayServer(db.Stores{}, RunInfo{OrgID: "org1"}, git)

	args, _ := json.Marshal(agentproc.AuthorizeRepoArgs{Owner: "acme", Repo: "widgets"})
	raw, err := s.DispatchCall(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpAuthorizeRepo, args)
	if err != nil {
		t.Fatalf("DispatchCall: %v", err)
	}
	var reply agentproc.AuthorizeRepoReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Allowed {
		t.Fatal("expected a deny")
	}
	if reply.DenyReason != "repo-not-materialized" {
		t.Errorf("DenyReason = %q, want repo-not-materialized", reply.DenyReason)
	}
	if !strings.Contains(reply.DenyMessage, "workspace add acme/widgets") {
		t.Errorf("DenyMessage = %q, want the workspace-add recovery hint", reply.DenyMessage)
	}
}

func TestRelayServer_AuthorizeRepoFailsClosedWithoutGate(t *testing.T) {
	// A run with no git gate wired (a Jira-only run) must deny, never allow-all.
	s := NewRelayServer(db.Stores{}, RunInfo{}, nil)
	args, _ := json.Marshal(agentproc.AuthorizeRepoArgs{Owner: "acme", Repo: "widgets"})
	raw, err := s.DispatchCall(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpAuthorizeRepo, args)
	if err != nil {
		t.Fatalf("DispatchCall: %v", err)
	}
	var reply agentproc.AuthorizeRepoReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Allowed {
		t.Fatal("expected a fail-closed deny with no gate wired")
	}
}

func TestRelayServer_RecordPushRoutesToRecorder(t *testing.T) {
	recorded := make(chan gitproxy.PushedRef, 1)
	git := &agentproc.GitProxyConfig{
		RecordPush: func(_ context.Context, push gitproxy.PushedRef) { recorded <- push },
	}
	s := NewRelayServer(db.Stores{}, RunInfo{}, git)

	args, _ := json.Marshal(agentproc.RecordPushArgs{Repo: "acme/widgets", Ref: "refs/heads/feature", NewSHA: "deadbeef", Created: true, Status: 200})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordPush, args)

	got := <-recorded
	want := gitproxy.PushedRef{Repo: "acme/widgets", Ref: "refs/heads/feature", NewSHA: "deadbeef", Created: true, Status: 200}
	if got != want {
		t.Fatalf("recorder saw %+v, want %+v", got, want)
	}
}

func TestRelayServer_RecordDenialRoutesToRecorder(t *testing.T) {
	recorded := make(chan gitproxy.DeniedGitOp, 1)
	git := &agentproc.GitProxyConfig{
		RecordDenial: func(_ context.Context, denied gitproxy.DeniedGitOp) { recorded <- denied },
	}
	s := NewRelayServer(db.Stores{}, RunInfo{}, git)

	args, _ := json.Marshal(agentproc.RecordDenialArgs{Owner: "acme", Repo: "widgets", Ref: "refs/heads/main", Op: "push", Reason: "off-ref"})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordDenial, args)

	got := <-recorded
	if got.Owner != "acme" || got.Repo != "widgets" || got.Reason != "off-ref" {
		t.Fatalf("recorder saw wrong denial: %+v", got)
	}
}

// TestRelayServer_EgressDenialWritesAuditRow pins the executor-side half of the
// egress audit: the sidecar relays only (target, reason), and the orchestrator
// binds the conversation from its OWN RunInfo — so a sidecar can neither
// attribute a probe to another run nor forge the dedup key that collapses the
// repeats.
func TestRelayServer_EgressDenialWritesAuditRow(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordEgressDenialArgs{
		Target: "api.github.com:443",
		Reason: `host "api.github.com" is not on the sandbox egress allowlist`,
	})
	// Relayed twice, as a probing agent would: one row.
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordEgressDenial, args)
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordEgressDenial, args)

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 egress row after two relays, got %d: %+v", len(acts), acts)
	}
	a := acts[0]
	if a.Action != domain.ActionEgressDenied || a.Target != "api.github.com:443" ||
		a.Provider != domain.ArtifactProviderNetwork {
		t.Errorf("relayed egress row mismatch: %+v", a)
	}
	if a.ConversationID != info.RunID {
		t.Errorf("conversation = %q, want the server's own run %q (never the wire)", a.ConversationID, info.RunID)
	}
}

// TestRelayServer_GHWriteWritesAuditRow pins the same for the gh channel: a
// refused write relays into the opaque attempt row — never the verb it was
// reaching for — attributed to this run.
func TestRelayServer_GHWriteWritesAuditRow(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method: "PUT", Path: "/repos/octo/repo/pulls/7/merge", Status: 404,
	})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, args)

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 gh write row, got %d: %+v", len(acts), acts)
	}
	a := acts[0]
	if a.Action != domain.ActionGHChannelWrite || a.Target != "octo/repo" ||
		a.ConversationID != info.RunID {
		t.Errorf("relayed gh write row mismatch: %+v", a)
	}
	if !strings.Contains(a.DetailJSON, `"http_status":404`) ||
		!strings.Contains(a.DetailJSON, `"attempted":"pr_merged"`) {
		t.Errorf("detail_json = %q, want the refused status and the attempted act", a.DetailJSON)
	}
}

// TestRelayServer_PRCreateLandsOneArtifactAndOneAction pins the two halves of a
// raw PR create staying in their own lanes. The injector reports it twice — once
// as an artifact-bearing mutation, once as a write — and the two must produce
// exactly one artifact and exactly one action: an artifact says the pull request
// exists, an action says this run opened it, and neither answers for the other.
//
// The action's id is the PR NUMBER, matching what the verb path records and what
// every other surface addresses a PR by — not the database id the response's own
// `id` field carries.
func TestRelayServer_PRCreateLandsOneArtifactAndOneAction(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)
	ctx := context.Background()

	const prURL = "https://github.com/acme/widgets/pull/42"
	obsArgs, _ := json.Marshal(agentproc.RecordObservationArgs{
		Kind: domain.ArtifactKindPullRequest, Owner: "acme", Repo: "widgets", Number: 42,
		NodeID: "PR_kwx", URL: prURL, Title: "Fix it", Draft: true,
	})
	s.DispatchNotify(ctx, agentproc.RelayNamespaceCore, agentproc.OpRecordObservation, obsArgs)

	writeArgs, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method: "POST", Path: "/repos/acme/widgets/pulls", Status: 201,
		ExternalID: "2314567890", URL: prURL,
	})
	s.DispatchNotify(ctx, agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, writeArgs)

	arts := listRunArtifacts(t, stores, info.RunID)
	if len(arts) != 1 || arts[0].Kind != domain.ArtifactKindPullRequest {
		t.Fatalf("want 1 pull_request artifact, got %d: %+v", len(arts), arts)
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want exactly 1 action for one create, got %d: %+v", len(acts), acts)
	}
	a := acts[0]
	if a.Action != domain.ActionPRCreated || a.Target != "acme/widgets#42" ||
		a.ExternalID != "42" || a.URL != prURL {
		t.Errorf("pr create row = %+v, want pr_created on acme/widgets#42 keyed on the number", a)
	}
	if !strings.Contains(a.DetailJSON, `"path":"/repos/acme/widgets/pulls"`) ||
		!strings.Contains(a.DetailJSON, `"method":"POST"`) {
		t.Errorf("detail_json = %q, want the raw method + path that distinguish this from a verb-path create", a.DetailJSON)
	}
}

// TestRelayServer_GHWriteClassifiesTheCreate pins the relayed half of the
// incident fix: the sidecar carries the wire facts it alone can see — the
// created reply's id and link, read off the response — and the semantic row is
// built HERE, on the side that owns the vocabulary. One request, one row.
func TestRelayServer_GHWriteClassifiesTheCreate(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method:     "POST",
		Path:       "/repos/acme/widgets/pulls/841/comments/555/replies",
		Status:     201,
		ExternalID: "777",
		URL:        "https://github.com/acme/widgets/pull/841#discussion_r777",
	})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, args)

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want exactly 1 row for one request, got %d: %+v", len(acts), acts)
	}
	a := acts[0]
	if a.Action != domain.ActionCommentPosted || a.Target != "acme/widgets#841" ||
		a.ExternalID != "777" || a.URL != "https://github.com/acme/widgets/pull/841#discussion_r777" ||
		a.ConversationID != info.RunID {
		t.Errorf("relayed reply row mismatch: %+v", a)
	}
	if !strings.Contains(a.DetailJSON, `"in_reply_to":555`) {
		t.Errorf("detail_json = %q, want the thread the reply landed on", a.DetailJSON)
	}
}

// TestRelayServer_GraphQLWriteWritesAuditRow pins the GraphQL hop end to end.
// The act is named only in the request, which only the sidecar ever sees, so
// what crosses this socket is the whole of what the row can know — a facts
// member dropped in transit is an unnamed write, silently.
func TestRelayServer_GraphQLWriteWritesAuditRow(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method: "POST",
		Path:   "/graphql",
		Status: 200,
		URL:    "https://github.com/octo/repo/pull/7#issuecomment-9",
		GraphQL: &agentproc.GraphQLWriteFacts{
			Operation: "CommentCreate",
			Mutations: []string{"addComment"},
			Subject:   "PR_kwRelay",
		},
	})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, args)

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 graphql write row, got %d: %+v", len(acts), acts)
	}
	a := acts[0]
	if a.Action != domain.ActionCommentPosted || a.Target != "octo/repo#7" || a.ConversationID != info.RunID {
		t.Errorf("relayed graphql write row mismatch: %+v", a)
	}
}

// TestRelayServer_GraphQLRefusalRelaysAsAnAttempt: the endpoint's 200-with-
// errors refusal is a fact only the sidecar's response read can establish, so
// it has to survive the hop as its own field — the status code alone says the
// opposite.
func TestRelayServer_GraphQLRefusalRelaysAsAnAttempt(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method: "POST", Path: "/graphql", Status: 200, Errored: true,
		GraphQL: &agentproc.GraphQLWriteFacts{
			Mutations: []string{"mergePullRequest"},
			Subject:   "PR_kwRelayRefused",
		},
	})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, args)

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(acts), acts)
	}
	if acts[0].Action != domain.ActionGraphQLWrite ||
		!strings.Contains(acts[0].DetailJSON, `"attempted":"pr_merged"`) {
		t.Errorf("relayed refusal = %+v, want an attempt row naming the merge", acts[0])
	}
}
