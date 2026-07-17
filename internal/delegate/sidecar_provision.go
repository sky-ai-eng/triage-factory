package delegate

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// executorSandbox is the run network + credential sidecar + proxy coordinates
// the dispatcher stands up on TF_ROLE=executor BEFORE the pre-sandbox clone, so
// the clone (and GetPR, and the agenthost) route through the sidecar's proxies
// while the orchestrator holds no credential. It owns its own teardown, which
// the dispatcher defers until after the agent run returns.
type executorSandbox struct {
	net     *sandbox.RunNetwork
	sidecar sandbox.LaunchedSidecar
	conn    *sidecarproto.Conn
	res     *sidecarproto.StartProxiesResult

	// stopRelay stops the credential-refresh relay goroutine; closed once by
	// Close. relayOnce guards the close against a double Close.
	stopRelay chan struct{}
	relayOnce sync.Once
}

// Close tears the executor sandbox down in the order that keeps the veth alive
// under anything still using it: stop the refresh relay, stop the supervision
// reader, SIGKILL the sidecar (freeing its proxies + unsealed bundle), then tear
// down the network the proxies were bound on and release its subnet index. Safe
// on nil.
func (e *executorSandbox) Close() {
	if e == nil {
		return
	}
	if e.stopRelay != nil {
		e.relayOnce.Do(func() { close(e.stopRelay) })
	}
	if e.conn != nil {
		_ = e.conn.Close()
	}
	if e.sidecar != nil {
		_ = e.sidecar.Close()
	}
	if e.net != nil {
		_ = e.net.Close()
	}
}

// proxyEnv is the non-secret sandbox env the sidecar's proxies produced (LLM /
// git / egress URLs + placeholders + the single GIT_CONFIG_* block), threaded
// into agentproc.RunOptions.PrebuiltProxyEnv. nil-safe.
func (e *executorSandbox) proxyEnv() []string {
	if e == nil || e.res == nil {
		return nil
	}
	return e.res.Env
}

// runNetwork is the prebuilt network handed to agentproc.RunOptions.PrebuiltNetwork.
func (e *executorSandbox) runNetwork() *sandbox.RunNetwork {
	if e == nil {
		return nil
	}
	return e.net
}

// Network / ProxyEnv / GitCloneAuth are the exported accessors the curator seam
// (internal/curator's TurnSandbox) consumes — a homed curator turn runs under
// this same executor sandbox but curator can't import the unexported delegate
// type, so it depends on an interface these satisfy. Kept thin wrappers over
// the run-path accessors so both paths read the same coordinates.

// Network is the prebuilt run network for a curator turn (PrebuiltNetwork).
func (e *executorSandbox) Network() *sandbox.RunNetwork { return e.runNetwork() }

// ProxyEnv is the non-secret sandbox proxy env for a curator turn
// (PrebuiltProxyEnv).
func (e *executorSandbox) ProxyEnv() []string { return e.proxyEnv() }

// GitCloneAuth builds the CloneAuth that routes a host-side fetch of cloneURL
// through this turn's sidecar git proxy — the curator materialize's equivalent
// of the delegated clone's CloneAuthViaGitProxy (internal/delegate/delegate.go),
// so the orchestrator holds no token and the real credential is injected
// host-side on the sidecar's upstream hop. Empty (a no-op CloneAuth) when the
// sidecar exposes no git proxy.
func (e *executorSandbox) GitCloneAuth(cloneURL string) worktree.CloneAuth {
	if e == nil || e.res == nil {
		return worktree.CloneAuth{}
	}
	return worktree.CloneAuthViaGitProxy(e.res.GitProxyURL, cloneHostBase(cloneURL), e.res.GitProxyToken)
}

// bringUpExecutorSandbox stands up the run network, launches the credential
// sidecar, provisions it (publishing the sidecar's public key so the brain
// seals THIS run's bundle to it, without the orchestrator ever unsealing), and
// binds the sidecar's proxies on the veth. Gated on the MODE, not the role:
// multi mode is always per-run-isolated, so every multi dispatch takes the
// sidecar path (in practice only executors dispatch — control runs no
// dispatcher and the all role is local-only — but hanging the gate on the
// mode means a future multi caller can never silently fall back to the
// in-process credential path; on a pod with no cap-broker it fails loudly at
// construction instead). Returns nil in local mode and on an unwired test
// fixture — the caller then keeps the in-process proxy path (agentproc
// allocates the network and runs the proxies itself).
//
// On any error the partial state (network, sidecar) is already torn down; the
// caller does not Close a nil return.
func (s *Spawner) bringUpExecutorSandbox(ctx context.Context, orgID string, run *domain.AgentRun, task domain.Task) (*executorSandbox, error) {
	if runmode.Current() != runmode.ModeMulti {
		return nil, nil
	}
	// Not wired (a test fixture) — degrade like every other nil-store seam in
	// this package: behave as if the executor path doesn't exist.
	if s.runCredentials == nil || s.runQueue == nil {
		return nil, nil
	}

	net, err := sandbox.SetupRunNetwork(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("set up run network: %w", err)
	}
	sc, err := sandbox.LaunchSidecar(ctx, sandbox.SidecarConfig{RunID: run.ID, SubnetIdx: net.Idx})
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("launch credential sidecar: %w", err)
	}

	stores, storesSet := s.getStores()
	info := agenthost.RunInfo{
		OrgID:            orgID,
		UserID:           run.CreatorUserID,
		RunID:            run.ID,
		TeamID:           run.TeamID,
		IsEventTriggered: run.TriggerType == domain.TriggerTypeEvent,
	}

	// Git gate for the sidecar's git proxy: the DB-backed authorize/denial the
	// proxy relays back per push, plus the non-secret org base as the insteadOf
	// upstream. No TokenSource — the sidecar resolves the real token from its
	// own unsealed bundle. Omitted (no git proxy) when stores are unwired.
	var git *agentproc.GitProxyConfig
	if storesSet {
		git = s.executorGitGate(ctx, info, stores)
	}

	// The commit identity's git config pairs (author/committer) fold into the
	// sandbox's single GIT_CONFIG_* block alongside the proxy routing — resolved
	// here because on the executor path agentproc runs no ConfigureProxies to
	// fold them itself.
	identity := s.resolveCommitIdentity(ctx, orgID, run.TriggerType, run.CreatorUserID)

	params := agentproc.SidecarBringUpParams{
		HostVethIP: net.HostIP,
		Git:        git,
		// The org-bound op server the sidecar's relay envelope dispatches to:
		// the git proxy's push authz/audit (backed by the same git gate) plus
		// the exec verb-trace DB ops, all identity-bound from RunInfo. Holds
		// the stores + secrets the capless sidecar cannot.
		Relay:         agenthost.NewRelayServer(stores, info, git),
		IdentityPairs: githooks.IdentityConfigPairs(identity.Name, identity.Email),
		// Every delegated run may touch a GitHub repo (clone/push + GetPR +
		// agenthost gh verbs), so the git + GitHub-REST proxies are always on.
		GitHubAPIEnabled:  true,
		GitHubAPIUpstream: s.githubAPIUpstreamFor(ctx, orgID),
		// Jira REST only for Jira runs (their bundle carries a Jira credential;
		// a non-Jira org's bundle carries none and the proxy would fail to bind).
		// Upstream left empty — the sidecar derives it from the bundle's Jira URL.
		JiraAPIEnabled: task.EntitySource == "jira",
		// Relocate the exec-verb socket server into the sidecar: it hosts the
		// hostile-input parser in this capless jail, relaying every DB effect
		// back to the orchestrator. Carries the run's non-secret identity only.
		AgentHost: &sidecarproto.AgentHostInfo{
			OrgID:          info.OrgID,
			UserID:         info.UserID,
			TeamID:         info.TeamID,
			RunID:          info.RunID,
			EventTriggered: info.IsEventTriggered,
		},
	}

	res, conn, err := agentproc.BringUpRunSidecar(ctx, sc, s.sidecarProvisionFor(orgID, run.ID), params)
	if err != nil {
		_ = sc.Close()
		_ = net.Close()
		return nil, fmt.Errorf("bring up credential sidecar: %w", err)
	}

	es := &executorSandbox{net: net, sidecar: sc, conn: conn, res: res, stopRelay: make(chan struct{})}
	// The sidecar received this run's bundle once, at bring-up, but the brain's
	// refresh sweep re-mints short-lived credentials mid-run (role-mode STS at
	// its half-life; hour-lived GitHub installation tokens) and re-seals them
	// into run_credentials against the SAME per-run key. The sidecar is capless
	// with no DB access, so it can't pull them — this goroutine relays each new
	// sealed blob down (the sidecar idempotently re-accepts it) so a long run
	// keeps signing with live credentials instead of 502-ing on expiry.
	_, myBootEpoch := s.executorIdentity()
	go s.relayCredentialRefreshes(run.ID, func(ctx context.Context) (int64, []byte, bool, error) {
		_, be, sealed, ok, err := s.runCredentials.Get(ctx, orgID, run.ID)
		return be, sealed, ok, err
	}, myBootEpoch, conn, es.stopRelay)
	return es, nil
}

// credRefreshRelayInterval is how often the orchestrator re-reads
// run_credentials and relays a changed sealed bundle to the sidecar. Well
// inside every credential's refresh half-life (the brain re-mints long before
// expiry), so the sidecar never serves a stale credential in practice.
const credRefreshRelayInterval = 30 * time.Second

// credBundleGetter reads the current sealed bundle for a run or a curator turn:
// its boot_epoch, the ciphertext, whether a row exists, and any read error. The
// run variant wraps runCredentials.Get; the curator variant wraps
// curatorTurnCredentials.Get. relayCredentialRefreshes never unseals the bytes.
type credBundleGetter func(ctx context.Context) (bootEpoch int64, sealed []byte, ok bool, err error)

// relayCredentialRefreshes polls the sealed-bundle channel for one run/turn and
// relays any NEW sealed bundle down to the sidecar over the supervision channel
// — the push half of mid-run credential refresh (the sidecar can't reach the
// DB). It relays only when the sealed bytes actually change (the brain re-mint),
// never unsealing them. subject is the run/turn id for logging; getter reads the
// channel (run vs curator). Runs until stop is closed (executorSandbox.Close) or
// the supervision channel dies. Never fires on all/local (no executor sandbox).
func (s *Spawner) relayCredentialRefreshes(subject string, getter credBundleGetter, bootEpoch int64, conn *sidecarproto.Conn, stop <-chan struct{}) {
	if getter == nil {
		return
	}
	ticker := time.NewTicker(credRefreshRelayInterval)
	defer ticker.Stop()
	// The bytes last handed to the sidecar, so a tick that reads the same blob
	// does not re-relay it. Starts nil: the first tick re-relays whatever
	// bring-up already sent (the sidecar re-accepts it idempotently), and only a
	// genuine brain re-mint changes the blob thereafter.
	var lastSealed []byte
	for {
		select {
		case <-stop:
			return
		case <-conn.Done():
			return
		case <-ticker.C:
			readCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			be, sealed, ok, err := getter(readCtx)
			cancel()
			if err != nil {
				dispatchLog.Warn("read sealed credential bundle for refresh relay failed; retrying", "subject", subject, "error", err)
				continue
			}
			if !ok || be != bootEpoch || len(sealed) == 0 || bytes.Equal(sealed, lastSealed) {
				continue
			}
			relayCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = conn.Call(relayCtx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealed, BootEpoch: be}, nil)
			cancel()
			if err != nil {
				dispatchLog.Warn("relay refreshed credential bundle to sidecar failed; will retry next tick", "subject", subject, "error", err)
				continue
			}
			lastSealed = sealed
		}
	}
}

// githubAPIUpstreamFor resolves the REST API base the sidecar's GitHub proxy
// forwards to — api.github.com, a GHES {base}/api/v3, or a .ghe.com api host —
// derived from the org's non-secret configured GitHub base via the same
// APIBase mapping the client uses. Empty (resolver unwired / read error)
// defaults to api.github.com inside the sidecar.
func (s *Spawner) githubAPIUpstreamFor(ctx context.Context, orgID string) string {
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return ""
	}
	base, err := resolver.BaseURLFor(ctx, orgID)
	if err != nil {
		delegateLog.Warn("resolve github base for sidecar api proxy failed; defaulting to api.github.com", "org", orgID, "error", err)
		return ""
	}
	return ghclient.APIBase(base)
}

// executorGitGate builds the orchestrator-side git gate the sidecar's git proxy
// relays to: the non-secret org base as the insteadOf upstream, plus the
// DB-backed Authorize (team-tracks + run_worktrees ledger + ref allowlist), the
// RecordDenial audit, and the RecordPush capture. No TokenSource /
// ProbeCredentials — the sidecar resolves and probes the real token from its
// own unsealed bundle. Returns nil when the resolver is unwired.
//
// RecordPush is load-bearing on the executor: the pre-push hook stands down in
// the sandbox (the sidecar's StartRunProxies sets PushCaptureProxy), so the
// proxy's relayed capture is the ONLY path a branch push reaches the audit log
// — the same gitPushRecorder the in-process (all/local) backstop uses, so both
// modes land the identical branch artifact / push-failed row.
func (s *Spawner) executorGitGate(ctx context.Context, info agenthost.RunInfo, stores db.Stores) *agentproc.GitProxyConfig {
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return nil
	}
	upstream := ""
	if base, err := resolver.BaseURLFor(ctx, info.OrgID); err != nil {
		delegateLog.Warn("resolve git host base for sidecar git proxy failed; leaving upstream empty (sidecar defaults to github.com)", "org", info.OrgID, "error", err)
	} else {
		upstream = base
	}
	denialHost := agenthost.NewLocal(stores, info)
	return &agentproc.GitProxyConfig{
		Upstream: upstream,
		Authorize: func(ctx context.Context, owner, repo string) (gitproxy.Decision, error) {
			return gitAuthorizeDecision(ctx, stores, info, owner, repo)
		},
		RecordDenial: func(ctx context.Context, denied gitproxy.DeniedGitOp) {
			denialHost.RecordGitDenied(ctx, denied.Owner, denied.Repo, denied.Ref, denied.Op, denied.Reason)
		},
		RecordPush: gitPushRecorder(stores, info),
	}
}

// sidecarProvisionFor builds the callback BringUpRunSidecar invokes with the
// sidecar's published public key. It publishes that key onto the run's claim
// (runs.cred_pubkey) and fires the cred_request doorbell — so the brain seals
// THIS run's bundle to the sidecar's per-run key, not the claiming instance's
// key — then polls run_credentials for the OPAQUE sealed bytes and hands them
// back. The orchestrator never opens them; only the sidecar holds the matching
// private key. This is what replaces the pre-sandbox awaitCredentials unseal.
//
// A timeout (brain down at claim time) surfaces as an error, failing the
// bring-up; the dispatcher then requeues the run's setup like any transient
// setup failure. Only the executor role reaches here (bringUpExecutorSandbox
// returns nil otherwise).
func (s *Spawner) sidecarProvisionFor(orgID, runID string) agentproc.SidecarProvisionFunc {
	myID, myBootEpoch := s.executorIdentity()
	return func(provCtx context.Context, sidecarPubKeyB64 string) ([]byte, int64, error) {
		// Publish the sidecar's per-run pubkey onto the claim + fire the
		// cred_request doorbell. MarkAwaitingCredentials is the same call the
		// old pre-sandbox gate used, now carrying the recipient key so the
		// brain seals to it (credprovision reads claim.CredPubKey).
		if _, err := s.runQueue.MarkAwaitingCredentials(provCtx, orgID, runID, sidecarPubKeyB64); err != nil {
			return nil, 0, fmt.Errorf("mark awaiting-credentials for run %s: %w", runID, err)
		}

		timeout, pollInterval := s.awaitingCredentialsKnobs()
		deadline := time.Now().Add(timeout)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			credExecutorID, bootEpoch, sealed, ok, err := s.runCredentials.Get(provCtx, orgID, runID)
			if err != nil {
				dispatchLog.Warn("read run credential bundle failed; retrying", "run", runID, "error", err)
			} else if ok && credExecutorID == myID && bootEpoch == myBootEpoch {
				// Opaque ciphertext — the orchestrator relays it verbatim and
				// never opens it (only the sidecar's private key can). Gate on the
				// bundle's stamped (executor, boot_epoch) matching ours: a row left
				// by a prior claimant — a dead executor's, or an earlier boot's —
				// is sealed to a sidecar key this run never minted, so relaying it
				// would just fail the unseal. Skip and keep polling until the brain
				// re-seals for THIS claim.
				return sealed, bootEpoch, nil
			}
			select {
			case <-provCtx.Done():
				return nil, 0, provCtx.Err()
			case <-ticker.C:
				if time.Now().After(deadline) {
					return nil, 0, fmt.Errorf("timed out waiting for run %s credential bundle (brain not provisioning)", runID)
				}
			}
		}
	}
}
