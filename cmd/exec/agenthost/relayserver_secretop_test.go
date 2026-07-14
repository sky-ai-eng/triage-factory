package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// servedCoreRelayOps mirrors the op set RelayServer.dispatchCoreCall +
// dispatchCoreNotify actually serve (relayserver.go). Kept here as an explicit
// list so the security claim below reads as an enumeration a reviewer can check
// against the switch by eye: policy (authorize_repo), DB reads/writes (the exec
// verb trace), audit (record_*), and entitlement — and nothing that resolves or
// returns a credential.
var servedCoreRelayOps = []string{
	// git push policy
	agentproc.OpAuthorizeRepo,
	// DB reads
	opGetAgentRun,
	opGetTask,
	opListRepos,
	opGetRepo,
	opTeamTracksRepo,
	opGetRunWorktreeByRepoRef,
	opListRunWorktrees,
	opListRunArtifacts,
	opOrgJiraBase,
	opBuildAgentRunFooter,
	// DB writes
	opInsertRunWorktree,
	opDeleteRunWorktree,
	opUpsertArtifact,
	// entitlement gate
	opCheckEntitlement,
	// audit notifies
	agentproc.OpRecordDenial,
	agentproc.OpRecordPush,
	opRecordExternalWrite,
}

// credentialOpSubstrings are the tokens a credential-resolution op name would
// carry. The orchestrator's relay surface must expose none — provider creds are
// read sidecar-side from the sealed bundle, never asked of the orchestrator.
var credentialOpSubstrings = []string{"secret", "credential", "unseal", "private_key", "sealed_bundle"}

// TestRelayServer_ServesNoCredentialBearingOp is the executable form of the
// fourth cross-sidecar negative: the run-time surface the agenthost relocation
// added to the orchestrator — the RelayServer — carries no secret-resolution op.
// Two independent checks:
//
//  1. None of the ops it serves is named like a credential resolver.
//  2. A request for a credential-shaped op is rejected as unsupported (the
//     switch has no such case), so a compromised sidecar cannot ask the
//     orchestrator to hand back a secret.
func TestRelayServer_ServesNoCredentialBearingOp(t *testing.T) {
	// (1) No served op is a credential resolver by name.
	for _, op := range servedCoreRelayOps {
		low := strings.ToLower(op)
		for _, bad := range credentialOpSubstrings {
			if strings.Contains(low, bad) {
				t.Errorf("RelayServer serves op %q which reads like a credential resolver (matched %q) — the orchestrator must hold no per-run secret op", op, bad)
			}
		}
	}

	// (2) A credential-shaped request is unsupported. A zero-value stores and a
	// nil git gate are fine: the switch default returns before touching either.
	srv := NewRelayServer(db.Stores{}, RunInfo{OrgID: "org", RunID: "run"}, nil)
	ctx := context.Background()
	for _, op := range []string{"get_provider_credential", "resolve_secret", "unseal_bundle", "provider_credential", "get_llm_credential"} {
		_, err := srv.DispatchCall(ctx, agentproc.RelayNamespaceCore, op, nil)
		if err == nil {
			t.Errorf("RelayServer served credential-shaped op %q instead of rejecting it — the orchestrator exposes a secret surface", op)
			continue
		}
		if !strings.Contains(err.Error(), "unsupported core relay call op") {
			t.Errorf("credential-shaped op %q failed with %v; want the plain unsupported-op rejection (proving no such case exists)", op, err)
		}
	}
}

// recordingRelayConn is a relayConn that fails the test if the credential path
// ever reaches across the supervision channel. ProviderCredential must resolve
// entirely from the held bundle, so any call/notify here is a defect.
type recordingRelayConn struct{ t *testing.T }

func (c recordingRelayConn) call(_ context.Context, namespace, op string, _, _ any) error {
	c.t.Fatalf("ProviderCredential relayed %s/%s to the orchestrator — provider creds must resolve locally from the sealed bundle", namespace, op)
	return nil
}

func (c recordingRelayConn) notify(namespace, op string, _ any) {
	c.t.Fatalf("ProviderCredential notified %s/%s to the orchestrator — provider creds must resolve locally", namespace, op)
}

// TestRelayRuntime_ProviderCredentialNeverRelays pins the other half of the
// claim: on the sidecar, a provider handler's credential comes from the bundle
// the brain sealed to this run — the orchestrator is never asked. If the
// credential accessor reached across the relay conn, the recordingRelayConn
// above would fail the test.
func TestRelayRuntime_ProviderCredentialNeverRelays(t *testing.T) {
	sentinel := json.RawMessage(`{"bot_token":"xoxb-sealed-in-this-run"}`)
	rt := newRelayRuntime(recordingRelayConn{t: t}, RunInfo{OrgID: "org", RunID: "run"},
		func(namespace string) (json.RawMessage, bool) {
			if namespace != "slack" {
				return nil, false
			}
			return sentinel, true
		})

	got, err := rt.ProviderCredential(context.Background(), "slack")
	if err != nil {
		t.Fatalf("ProviderCredential: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("ProviderCredential returned %s; want the sealed-bundle value %s", got, sentinel)
	}

	// A namespace the bundle carries no credential for fails closed, still
	// without relaying.
	if _, err := rt.ProviderCredential(context.Background(), "github"); err == nil {
		t.Fatal("ProviderCredential for an absent namespace should fail closed, not succeed")
	}
}
