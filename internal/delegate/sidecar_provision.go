package delegate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"go.opentelemetry.io/otel/trace"
)

// runSidecar is this engagement's credential sidecar — the process holding its
// unsealed bundle — plus the network that process's proxies bind on and the
// coordinates for reaching them. The dispatcher stands it up on
// TF_ROLE=executor BEFORE the pre-sandbox clone, so the clone (and GetPR, and
// the agenthost) route through those proxies while the orchestrator holds no
// credential. It owns its own teardown, which the dispatcher defers until after
// the agent run returns.
//
// It is NOT the jail. The jail is built later by the cap-broker, out of the network
// and env this hands to agentproc.Run.
type runSidecar struct {
	net  *sandbox.RunNetwork
	proc sandbox.LaunchedSidecar
	conn *sidecarproto.Conn
	res  *sidecarproto.StartProxiesResult

	// conversationID is the key every per-run file under the socket root is named
	// after — the conversation id. Held so teardown can clear this cell's files
	// by the same derivation bring-up cleared them by.
	conversationID string

	// stopRelay stops the credential-refresh relay goroutine; closed once by
	// Close. relayOnce guards the close against a double Close.
	stopRelay chan struct{}
	relayOnce sync.Once

	// credNudge asks that relay goroutine for an out-of-band read-and-push,
	// for a caller that can't wait out the 30s tick — the relayed
	// `workspace add` whose clone needs the just-re-sealed bundle now.
	// Unbuffered — a nudge is served only while that goroutine is alive, and
	// each request carries its own buffered reply channel.
	credNudge chan credRelayNudge
}

// Close tears the run sidecar down in the order that keeps the veth alive
// under anything still using it: stop the refresh relay, stop the supervision
// reader, SIGKILL the sidecar (freeing its proxies + unsealed bundle), remove
// the per-run files that sidecar left under the socket root, then tear down the
// network the proxies were bound on and release its subnet index. Safe on nil.
//
// The file removal sits exactly there — after the sidecar is dead so nothing
// can re-create them behind it, and before the index is released so the
// ownership guard ReleaseRunCellFiles applies still holds (see its doc). It is
// hygiene: the run's next engagement clears whatever is left regardless, and
// this is what keeps a long-lived executor's tmpfs socket root from carrying
// one orphaned pair per run id it ever ran.
func (rs *runSidecar) Close() {
	if rs == nil {
		return
	}
	started := time.Now()
	if rs.stopRelay != nil {
		rs.relayOnce.Do(func() { close(rs.stopRelay) })
	}
	if rs.conn != nil {
		_ = rs.conn.Close()
	}
	if rs.proc != nil {
		_ = rs.proc.Close()
	}
	if rs.net != nil {
		if rs.conversationID != "" {
			sandbox.ReleaseRunCellFiles(rs.conversationID, rs.net.Idx)
		}
		_ = rs.net.Close()
	}
	// A stop that queues a message re-claims the conversation within a scan
	// tick, so this teardown routinely overlaps its own successor's bring-up.
	// Every resource it frees — the subnet index, the sidecar uid derived from
	// it, the netns name — is one the successor may be waiting on, so how long
	// it took is worth having in the log next to the successor's first line.
	if elapsed := time.Since(started); elapsed > slowCellTeardown {
		dispatchLog.Warn("cell teardown was slow; the run's subnet index and sidecar uid stayed held for that long",
			"conversation", rs.conversationID, "took", elapsed)
	}
}

// slowCellTeardown is how long a cell teardown may take before it is logged.
// The work is a supervision-socket close, a SIGKILL, two unlinks and a
// brokered network teardown — none of which waits on the agent — so a teardown
// past this is holding a scarce slot for a reason worth naming.
const slowCellTeardown = 2 * time.Second

// credRelayNudge is a one-shot request to the credential-refresh relay: re-read
// the sealed-bundle channel now and push any new blob to the sidecar, replying
// on done with the outcome. Routed through that goroutine rather than calling
// conn.Call directly so it stays the only writer of sealed bundles on the
// supervision channel — a second path could hand the sidecar an older blob
// after a newer one.
//
// It carries its caller's ctx because it IS the caller's request: the relay
// runs the DB read and the sidecar Call under it, so cancelling the nudge
// actually stops the work rather than just abandoning it. Without that, a
// caller that gave up would leave a read+Call running for the relay's own step
// budget, and "bounded by ctx" would be a claim the code doesn't keep.
type credRelayNudge struct {
	ctx  context.Context
	done chan error
}

// nudgeCredentialRelay drives one out-of-band read-and-push and waits for it,
// bounded by ctx on both sides — the handoff to the relay goroutine and the
// work it then does. Nil-safe, and safe on a torn-down sidecar: a stopped relay
// answers immediately instead of blocking until ctx expires.
func (rs *runSidecar) nudgeCredentialRelay(ctx context.Context) error {
	if rs == nil || rs.credNudge == nil {
		return nil
	}
	req := credRelayNudge{ctx: ctx, done: make(chan error, 1)}
	select {
	case rs.credNudge <- req:
	case <-rs.stopRelay:
		return errors.New("credential refresh relay has stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// jailEnv is the non-secret env the jail is launched with: the coordinates and
// placeholders the sidecar's proxies produced (git / egress URLs, the single
// GIT_CONFIG_* block, and the LLM proxy for an engagement that dials it from
// inside the jail), threaded into agentproc.RunOptions.PrebuiltProxyEnv.
// nil-safe.
func (rs *runSidecar) jailEnv() []string {
	if rs == nil || rs.res == nil {
		return nil
	}
	return rs.res.Env
}

// engineLLMEnv is this engagement's provider coordinates for an engine that runs in
// THIS process: the sidecar's LLM proxy address plus the per-run placeholder.
// Read separately from jailEnv because the two answer different questions —
// what the jail is pointed at versus what the executor dials — and a native
// engagement's jail is deliberately pointed at nothing. nil-safe.
func (rs *runSidecar) engineLLMEnv() []string {
	if rs == nil || rs.res == nil {
		return nil
	}
	return rs.res.LLMEnv
}

// runNetwork is the prebuilt network handed to agentproc.RunOptions.PrebuiltNetwork.
func (rs *runSidecar) runNetwork() *sandbox.RunNetwork {
	if rs == nil {
		return nil
	}
	return rs.net
}

// ghChannel is the real-gh channel params threaded into agentproc.RunOptions.
// GHChannel when the sidecar bound the injector: its host:port + per-run
// placeholder, plus the per-run cert source path the orchestrator bind-mounts
// (the sidecar wrote it to the same deterministic path). nil when no injector
// was bound (the run then keeps no gh binary/env). conversationID resolves the cert path.
func (rs *runSidecar) ghChannel(conversationID string) *agentproc.GHChannelParams {
	if rs == nil || rs.res == nil || rs.res.GHChannelHost == "" {
		return nil
	}
	return &agentproc.GHChannelParams{
		Host:           rs.res.GHChannelHost,
		Token:          rs.res.GHChannelToken,
		CertSourcePath: agenthost.CertPathFor(conversationID),
	}
}

// GitCloneAuth builds the CloneAuth that routes a host-side fetch of cloneURL
// through this run's sidecar git proxy, so the orchestrator holds no token
// and the real credential is injected host-side on the sidecar's upstream
// hop. Empty (a no-op CloneAuth) when the sidecar exposes no git proxy.
func (rs *runSidecar) GitCloneAuth(cloneURL string) worktree.CloneAuth {
	if rs == nil || rs.res == nil {
		return worktree.CloneAuth{}
	}
	return worktree.CloneAuthViaGitProxy(rs.res.GitProxyURL, cloneHostBase(cloneURL), rs.res.GitProxyToken)
}

// bringUpRunSidecar stands up the run network, launches the credential
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
func (s *Spawner) bringUpRunSidecar(ctx context.Context, orgID string, conv *domain.Conversation, task domain.Task) (*runSidecar, error) {
	if runmode.Current() != runmode.ModeMulti {
		return nil, nil
	}
	// Not wired (a test fixture) — degrade like every other nil-store seam in
	// this package: behave as if the executor path doesn't exist.
	if s.claimCredentials == nil || s.conversationQueue == nil {
		return nil, nil
	}

	// Clear whatever an earlier engagement of this conversation left under the
	// socket root before anything in the new cell is created. The per-run
	// socket and injector cert are keyed by run id, so sequential engagements
	// share those paths, and the sidecar about to be launched can neither
	// truncate nor unlink a file owned by its predecessor's uid — the socket
	// root is sticky. This process can (it owns that directory), and doing it
	// here, ahead of the launch, is what makes an immediate re-claim that
	// races the previous cell's teardown come up instead of failing.
	sandbox.ClearRunCellFiles(conv.ID)

	// The four-process choreography, from the executor's viewpoint. Every leg
	// below is a round trip into a process that exports nothing itself — the
	// cap-broker for the network, the sidecar for the credentials — so this
	// side's client spans are the only place the timings exist.
	ctx, bringUpSpan := tracer.Start(ctx, "engagement.sandbox.bringup")
	defer bringUpSpan.End()

	netCtx, netSpan := tracer.Start(ctx, "sandbox.network.setup")
	net, err := sandbox.SetupRunNetwork(netCtx, conv.ID)
	recordSpanError(netSpan, err)
	netSpan.End()
	if err != nil {
		recordSpanError(bringUpSpan, err)
		return nil, fmt.Errorf("set up run network: %w", err)
	}
	scCtx, scSpan := tracer.Start(ctx, "sandbox.sidecar.launch")
	sc, err := sandbox.LaunchSidecar(scCtx, sandbox.SidecarConfig{ConversationID: conv.ID, SubnetIdx: net.Idx})
	recordSpanError(scSpan, err)
	scSpan.End()
	if err != nil {
		recordSpanError(bringUpSpan, err)
		_ = net.Close()
		return nil, fmt.Errorf("launch credential sidecar: %w", err)
	}

	stores, storesSet := s.getStores()
	info := agenthost.ConversationInfo{
		OrgID:            orgID,
		UserID:           conv.CreatorUserID,
		ConversationID:   conv.ID,
		TeamID:           conv.TeamID,
		IsEventTriggered: conv.TriggerType == domain.TriggerTypeEvent,
	}

	// Git gate for the sidecar's git proxy: the DB-backed authorize/denial the
	// proxy relays back per push, plus the non-secret org base as the insteadOf
	// upstream. No TokenSource — the sidecar resolves the real token from its
	// own unsealed bundle. Omitted (no git proxy) when stores are unwired.
	//
	// The gate's audit client is held here so bring-up can tell it which GitHub
	// credential the sidecar ended up injecting; its denial and push rows carry
	// that credential, and nothing on this side can read it off the bundle.
	var (
		git       *agentproc.GitProxyConfig
		auditHost *agenthost.LocalClient
	)
	if storesSet {
		auditHost = agenthost.NewLocal(stores, info)
		git = s.managedGitGate(info, stores, auditHost)
	}

	// The commit identity's git config pairs (author/committer) fold into the
	// sandbox's single GIT_CONFIG_* block alongside the proxy routing — resolved
	// here because on the executor path agentproc runs no ConfigureProxies to
	// fold them itself.
	identity := s.resolveCommitIdentity(ctx, orgID, conv.TriggerType, conv.CreatorUserID)

	// Held so its proxy coords can be set once bring-up returns them (below):
	// the relayed workspace-add materialization clones through the run's proxies.
	relaySrv := agenthost.NewRelayServer(stores, info, git)
	params := agentproc.SidecarBringUpParams{
		HostVethIP: net.HostIP,
		Git:        git,
		// The org-bound op server the sidecar's relay envelope dispatches to:
		// the git proxy's push authz/audit (backed by the same git gate) plus
		// the exec verb-trace DB ops, all identity-bound from ConversationInfo. Holds
		// the stores + secrets the capless sidecar cannot.
		Relay:         relaySrv,
		IdentityPairs: githooks.IdentityConfigPairs(identity.Name, identity.Email),
		// Every delegated run may touch a GitHub repo (clone/push + GetPR +
		// agenthost gh verbs), so the git + GitHub-REST proxies are always on.
		// No upstream travels with them: the host each lane forwards to is the
		// one sealed into this run's bundle beside the token, so a base-URL
		// change between provisioning and bring-up cannot point a token
		// resolved for one host at another.
		GitHubAPIEnabled: true,
		// The real-gh channel's injector: the sandboxed gh binary reaches it via
		// GH_HOST. On for every delegated run (same rationale as the REST proxy);
		// the injector needs GitHub creds, which resolveGitHub already provisions.
		GHChannelEnabled: true,
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
			ConversationID: info.ConversationID,
			EventTriggered: info.IsEventTriggered,
		},
	}

	res, conn, err := agentproc.BringUpRunSidecar(ctx, sc, s.sidecarProvisionFor(orgID, conv.ID), params)
	if err != nil {
		recordSpanError(bringUpSpan, err)
		_ = sc.Close()
		_ = net.Close()
		return nil, fmt.Errorf("bring up credential sidecar: %w", err)
	}
	// The engagement root, captured once, so every op this run's relay server
	// later dispatches can link back to the setup that created it. Captured
	// rather than propagated: the sidecar frames carry no trace context by
	// design, and this server already knows which run it belongs to.
	relaySrv.SetEngagementSpanContext(s.engagementSpanContext(conv.ID))
	// Hand the relay server the run's now-known proxy coords so a relayed
	// `workspace add` clones + fetches PRs through them — the orchestrator holds
	// no real credential for either. Set before the agent runs, so always ready
	// by the time a materialization arrives.
	relaySrv.SetProxyCreds(&agenthost.ProxyCredentials{
		GitHubCredential: res.GitHubCredential,
		GitHubAPIURL:     res.GitHubAPIURL,
		GitHubAPIToken:   res.GitHubAPIToken,
		JiraAPIURL:       res.JiraAPIURL,
		JiraAPIToken:     res.JiraAPIToken,
		JiraDeployment:   res.JiraDeployment,
		GitProxyURL:      res.GitProxyURL,
		GitProxyToken:    res.GitProxyToken,
	})
	// The tier of the credential the sidecar injects, which only it could read
	// off the sealed bundle. Handed to both recorders on this side so a relayed
	// gh-channel write, a git denial and a branch push all name the credential
	// that made them. Set before the agent runs, like the coords above.
	relaySrv.SetGitHubCredential(res.GitHubCredential)
	if auditHost != nil {
		auditHost.SetGitHubCredential(res.GitHubCredential)
	}

	es := &runSidecar{conversationID: conv.ID, net: net, proc: sc, conn: conn, res: res, stopRelay: make(chan struct{}), credNudge: make(chan credRelayNudge)}
	_, myBootEpoch := s.executorIdentity()

	// A relayed `workspace add` widens this run's authorized repo set, which
	// its already-sealed bundle predates — so the materialization waits for a
	// bundle sealed after the reservation and has it pushed to the sidecar
	// before it clones. Both halves are timestamp-and-ciphertext only: the
	// orchestrator still never opens a bundle.
	relaySrv.SetCredentialRefresh(&agenthost.CredentialRefresh{
		SealedAt: func(ctx context.Context) (time.Time, bool, error) {
			b, ok, err := s.claimCredentials.Get(ctx, orgID, conv.ID)
			if err != nil || !ok || b.BootEpoch != myBootEpoch {
				return time.Time{}, false, err
			}
			return b.SealedAt, true, nil
		},
		Relay: es.nudgeCredentialRelay,
	})

	// The sidecar received this run's bundle once, at bring-up, but the brain's
	// refresh sweep re-mints short-lived credentials mid-run (role-mode STS at
	// its half-life; hour-lived GitHub installation tokens) and re-seals them
	// into claim_credentials against the SAME per-run key. The sidecar is capless
	// with no DB access, so it can't pull them — this goroutine relays each new
	// sealed blob down (the sidecar idempotently re-accepts it) so a long run
	// keeps signing with live credentials instead of 502-ing on expiry.
	go s.relayCredentialRefreshes(conv.ID, func(ctx context.Context) (int64, []byte, bool, error) {
		b, ok, err := s.claimCredentials.Get(ctx, orgID, conv.ID)
		return b.BootEpoch, b.Sealed, ok, err
	}, myBootEpoch, conn, es.credNudge, es.stopRelay)
	return es, nil
}

// credRefreshRelayInterval is how often the orchestrator re-reads
// claim_credentials and relays a changed sealed bundle to the sidecar. Well
// inside every credential's refresh half-life (the brain re-mints long before
// expiry), so the sidecar never serves a stale credential in practice.
const credRefreshRelayInterval = 30 * time.Second

// credBundleGetter reads the current sealed bundle for a run: its boot_epoch,
// the ciphertext, whether a row exists, and any read error. It keys
// claimCredentials.Get by run id. relayCredentialRefreshes never unseals the
// bytes.
type credBundleGetter func(ctx context.Context) (bootEpoch int64, sealed []byte, ok bool, err error)

// credRelayStepTimeout bounds one read-and-push through the sealed-bundle
// channel — the DB read plus the supervision-channel Call.
const credRelayStepTimeout = 20 * time.Second

// relayCredentialRefreshes polls the sealed-bundle channel for one run and
// relays any NEW sealed bundle down to the sidecar over the supervision channel
// — the push half of mid-run credential refresh (the sidecar can't reach the
// DB). It relays only when the sealed bytes actually change (the brain re-mint),
// never unsealing them. subject is the run id for logging. nudge carries
// out-of-band requests from a caller that can't wait out the tick (the relayed
// `workspace add`); nil-safe for a caller with nothing to nudge. Runs until
// stop is closed (runSidecar.Close) or the supervision channel dies. Never
// fires on all/local (no run sidecar).
func (s *Spawner) relayCredentialRefreshes(subject string, getter credBundleGetter, bootEpoch int64, conn *sidecarproto.Conn, nudge <-chan credRelayNudge, stop <-chan struct{}) {
	if getter == nil {
		return
	}
	ticker := time.NewTicker(credRefreshRelayInterval)
	defer ticker.Stop()
	// The bytes last handed to the sidecar, so a pass that reads the same blob
	// does not re-relay it. Starts nil: the first pass re-relays whatever
	// bring-up already sent (the sidecar re-accepts it idempotently), and only a
	// genuine brain re-mint changes the blob thereafter. Owned solely by this
	// goroutine — every pass, ticked or nudged, runs on it.
	var lastSealed []byte
	// parent bounds this pass: the ticker's own detached context, or a nudger's
	// (so its cancellation stops the read and the Call, not just the wait for
	// them). credRelayStepTimeout is the cap either way; a nudger with a shorter
	// deadline wins.
	relayOnce := func(parent context.Context) error {
		ctx, cancel := context.WithTimeout(parent, credRelayStepTimeout)
		defer cancel()
		be, sealed, ok, err := getter(ctx)
		if err != nil {
			return fmt.Errorf("read sealed credential bundle: %w", err)
		}
		if !ok || be != bootEpoch || len(sealed) == 0 || bytes.Equal(sealed, lastSealed) {
			return nil
		}
		if err := conn.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealed, BootEpoch: be}, nil); err != nil {
			return fmt.Errorf("relay sealed credential bundle to sidecar: %w", err)
		}
		lastSealed = sealed
		return nil
	}
	for {
		select {
		case <-stop:
			return
		case <-conn.Done():
			return
		case req := <-nudge:
			// Buffered reply channel, so a nudger that already gave up on its
			// own deadline can't wedge this goroutine.
			req.done <- relayOnce(req.ctx)
		case <-ticker.C:
			if err := relayOnce(context.Background()); err != nil {
				dispatchLog.Warn("credential refresh relay pass failed; will retry next tick", "subject", subject, "error", err)
			}
		}
	}
}

// managedGitGate builds the shared DB-backed policy and audit half of managed
// Git. Local's loopback proxy calls it directly; the multi sidecar relays to it.
// It carries the DB-backed Authorize (team-tracks + conversation_worktrees
// ledger + ref allowlist), the RecordDenial audit, and the RecordPush capture.
// No TokenSource / ProbeCredentials — the sidecar resolves and probes the real
// token from its own unsealed bundle — and no Upstream: the sidecar reads the
// host off that same bundle, so only local's caller, which has no bundle, fills
// one in. Returns nil when the resolver is unwired.
//
// RecordPush is load-bearing on the executor: the pre-push hook stands down in
// the sandbox (the sidecar's StartRunProxies sets PushCaptureProxy), so the
// proxy's relayed capture is the ONLY path a branch push reaches the audit log
// — the same gitPushRecorder the in-process (all/local) backstop uses, so both
// modes land the identical branch artifact / push-failed row.
//
// auditHost is the caller's — not built here — because the run's acting GitHub
// credential is not known until the sidecar reports it, and the caller is the
// half that holds both the client and that answer.
func (s *Spawner) managedGitGate(info agenthost.ConversationInfo, stores db.Stores, auditHost *agenthost.LocalClient) *agentproc.GitProxyConfig {
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return nil
	}
	// Captured while the engagement's root is still live — the gate is built
	// during bring-up, but every callback below fires mid-run, on a git
	// operation the agent performed. Same reason the permission handler
	// captures at construction.
	engagement := s.engagementSpanContext(info.ConversationID)
	recordPush := gitPushRecorder(auditHost, info)
	return &agentproc.GitProxyConfig{
		Authorize: func(ctx context.Context, owner, repo string) (gitproxy.Decision, error) {
			// The push gate is synchronous in front of the agent's git — a
			// slow one stalls a push with no other signal that it did.
			// owner/repo are deliberately absent from the span: a repo name
			// is tenant data, and the decision's shape is what matters here.
			ctx, span := startPunctualLinked(ctx, engagement, info.ConversationID, "git.authorize", telemetry.OrgID(info.OrgID))
			defer span.End()
			dec, err := gitAuthorizeDecision(ctx, stores, info, owner, repo)
			recordSpanError(span, err)
			if err == nil {
				span.SetAttributes(telemetry.Outcome(gitAuthorizeOutcome(dec)))
			}
			return dec, err
		},
		RecordDenial: func(ctx context.Context, denied gitproxy.DeniedGitOp) {
			auditHost.RecordGitDenied(ctx, denied.Owner, denied.Repo, denied.Ref, denied.Op, denied.Reason)
		},
		RecordPush: func(ctx context.Context, pushed gitproxy.PushedRef) {
			// This is the ONLY path a branch push reaches the audit log on an
			// executor — the pre-push hook stands down inside the sandbox —
			// so "was it even called" is a real question when an artifact is
			// missing. The recorder swallows its own errors by contract, so
			// the span reports that the relay arrived and how long the write
			// took, not whether it succeeded; the store spans beneath it carry
			// that.
			ctx, span := startPunctualLinked(ctx, engagement, info.ConversationID, "git.record_push", telemetry.OrgID(info.OrgID))
			defer span.End()
			recordPush(ctx, pushed)
		},
	}
}

// gitAuthorizeOutcome names how the push gate answered, as the closed enum
// the span carries: the decision's own allow/deny plus its deny reason,
// which is a fixed vocabulary (untracked repo, protected ref, ...) rather
// than the human-readable message beside it.
func gitAuthorizeOutcome(dec gitproxy.Decision) string {
	if dec.Allowed {
		return "allowed"
	}
	if dec.DenyReason != "" {
		return "denied_" + dec.DenyReason
	}
	return "denied"
}

// sidecarProvisionFor builds the callback BringUpRunSidecar invokes with the
// sidecar's published public key. It publishes that key onto the run's claim
// (claims.cred_pubkey) and fires the cred_request doorbell — so the brain seals
// THIS run's bundle to the sidecar's per-run key, not the claiming instance's
// key — then polls claim_credentials for the OPAQUE sealed bytes and hands them
// back. The orchestrator never opens them; only the sidecar holds the matching
// private key. This is what replaces the pre-sandbox awaitCredentials unseal.
//
// A timeout (brain down at claim time) surfaces as an error, failing the
// bring-up; the dispatcher then requeues the run's setup like any transient
// setup failure. Only the executor role reaches here (bringUpRunSidecar
// returns nil otherwise).
func (s *Spawner) sidecarProvisionFor(orgID, conversationID string) agentproc.SidecarProvisionFunc {
	myID, myBootEpoch := s.executorIdentity()
	return func(provCtx context.Context, sidecarPubKeyB64 string) ([]byte, int64, error) {
		// The awaiting-credentials park, measured from the run's side: how long
		// this engagement sat between ringing the doorbell and holding a bundle
		// sealed to its own sidecar. That interval is somebody else's work — the
		// brain's provisioner, on another pod — and this span is deliberately
		// NOT joined to it: the tf_ctl doorbell is lossy, the sweep is the real
		// completion path, and a link would promise a 1:1 handoff that does not
		// exist. Both sides carry conversation.id, which is the join.
		provCtx, span := tracer.Start(provCtx, "engagement.credentials.await",
			trace.WithAttributes(telemetry.ConversationID(conversationID), telemetry.OrgID(orgID)))
		defer span.End()

		// Publish the sidecar's per-run pubkey onto the claim + fire the
		// cred_request doorbell. MarkAwaitingCredentials is the same call the
		// old pre-sandbox gate used, now carrying the recipient key so the
		// brain seals to it (credprovision reads claim.CredPubKey).
		if _, err := s.conversationQueue.MarkAwaitingCredentials(provCtx, orgID, conversationID, sidecarPubKeyB64); err != nil {
			recordSpanError(span, err)
			return nil, 0, fmt.Errorf("mark awaiting-credentials for conversation %s: %w", conversationID, err)
		}

		timeout, pollInterval := s.awaitingCredentialsKnobs()
		deadline := time.Now().Add(timeout)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		polls := 0
		for {
			polls++
			b, ok, err := s.claimCredentials.Get(provCtx, orgID, conversationID)
			if err != nil {
				dispatchLog.Warn("read claim credential bundle failed; retrying", "conversation", conversationID, "error", err)
			} else if ok && b.ExecutorID == myID && b.BootEpoch == myBootEpoch {
				// Opaque ciphertext — the orchestrator relays it verbatim and
				// never opens it (only the sidecar's private key can). Gate on the
				// bundle's stamped (executor, boot_epoch) matching ours: a row left
				// by a prior claimant — a dead executor's, or an earlier boot's —
				// is sealed to a sidecar key this run never minted, so relaying it
				// would just fail the unseal. Skip and keep polling until the brain
				// re-seals for THIS claim.
				//
				// The poll count separates "the brain answered the doorbell"
				// from "the sweep eventually got to it" — the same elapsed time
				// means different things for the two, and only one of them is a
				// working doorbell.
				span.SetAttributes(telemetry.Count(polls), telemetry.Outcome("sealed"))
				return b.Sealed, b.BootEpoch, nil
			}
			select {
			case <-provCtx.Done():
				recordSpanError(span, provCtx.Err())
				return nil, 0, provCtx.Err()
			case <-ticker.C:
				if time.Now().After(deadline) {
					err := fmt.Errorf("timed out waiting for conversation %s credential bundle (brain not provisioning)", conversationID)
					span.SetAttributes(telemetry.Count(polls))
					recordSpanError(span, err)
					return nil, 0, err
				}
			}
		}
	}
}
