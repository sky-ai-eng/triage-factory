package agenthost

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

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

	info := ConversationInfo{OrgID: "org-1", ConversationID: "run-panic"}
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
	got, err := client.LookupConversation(context.Background())
	if err != nil {
		t.Fatalf("next RPC after the panic: %v", err)
	}
	if got.ConversationID != info.ConversationID {
		t.Errorf("LookupConversation after the panic returned %+v, want run %q", got, info.ConversationID)
	}
}

// TestHandleConn_NilMethodSeenServesNormally pins the one hazard the panic
// guard's out-parameter introduces: a caller that passes nil because it doesn't
// want the method name must get a served request, not a nil dereference. A
// crash there would be contained by serveConn — and would still have turned a
// working RPC into a failed one, using the mechanism added to prevent exactly
// that.
func TestHandleConn_NilMethodSeenServesNormally(t *testing.T) {
	info := ConversationInfo{OrgID: "org-1", ConversationID: "run-nil-out"}
	stores, _ := newTestDB(t)
	srv := NewServer(stores, info, nil)

	srvSide, cliSide := net.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer func() { _ = srvSide.Close() }()
		srv.handleConn(srvSide, nil)
	}()

	if err := cliSide.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if err := writeFrame(cliSide, request{Version: ProtocolVersion, Method: methodLookupConversation, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var resp response
	if err := readFrame(cliSide, &resp); err != nil {
		t.Fatalf("readFrame: %v (a nil methodSeen dropped the connection instead of serving)", err)
	}
	if resp.Error != "" {
		t.Fatalf("daemon answered with an error: %s", resp.Error)
	}
	var res lookupConversationResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Info.ConversationID != info.ConversationID {
		t.Errorf("LookupConversation returned %+v, want run %q", res.Info, info.ConversationID)
	}
	_ = cliSide.Close()
	<-served
}
