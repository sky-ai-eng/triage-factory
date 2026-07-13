package agentproc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// sidecarHelloTimeout bounds how long the orchestrator waits for a freshly
// launched sidecar to announce its public key. The broker has already
// confirmed the process is running by the time LaunchSidecar returns; the
// hello is the first thing the sidecar writes, so this only guards a sidecar
// that started but wedged before minting its key.
const sidecarHelloTimeout = 30 * time.Second

// recordPushRelayTimeout bounds the orchestrator-side artifact write kicked
// off by a relayed KindRecordPush, mirroring the in-process backstop's own cap
// (gitproxy.recordPushTimeout) so a wedged store can't hold the supervision
// channel's per-frame goroutine open indefinitely.
const recordPushRelayTimeout = 30 * time.Second

// SidecarProvisionFunc parks the run in awaiting-credentials with the
// sidecar's published public key and returns the opaque sealed bundle the
// brain wrote for it, plus the boot epoch it was sealed under. The
// orchestrator relays those bytes to the sidecar without ever opening them —
// only the sidecar holds the matching private key. Provided by the delegate
// (which owns the run-queue + run_credentials stores); the returned bytes are
// ciphertext the orchestrator cannot read.
type SidecarProvisionFunc func(ctx context.Context, sidecarPubKeyB64 string) (sealed []byte, bootEpoch int64, err error)

// sidecarSupervisor is the orchestrator's end of the supervision channel: it
// captures the sidecar's hello (its public key) and serves the git proxy's
// authorize / denial callbacks against the DB-backed decision the delegate
// supplied on the GitProxyConfig. The credential-bearing direction (relaying
// the sealed bundle down, asking for proxies) is driven by bringUpSidecar,
// not here.
type sidecarSupervisor struct {
	git *GitProxyConfig

	helloOnce sync.Once
	helloCh   chan string
}

func newSidecarSupervisor(git *GitProxyConfig) *sidecarSupervisor {
	return &sidecarSupervisor{git: git, helloCh: make(chan string, 1)}
}

// Handle implements sidecarproto.Handler for inbound sidecar → orchestrator
// traffic.
func (s *sidecarSupervisor) Handle(ctx context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	switch kind {
	case sidecarproto.KindHello:
		var h sidecarproto.HelloBody
		if err := json.Unmarshal(body, &h); err != nil {
			return nil, err
		}
		s.helloOnce.Do(func() { s.helloCh <- h.PubKey })
		return nil, nil

	case sidecarproto.KindAuthorizeRepo:
		var req sidecarproto.AuthorizeRepoBody
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		// A run with no authorize gate wired (a Jira-only run reached this
		// path, or a test fixture) must fail closed — a git push the
		// orchestrator can't adjudicate is denied, never allowed.
		if s.git == nil || s.git.Authorize == nil {
			return sidecarproto.AuthorizeRepoResult{Allowed: false}, nil
		}
		dec, err := s.git.Authorize(ctx, req.Owner, req.Repo)
		if err != nil {
			return nil, err
		}
		return sidecarproto.AuthorizeRepoResult{Allowed: dec.Allowed, AllowedRefs: dec.AllowedRefs}, nil

	case sidecarproto.KindRecordDenial:
		var d sidecarproto.RecordDenialBody
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		if s.git != nil && s.git.RecordDenial != nil {
			s.git.RecordDenial(ctx, gitproxy.DeniedGitOp{
				Owner:  d.Owner,
				Repo:   d.Repo,
				Ref:    d.Ref,
				Op:     d.Op,
				Reason: d.Reason,
			})
		}
		return nil, nil

	case sidecarproto.KindRecordPush:
		var p sidecarproto.RecordPushBody
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		if s.git != nil && s.git.RecordPush != nil {
			// The relayed push already completed upstream; the DB write here
			// must not run unbounded on the shared control channel, so cap it
			// at the same window the in-process backstop uses.
			recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
			defer cancel()
			s.git.RecordPush(recCtx, gitproxy.PushedRef{
				Repo:    p.Repo,
				Ref:     p.Ref,
				NewSHA:  p.NewSHA,
				Created: p.Created,
				Status:  p.Status,
			})
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

	// Git, when non-nil, requests the git-over-HTTPS proxy and carries the
	// orchestrator-side authorize gate. TokenSource is IGNORED here — the
	// sidecar resolves the real token from its own unsealed bundle; only
	// Authorize / RecordDenial / RecordPush / Upstream are consumed (the first
	// three relay back over the supervision channel). nil = no git proxy.
	Git *GitProxyConfig

	// IdentityPairs are the org commit-identity git config pairs folded into
	// the sandbox's GIT_CONFIG block (user.name/user.email).
	IdentityPairs [][2]string

	// GitHubAPIEnabled requests the GitHub-REST credential proxy (GetPR +
	// agenthost gh verbs). GitHubAPIUpstream is the resolved REST base —
	// api.github.com, a GHES {base}/api/v3, or a .ghe.com api host — which
	// the proxy prepends; the orchestrator points its client at the bare
	// proxy URL the result returns.
	GitHubAPIEnabled  bool
	GitHubAPIUpstream string

	// JiraAPIEnabled requests the Jira-REST credential proxy (agenthost jira
	// verbs). JiraAPIUpstream is the org's Jira base (Cloud gateway or DC
	// host); the sidecar injects the real Cloud-Basic / DC-Bearer auth.
	JiraAPIEnabled  bool
	JiraAPIUpstream string
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
	sup := newSidecarSupervisor(params.Git)
	conn := sidecarproto.New(stream, sup)

	// The sidecar's hello is its first write; wait for it before provisioning
	// (the brain needs the public key to seal against).
	var pubKey string
	select {
	case pubKey = <-sup.helloCh:
	case <-conn.Done():
		_ = conn.Close()
		return nil, nil, fmt.Errorf("agentproc: sidecar closed before announcing its key: %w", conn.Err())
	case <-time.After(sidecarHelloTimeout):
		_ = conn.Close()
		return nil, nil, fmt.Errorf("agentproc: timed out waiting for sidecar hello")
	case <-ctx.Done():
		_ = conn.Close()
		return nil, nil, ctx.Err()
	}

	sealed, bootEpoch, err := provision(ctx, pubKey)
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
		GitHubAPIUpstream:   params.GitHubAPIUpstream,
		JiraAPIEnabled:      params.JiraAPIEnabled,
		JiraAPIUpstream:     params.JiraAPIUpstream,
	}
	if params.Git != nil {
		req.GitUpstream = params.Git.Upstream
	}
	var res sidecarproto.StartProxiesResult
	if err := conn.Call(ctx, sidecarproto.KindStartProxies, req, &res); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("agentproc: start sidecar proxies: %w", err)
	}

	return &res, conn, nil
}
