package agenthost

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// credentialOpSubstrings are the tokens a credential-resolution op name would
// carry, matched against the served ops' case-label identifiers (camelCase, so
// no underscores). The orchestrator's relay surface must expose none — provider
// creds are read sidecar-side from the sealed bundle, never asked of the
// orchestrator.
var credentialOpSubstrings = []string{"secret", "cred", "unseal", "token", "key"}

// extractServedCoreRelayOpLabels parses relayserver.go and returns a label for
// every case in dispatchCoreCall's and dispatchCoreNotify's `switch op` — the
// ops the RelayServer actually serves, read from the live source rather than a
// hand-maintained mirror. A named const case yields its identifier
// (agentproc.OpAuthorizeRepo → OpAuthorizeRepo, opGetTask → opGetTask); a raw
// string-literal case yields its unquoted value, so a non-idiomatic literal case
// can't dodge the name check either. It fails loudly if a dispatch func or the
// `switch op` it expects has moved, so a refactor surfaces here instead of
// silently emptying the check — which is the whole point: a future op added to
// the switch is then covered automatically, with nothing to keep in sync.
func extractServedCoreRelayOpLabels(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "relayserver.go", nil, 0)
	if err != nil {
		t.Fatalf("parse relayserver.go: %v", err)
	}
	wantFuncs := map[string]bool{"dispatchCoreCall": true, "dispatchCoreNotify": true}
	seenFunc := map[string]bool{}
	var labels []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || !wantFuncs[fn.Name.Name] {
			return true
		}
		seenFunc[fn.Name.Name] = true
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			sw, ok := m.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if tag, ok := sw.Tag.(*ast.Ident); !ok || tag.Name != "op" {
				return true // some other switch — keep looking
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List { // nil List == the default clause
					switch e := expr.(type) {
					case *ast.Ident:
						labels = append(labels, e.Name)
					case *ast.SelectorExpr:
						labels = append(labels, e.Sel.Name)
					case *ast.BasicLit:
						if e.Kind == token.STRING {
							if v, uerr := strconv.Unquote(e.Value); uerr == nil {
								labels = append(labels, v)
							}
						}
					}
				}
			}
			return false // found the `switch op`; don't descend into it
		})
		return true
	})
	for name := range wantFuncs {
		if !seenFunc[name] {
			t.Fatalf("relayserver.go: dispatch func %q not found — did it get renamed? update this drift guard", name)
		}
	}
	if len(labels) == 0 {
		t.Fatal("relayserver.go: extracted no `switch op` case labels — the AST drift guard is broken")
	}
	return labels
}

// TestRelayServer_ServesNoCredentialBearingOp is the executable form of the
// fourth cross-sidecar negative: the run-time surface the agenthost relocation
// added to the orchestrator — the RelayServer — carries no secret-resolution op.
// Two independent checks:
//
//  1. No op the switch actually serves is named like a credential resolver
//     (read live from relayserver.go, so a newly-added op can't slip past).
//  2. A request for a credential-shaped op is rejected as unsupported (the
//     switch has no such case), so a compromised sidecar cannot ask the
//     orchestrator to hand back a secret.
func TestRelayServer_ServesNoCredentialBearingOp(t *testing.T) {
	// (1) No served op is a credential resolver by name — checked against the
	// live switch's case labels, not a mirror that could drift out of coverage.
	served := extractServedCoreRelayOpLabels(t)
	for _, op := range served {
		low := strings.ToLower(op)
		for _, bad := range credentialOpSubstrings {
			if strings.Contains(low, bad) {
				t.Errorf("RelayServer serves op %q whose identifier reads like a credential resolver (matched %q) — the orchestrator must hold no per-run secret op", op, bad)
			}
		}
	}
	t.Logf("checked %d live-extracted RelayServer ops for credential-shaped names", len(served))

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
