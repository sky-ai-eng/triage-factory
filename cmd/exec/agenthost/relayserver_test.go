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
