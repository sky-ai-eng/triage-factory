package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// fakeExtensionFeature stands in for an ee/ feature's entitlement key
// throughout this file — mirroring routing's fakeEventType convention (see
// internal/routing/source_registry_test.go).
const fakeExtensionFeature = entitlements.Feature("test-fake-extension")

func noopExtensionHandler(context.Context, ExtensionRuntime, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func TestRegisterExtension_PanicsOnEmptyNamespace(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for an empty namespace")
		}
	}()
	RegisterExtension("", fakeExtensionFeature, noopExtensionHandler)
}

// TestRegisterExtension_PanicsOnReservedNamespace pins the shadow guard: the
// built-in exec subcommands ("gh", "jira", "workspace") never go through the
// extension seam, so registering one here must fail at boot — mirroring
// routing.RegisterSource's reserved-prefix guard.
func TestRegisterExtension_PanicsOnReservedNamespace(t *testing.T) {
	for _, ns := range []string{"gh", "jira", "workspace"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic for reserved namespace %q", ns)
				}
			}()
			RegisterExtension(ns, fakeExtensionFeature, noopExtensionHandler)
		}()
	}
}

func TestRegisterExtension_PanicsOnNilHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for a nil handler")
		}
	}()
	RegisterExtension("fake-nil-handler", fakeExtensionFeature, nil)
}

// TestRegisterExtension_PanicsOnDuplicate pins that a second registration for
// the same namespace fails at boot instead of silently swapping handlers.
func TestRegisterExtension_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(ResetExtensions)
	RegisterExtension("fake-dup", fakeExtensionFeature, noopExtensionHandler)
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	RegisterExtension("fake-dup", fakeExtensionFeature, noopExtensionHandler)
}

func TestLocalClient_CallExtension_UnknownNamespace(t *testing.T) {
	t.Cleanup(ResetExtensions)
	stores, _ := newTestDB(t)
	c := NewLocal(stores, ConversationInfo{OrgID: "org-1", ConversationID: "conv-1"})

	if _, err := c.CallExtension(context.Background(), "nope", "do", nil); err == nil {
		t.Fatal("expected an error for an unregistered namespace")
	}
}

// TestLocalClient_CallExtension_NoProvider_NotEnabled pins the whole security
// story in one test: with no Provider registered (the default Static()
// everything-off answer — exactly what a CLI process sees, per main.go's
// dispatchCLI/ee.Install ordering), CallExtension refuses before the handler
// ever runs.
func TestLocalClient_CallExtension_NoProvider_NotEnabled(t *testing.T) {
	t.Cleanup(ResetExtensions)
	t.Cleanup(entitlements.Reset)

	RegisterExtension("fake", fakeExtensionFeature, func(context.Context, ExtensionRuntime, string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("handler must not run when the org isn't entitled")
		return nil, nil
	})

	stores, _ := newTestDB(t)
	c := NewLocal(stores, ConversationInfo{OrgID: "org-1", ConversationID: "conv-1"})

	_, err := c.CallExtension(context.Background(), "fake", "do", nil)
	if err == nil {
		t.Fatal("expected a 'not enabled' error with the default Static provider")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %q, want it to mention 'not enabled'", err)
	}
}

// TestLocalClient_CallExtension_Entitled_InvokesHandlerWithConversationInfo pins the
// success path: a Provider that grants the feature lets the call through,
// the run's identity threads to the handler unmodified, and args/result
// round-trip.
func TestLocalClient_CallExtension_Entitled_InvokesHandlerWithConversationInfo(t *testing.T) {
	t.Cleanup(ResetExtensions)
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterProvider(entitlements.Static(fakeExtensionFeature))

	info := ConversationInfo{OrgID: "org-1", UserID: "user-1", ConversationID: "conv-1", TeamID: "team-1"}
	var gotInfo ConversationInfo
	var gotMethod string
	var gotArgs json.RawMessage
	RegisterExtension("fake", fakeExtensionFeature, func(_ context.Context, rt ExtensionRuntime, method string, args json.RawMessage) (json.RawMessage, error) {
		gotInfo = rt.Info()
		gotMethod = method
		gotArgs = args
		return json.RawMessage(`{"ok":true}`), nil
	})

	stores, _ := newTestDB(t)
	c := NewLocal(stores, info)

	result, err := c.CallExtension(context.Background(), "fake", "post", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallExtension: %v", err)
	}
	if !reflect.DeepEqual(gotInfo, info) {
		t.Errorf("handler saw ConversationInfo %+v, want %+v", gotInfo, info)
	}
	if gotMethod != "post" {
		t.Errorf("handler saw method %q, want %q", gotMethod, "post")
	}
	if string(gotArgs) != `{"x":1}` {
		t.Errorf("handler saw args %s, want %s", gotArgs, `{"x":1}`)
	}
	if string(result) != `{"ok":true}` {
		t.Errorf("CallExtension result = %s, want %s", result, `{"ok":true}`)
	}
}

// startExtensionTestDaemon spins up a real Server + unix listener for info,
// mirroring the harness in agenthost_test.go (TestServer_LookupConversation_RoundTrip
// et al.). Returns a connected IPCClient; cleanup is registered via t.Cleanup.
func startExtensionTestDaemon(t *testing.T, info ConversationInfo) *IPCClient {
	t.Helper()
	stores, _ := newTestDB(t)
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	client := Dial(sockPath)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestIPCClient_CallExtension_EntitledRoundTrip pins the sandbox transport
// end to end: a real daemon dispatches CallExtension to the registered
// handler and the result crosses the wire intact.
func TestIPCClient_CallExtension_EntitledRoundTrip(t *testing.T) {
	t.Cleanup(ResetExtensions)
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterProvider(entitlements.Static(fakeExtensionFeature))

	RegisterExtension("fake", fakeExtensionFeature, func(_ context.Context, _ ExtensionRuntime, method string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(fmt.Sprintf(`{"echo":%q}`, method)), nil
	})

	client := startExtensionTestDaemon(t, ConversationInfo{OrgID: "org-1", ConversationID: "conv-1"})

	result, err := client.CallExtension(context.Background(), "fake", "post", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("CallExtension: %v", err)
	}
	if string(result) != `{"echo":"post"}` {
		t.Errorf("result = %s, want %s", result, `{"echo":"post"}`)
	}
}

// TestIPCClient_CallExtension_DaemonRefusal_SurfacesErrorString pins that a
// daemon-side entitlement refusal reaches the sandbox client as a readable
// error, not a dropped connection.
func TestIPCClient_CallExtension_DaemonRefusal_SurfacesErrorString(t *testing.T) {
	t.Cleanup(ResetExtensions)
	t.Cleanup(entitlements.Reset) // default Static(): nothing entitled

	RegisterExtension("fake", fakeExtensionFeature, func(context.Context, ExtensionRuntime, string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("handler must not run when the org isn't entitled")
		return nil, nil
	})

	client := startExtensionTestDaemon(t, ConversationInfo{OrgID: "org-1", ConversationID: "conv-1"})

	_, err := client.CallExtension(context.Background(), "fake", "post", nil)
	if err == nil {
		t.Fatal("expected the daemon's refusal to surface as an error")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %q, want it to mention 'not enabled'", err)
	}
}

// TestIPCClient_CallExtension_UnknownMethodOnOldDaemon simulates a daemon
// built before this RPC existed: it answers any request with the same
// "unknown method" error dispatch's default case produces. Pins that the
// client surfaces a clean error rather than hanging or panicking — the
// point of NOT bumping ProtocolVersion for this backward-compatible
// addition (an older daemon just answers CallExtension like any other
// method it doesn't recognize).
func TestIPCClient_CallExtension_UnknownMethodOnOldDaemon(t *testing.T) {
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req request
		if err := readFrame(conn, &req); err != nil {
			return
		}
		_ = writeFrame(conn, response{Error: fmt.Sprintf("%s: %s", ErrUnknownMethod, req.Method)})
	}()

	client := Dial(sockPath)
	defer client.Close()

	_, err = client.CallExtension(context.Background(), "fake", "post", nil)
	if err == nil {
		t.Fatal("expected a clean error from an old daemon, got nil")
	}
	if !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error = %q, want it to mention 'unknown method'", err)
	}
}
