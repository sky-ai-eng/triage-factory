package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// A provider is a verb namespace (github, jira, slack, future) that needs the
// same three primitives split across the run's trust boundary: a sealed
// credential keyed set the control plane resolves, a set of narrow org-bound
// policy/data relays, and the shared external-write audit funnel. This file
// holds the brain-side half of the first primitive — the registry the
// credential provisioner resolves each provider's keyed set through — kept in
// core so the brain-side provisioner never imports a provider package (core
// holds zero provider-specific credential symbols; only the JSON envelope
// crosses). A provider (e.g. ee/slack) registers its resolver here; the
// sidecar half that SELECTS a member of the set lives with the provider.

// ProvisionScope identifies the run a provider resolves credentials for. It
// carries the run's non-secret coordinates only — the brain binds identity
// from the claim, never from wire input.
type ProvisionScope struct {
	OrgID          string
	TeamID         string
	TaskID         string
	ConversationID string
}

// ProviderCredentialResolver resolves one provider's sealed credential set for
// a run, brain-side, against the secret-bearing stores only the control plane
// holds. It returns the provider's own opaque keyed set (JSON) to seal into
// the bundle, or (nil, nil) when the run needs none (the org/team has nothing
// configured — not an error). Only this seam is generic: the body may
// reference the provider's own types, since it is registered from the provider
// package.
type ProviderCredentialResolver func(ctx context.Context, stores db.Stores, scope ProvisionScope) (json.RawMessage, error)

// providerCredentialResolvers is the process-global registry of provider
// credential resolvers, keyed by namespace. Written once during
// single-threaded startup (an ee package's install()); read thereafter by the
// brain-side provisioner. Same no-mutex startup-write contract as
// RegisterExtension.
var providerCredentialResolvers = map[string]ProviderCredentialResolver{}

// RegisterProviderCredential registers namespace's brain-side credential
// resolver. Panics on empty/duplicate namespace or nil resolver (a wiring bug
// — fail at boot).
func RegisterProviderCredential(namespace string, r ProviderCredentialResolver) {
	if namespace == "" {
		panic("agenthost.RegisterProviderCredential: namespace must not be empty")
	}
	if r == nil {
		panic("agenthost.RegisterProviderCredential: resolver must not be nil")
	}
	if _, exists := providerCredentialResolvers[namespace]; exists {
		panic("agenthost.RegisterProviderCredential: " + namespace + " is already registered")
	}
	providerCredentialResolvers[namespace] = r
}

// ProviderOp is the orchestrator-side handler for one provider policy/data op
// (Primitive 2): it holds the run's org-scoped stores and binds identity from
// ConversationInfo — the wire args carry no org id, so a sidecar cannot steer the op at
// another org. args is the op's opaque request; the return is marshaled as the
// op result (nil for a notify-style op). Registered by a provider under its
// (namespace, op).
type ProviderOp func(ctx context.Context, stores db.Stores, info ConversationInfo, args json.RawMessage) (any, error)

// providerOps is the process-global registry of provider policy/data op
// handlers, keyed namespace → op. Same single-threaded-startup-write contract
// as the credential registry.
var providerOps = map[string]map[string]ProviderOp{}

// RegisterProviderOp registers namespace's op handler. Panics on empty
// namespace/op, nil handler, or a duplicate (namespace, op) — a wiring bug.
func RegisterProviderOp(namespace, op string, h ProviderOp) {
	if namespace == "" || op == "" {
		panic("agenthost.RegisterProviderOp: namespace and op must not be empty")
	}
	if h == nil {
		panic("agenthost.RegisterProviderOp: handler must not be nil")
	}
	if providerOps[namespace] == nil {
		providerOps[namespace] = map[string]ProviderOp{}
	}
	if _, exists := providerOps[namespace][op]; exists {
		panic("agenthost.RegisterProviderOp: " + namespace + "/" + op + " is already registered")
	}
	providerOps[namespace][op] = h
}

// ResetProviders clears the provider registries (tests only).
func ResetProviders() {
	providerCredentialResolvers = map[string]ProviderCredentialResolver{}
	providerOps = map[string]map[string]ProviderOp{}
}

// dispatchProviderOp runs a registered provider op against stores+info and
// returns its marshaled result — the shared path behind both the orchestrator's
// RelayServer (serving a relayed provider op) and the direct runtime (running
// one locally in all/local). An unknown namespace/op is an error, never a
// silent success.
func dispatchProviderOp(ctx context.Context, stores db.Stores, info ConversationInfo, namespace, op string, argsRaw json.RawMessage) (json.RawMessage, error) {
	ops, ok := providerOps[namespace]
	if !ok {
		return nil, fmt.Errorf("unknown relay namespace %q", namespace)
	}
	h, ok := ops[op]
	if !ok {
		return nil, fmt.Errorf("unknown %s relay op %q", namespace, op)
	}
	result, err := h(ctx, stores, info, argsRaw)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	if raw, ok := result.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(result)
}

// dispatchProviderOpLocal is the direct-runtime entry: marshal the typed args,
// run the op, unmarshal the result into out (nil out for a notify-style op).
func dispatchProviderOpLocal(ctx context.Context, stores db.Stores, info ConversationInfo, namespace, op string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal %s/%s args: %w", namespace, op, err)
	}
	res, err := dispatchProviderOp(ctx, stores, info, namespace, op, raw)
	if err != nil {
		return err
	}
	if out == nil || len(res) == 0 {
		return nil
	}
	return json.Unmarshal(res, out)
}

// ResolveProviderCredentials runs every registered provider's credential
// resolver against stores for scope and returns the non-empty sealed sets
// keyed by namespace — what the brain places in the bundle's Providers map. A
// provider that resolves to nil (nothing configured for this org/team) is
// omitted, so a GitHub-only org's bundle carries no provider sections. A
// resolver error fails the whole provision (a provider that is configured but
// unresolvable must not silently ship a run without its credential).
func ResolveProviderCredentials(ctx context.Context, stores db.Stores, scope ProvisionScope) (map[string]json.RawMessage, error) {
	if len(providerCredentialResolvers) == 0 {
		return nil, nil
	}
	// Deterministic order so a resolver failure names the same provider first
	// across runs, and the sealed bundle is byte-stable for a given input.
	names := make([]string, 0, len(providerCredentialResolvers))
	for name := range providerCredentialResolvers {
		names = append(names, name)
	}
	sort.Strings(names)

	var out map[string]json.RawMessage
	for _, name := range names {
		raw, err := providerCredentialResolvers[name](ctx, stores, scope)
		if err != nil {
			return nil, fmt.Errorf("resolve %s credentials: %w", name, err)
		}
		if len(raw) == 0 {
			continue
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[name] = raw
	}
	return out, nil
}
