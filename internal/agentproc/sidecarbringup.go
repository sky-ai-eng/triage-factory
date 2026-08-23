package agentproc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// sidecarHelloTimeout bounds how long the orchestrator waits for a freshly
// launched sidecar to announce its public key. The broker has already
// confirmed the process is running by the time LaunchSidecar returns; the
// hello is the first thing the sidecar writes, so this only guards a sidecar
// that started but wedged before minting its key.
const sidecarHelloTimeout = 30 * time.Second

// SidecarProvisionFunc parks the run in awaiting-credentials with the
// sidecar's published public key and returns the opaque sealed bundle the
// brain wrote for it, plus the boot epoch it was sealed under. The
// orchestrator relays those bytes to the sidecar without ever opening them —
// only the sidecar holds the matching private key. Provided by the delegate
// (which owns the conversation-queue + claim_credentials stores); the
// returned bytes are ciphertext the orchestrator cannot read.
type SidecarProvisionFunc func(ctx context.Context, sidecarPubKeyB64 string) (sealed []byte, bootEpoch int64, err error)

// sidecarSupervisor is the orchestrator's end of the supervision channel: it
// captures the sidecar's hello (its public key) and routes the sidecar's
// generic relay envelope to the run's RelayDispatcher — the org-bound op
// server the delegate supplied, which holds the DB handle and secrets the
// capless sidecar cannot. The credential-bearing direction (relaying the
// sealed bundle down, asking for proxies) is driven by BringUpRunSidecar, not
// here.
type sidecarSupervisor struct {
	relay RelayDispatcher

	helloOnce sync.Once
	helloCh   chan string
}

func newSidecarSupervisor(relay RelayDispatcher) *sidecarSupervisor {
	return &sidecarSupervisor{relay: relay, helloCh: make(chan string, 1)}
}

// Handle implements sidecarproto.Handler for inbound sidecar → orchestrator
// traffic: the hello key publish, and the two relay-envelope forms routed to
// the RelayDispatcher.
func (s *sidecarSupervisor) Handle(ctx context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	switch kind {
	case sidecarproto.KindHello:
		var h sidecarproto.HelloBody
		if err := json.Unmarshal(body, &h); err != nil {
			return nil, err
		}
		s.helloOnce.Do(func() { s.helloCh <- h.PubKey })
		return nil, nil

	case sidecarproto.KindRelayCall:
		var env sidecarproto.RelayCallBody
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		// A run with no dispatcher wired (a test fixture) must fail closed —
		// a relayed decision the orchestrator can't serve is an error, never
		// a silent allow-all.
		if s.relay == nil {
			return nil, fmt.Errorf("agentproc: relay call %s/%s with no dispatcher wired", env.Namespace, env.Op)
		}
		// A json.RawMessage result is written to the response frame verbatim
		// (sidecarproto.marshalBody special-cases it), so the sidecar unmarshals
		// the op's own result shape.
		return s.relay.DispatchCall(ctx, env.Namespace, env.Op, env.Args)

	case sidecarproto.KindRelayNotify:
		var env sidecarproto.RelayCallBody
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if s.relay != nil {
			s.relay.DispatchNotify(ctx, env.Namespace, env.Op, env.Args)
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("agentproc: unexpected sidecar request kind %q", kind)
	}
}

// SidecarBringUpParams shapes what a run's sidecar proxies must cover. The
// delegate fills it before the pre-sandbox clone. HostVethIP and Git are
// separate from the wire body because Git also wires the supervisor's
// authorize/denial relay (the sidecar's git proxy calls back for the
// DB-backed push decision, which only the orchestrator can make).
type SidecarBringUpParams struct {
	// HostVethIP is the host-side veth address the sidecar binds every proxy
	// on — reachable both from the sandbox (the jailed agent) and from the
	// host (the orchestrator's own clone + agenthost).
	HostVethIP string

	// Git, when non-nil, requests the git-over-HTTPS proxy — its presence is
	// the whole of what BringUpRunSidecar reads out of it. Its
	// Authorize/RecordDenial/RecordPush are NOT consumed here — those are the
	// git proxy's push authz/audit, served through the Relay dispatcher as
	// core ops (the delegate builds the same GitProxyConfig into that
	// dispatcher). TokenSource and Upstream are likewise ignored — the sidecar
	// resolves both the real token and the host it belongs to from its own
	// unsealed bundle. nil = no git proxy.
	Git *GitProxyConfig

	// Relay is the org-bound op server the supervisor routes the sidecar's
	// relay envelope to (the git proxy's authorize/audit, and — once the
	// agenthost is relocated — the exec verb trace and provider policy ops).
	// The delegate builds it from the run's stores + ConversationInfo + git gate. nil
	// only in a test fixture, where a relayed op fails closed.
	Relay RelayDispatcher

	// IdentityPairs are the org commit-identity git config pairs folded into
	// the sandbox's GIT_CONFIG block (user.name/user.email).
	IdentityPairs [][2]string

	// GitHubAPIEnabled requests the GitHub-REST credential proxy (GetPR +
	// agenthost gh verbs). The REST base it prepends is derived sidecar-side
	// from the sealed bundle; the orchestrator points its client at the bare
	// proxy URL the result returns.
	GitHubAPIEnabled bool

	// JiraAPIEnabled requests the Jira-REST credential proxy (agenthost jira
	// verbs). JiraAPIUpstream is the org's Jira base (Cloud gateway or DC
	// host); the sidecar injects the real Cloud-Basic / DC-Bearer auth.
	JiraAPIEnabled  bool
	JiraAPIUpstream string

	// GHChannelEnabled requests the real-gh credential-injector proxy (the TLS
	// listener the sandboxed gh reaches via GH_HOST). It shares the REST base
	// the GitHub proxy derives from the bundle, and derives the GraphQL
	// endpoint from it. The injector's host:port + placeholder come back in
	// the result (GHChannelHost/GHChannelToken).
	GHChannelEnabled bool

	// AgentHost, when non-nil, asks the sidecar to also host the exec-verb
	// socket server for this run (the relocation) — carrying the run's
	// non-secret identity. nil leaves the socket server in the orchestrator
	// (all/local, and the pre-relocation executor path).
	AgentHost *sidecarproto.AgentHostInfo
}

// BringUpRunSidecar drives the full credential handshake with a launched
// sidecar and returns the coordinates its proxies produced — the non-secret
// sandbox env AND the per-run proxy URLs + placeholder tokens the orchestrator
// routes its own pre-sandbox clone / GetPR / agenthost through — plus the live
// supervision Conn (kept open for the run's lifetime so the git proxy's
// authorize callbacks and any mid-run bundle refresh keep working). The caller
// closes the returned Conn when the run ends (and closes the sidecar handle
// itself — that is what SIGKILLs the process and frees its credential address
// space); on any error here the Conn is already closed.
//
// Sequence: wait for the sidecar's hello (its per-run public key) → provision
// (publish the key so the brain seals this run's bundle to it, and hand back
// the opaque ciphertext) → relay the ciphertext down → ask the sidecar to bind
// its proxies on HostVethIP. The orchestrator never opens the bundle; only the
// sidecar holds the private key.
func BringUpRunSidecar(ctx context.Context, sc sandbox.LaunchedSidecar, provision SidecarProvisionFunc, params SidecarBringUpParams) (*sidecarproto.StartProxiesResult, *sidecarproto.Conn, error) {
	stream := sc.Supervision()
	if stream == nil {
		return nil, nil, fmt.Errorf("agentproc: launched sidecar exposes no supervision channel")
	}
	sup := newSidecarSupervisor(params.Relay)
	conn := sidecarproto.New(stream, sup)

	// The sidecar's hello is its first write; wait for it before provisioning
	// (the brain needs the public key to seal against). Its own span because
	// this is the sidecar process starting up — the one part of bring-up that
	// nothing on this side can attribute otherwise, since the sidecar exports
	// nothing itself.
	helloCtx, helloSpan := tracer.Start(ctx, "sidecar.hello")
	var pubKey string
	var helloErr error
	select {
	case pubKey = <-sup.helloCh:
	case <-conn.Done():
		helloErr = fmt.Errorf("agentproc: sidecar closed before announcing its key: %w", conn.Err())
	case <-time.After(sidecarHelloTimeout):
		helloErr = fmt.Errorf("agentproc: timed out waiting for sidecar hello")
	case <-helloCtx.Done():
		helloErr = helloCtx.Err()
	}
	recordSpanError(helloSpan, helloErr)
	helloSpan.End()
	if helloErr != nil {
		_ = conn.Close()
		return nil, nil, helloErr
	}

	// The credential handshake with the control plane: publish the key, then
	// wait for a bundle sealed to it. Usually the longest leg of bring-up, and
	// the one that measures somebody else's latency rather than this pod's.
	provCtx, provSpan := tracer.Start(ctx, "sidecar.provision")
	sealed, bootEpoch, err := provision(provCtx, pubKey)
	recordSpanError(provSpan, err)
	provSpan.End()
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("agentproc: provision sidecar credentials: %w", err)
	}

	if err := conn.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealed, BootEpoch: bootEpoch}, nil); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("agentproc: relay sealed bundle to sidecar: %w", err)
	}

	req := sidecarproto.StartProxiesBody{
		HostVethIP:          params.HostVethIP,
		GitEnabled:          params.Git != nil,
		IdentityConfigPairs: params.IdentityPairs,
		GitHubAPIEnabled:    params.GitHubAPIEnabled,
		JiraAPIEnabled:      params.JiraAPIEnabled,
		JiraAPIUpstream:     params.JiraAPIUpstream,
		GHChannelEnabled:    params.GHChannelEnabled,
		AgentHost:           params.AgentHost,
	}
	var res sidecarproto.StartProxiesResult
	proxyCtx, proxySpan := tracer.Start(ctx, "sidecar.start_proxies")
	err = conn.Call(proxyCtx, sidecarproto.KindStartProxies, req, &res)
	recordSpanError(proxySpan, err)
	proxySpan.End()
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("agentproc: start sidecar proxies: %w", err)
	}

	return &res, conn, nil
}
