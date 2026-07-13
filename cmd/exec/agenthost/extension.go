package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ExtensionHandler executes one extension method. rt is the provider runtime
// the handler closes over instead of db.Stores/secrets: rt.Info() carries the
// run's identity, rt.Relay reaches the provider's org-bound policy ops,
// rt.ProviderCredential yields its sealed credential set to select from, and
// rt.Record is the audit funnel. So the SAME handler runs in the orchestrator
// (direct runtime) and in the capless sidecar (relay runtime) unchanged.
// args/result are opaque JSON — the wire shape is owned by the registering
// feature on both ends. Results must fit the IPC frame cap (see maxFrameSize
// in protocol.go).
type ExtensionHandler func(ctx context.Context, rt ExtensionRuntime, method string, args json.RawMessage) (json.RawMessage, error)

// extensionRegistration pairs a namespace's handler with the feature gating
// it, so CallExtension can check entitlement before invoking the handler.
type extensionRegistration struct {
	feature entitlements.Feature
	handler ExtensionHandler
}

// extensionRegistry is the process-global map of registered extension
// namespaces. Written once during single-threaded startup (an ee package's
// init()), read thereafter from exec subcommand processes and the daemon's
// dispatch goroutines. Same no-mutex startup-write contract as
// routing.RegisterSource (internal/routing/source_registry.go:42) — see
// there for the rationale.
var extensionRegistry = map[string]extensionRegistration{}

// reservedExtensionNamespaces are the namespaces RegisterExtension refuses —
// the built-in exec subcommands, whose logic lives in core and never goes
// through the extension seam. Mirrors routing.reservedSourcePrefixes.
var reservedExtensionNamespaces = map[string]bool{
	"gh": true, "jira": true, "workspace": true,
}

// RegisterExtension registers a handler for a verb namespace (the exec
// subcommand name, e.g. "slack"), gated by feature. Called from an ee
// package's init() or extension install() — a handler that needs db.Stores
// (which init() doesn't have) registers from its install() closure instead;
// the registry is a process-global map and install() runs during server boot
// before the agenthost daemon serves any run, so the timing is equally safe
// either way (see e.g. ee/slack/exec_host.go's registerSlackExec). Panics on
// empty/duplicate/reserved namespace or nil handler (wiring bug — fail at
// boot).
func RegisterExtension(namespace string, feature entitlements.Feature, h ExtensionHandler) {
	if namespace == "" {
		panic("agenthost.RegisterExtension: namespace must not be empty")
	}
	if reservedExtensionNamespaces[namespace] {
		panic("agenthost.RegisterExtension: " + namespace + " is a built-in namespace and cannot be re-registered")
	}
	if h == nil {
		panic("agenthost.RegisterExtension: handler must not be nil")
	}
	if _, exists := extensionRegistry[namespace]; exists {
		panic("agenthost.RegisterExtension: " + namespace + " is already registered")
	}
	extensionRegistry[namespace] = extensionRegistration{feature: feature, handler: h}
}

// ResetExtensions clears the registry (tests only).
func ResetExtensions() {
	extensionRegistry = map[string]extensionRegistration{}
}

// RegisteredExtensionFeatures returns the distinct features referenced by
// registered extension namespaces, sorted. Exists for the composition-root
// parity test (feature_parity_test.go at the repo root) — see
// entitlements.RegisteredEventGateFeatures for the rationale.
func RegisteredExtensionFeatures() []entitlements.Feature {
	seen := map[entitlements.Feature]struct{}{}
	for _, reg := range extensionRegistry {
		seen[reg.feature] = struct{}{}
	}
	out := make([]entitlements.Feature, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// callExtension is the shared implementation behind LocalClient.CallExtension
// — the single dispatch point both transports route through, so a verb
// author cannot forget the entitlement gate: unknown namespace → error;
// feature not held by the run's org → error; otherwise the handler runs with
// the run's identity. In a CLI process the entitlements Provider is always
// the Static (everything off) default (see main.go's dispatchCLI/ee.Install
// ordering), so this always refuses there; in the daemon process the real
// Provider decides.
func callExtension(ctx context.Context, rt Runtime, namespace, method string, args json.RawMessage) (json.RawMessage, error) {
	reg, ok := extensionRegistry[namespace]
	if !ok {
		return nil, fmt.Errorf("unknown extension namespace %q", namespace)
	}
	// Gate on the run's entitlement BEFORE the handler runs. On the sidecar this
	// relays to the orchestrator (the sidecar process has no entitlements
	// provider); on all/local it checks in-process. rt satisfies the narrower
	// ExtensionRuntime the handler receives.
	allowed, err := rt.CheckEntitlement(ctx, string(reg.feature))
	if err != nil {
		return nil, fmt.Errorf("%s: check entitlement: %w", namespace, err)
	}
	if !allowed {
		return nil, fmt.Errorf("%s: not enabled for this organization", namespace)
	}
	return reg.handler(ctx, rt, method, args)
}
