package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// TestServer_PanickingVerb_FailsRPCAndKeepsServing is the containment
// invariant, exercised through the surface it protects: a verb handler dies
// mid-dispatch, and the only casualty is that RPC.
//
// If the accept goroutine had no recover, this test would not fail — it would
// take the whole test binary down with it, which is precisely what the panic
// would do to the sidecar in production. The extension registry is the seam
// used to plant the panic because it is a real dispatch path (CallExtension
// fans out to registered handler code exactly like the other forty verbs), not
// a test-only hook grafted into the switch.
func TestServer_PanickingVerb_FailsRPCAndKeepsServing(t *testing.T) {
	t.Cleanup(ResetExtensions)
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterProvider(entitlements.Static(fakeExtensionFeature))

	RegisterExtension("fake", fakeExtensionFeature, func(context.Context, ExtensionRuntime, string, json.RawMessage) (json.RawMessage, error) {
		panic("verb exploded mid-dispatch")
	})

	info := RunInfo{OrgID: "org-1", RunID: "run-panic"}
	client := startExtensionTestDaemon(t, info)

	// The client's contract for a mid-request death: EOF, surfaced as a failed
	// call. Same outcome it already handles when the daemon is torn down under
	// an in-flight request, so the agent needs no new behavior.
	_, err := client.CallExtension(context.Background(), "fake", "post", json.RawMessage(`{"a":1}`))
	if err == nil {
		t.Fatal("expected the panicking verb to fail the RPC")
	}
	if !strings.Contains(err.Error(), "closed connection") {
		t.Errorf("error = %q, want the dropped-connection surface (EOF)", err)
	}

	// The daemon is still serving: a fresh connection gets a real answer. This
	// is the half that matters — the panic cost one call, not the process and
	// not the proxies sharing it.
	got, err := client.LookupRun(context.Background())
	if err != nil {
		t.Fatalf("next RPC after the panic: %v", err)
	}
	if got.RunID != info.RunID {
		t.Errorf("LookupRun after the panic returned %+v, want run %q", got, info.RunID)
	}
}
