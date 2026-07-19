package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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
