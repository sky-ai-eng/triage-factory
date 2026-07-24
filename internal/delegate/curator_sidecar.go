package delegate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// BringUpCuratorSandbox stands up the run network + credential sidecar for a
// homed curator turn — the curator-turn analog of
// bringUpExecutorSandbox, keyed by the CONVERSATION id: the sealed-bundle
// channel (claim_credentials) resolves the conversation's active claim, and
// one active claim per conversation makes the id unique per concurrent
// sandbox/network/socket. Curator wires it through the SetTurnSandbox seam,
// gated on the executor runtime, so local (where this returns nil) keeps the
// in-process agenthost.Start path byte-identical.
//
// Gated on the MODE, not the role, for the same reason as
// bringUpExecutorSandbox: multi is always per-run-isolated, and a future
// multi caller must never silently fall back to in-process credential
// resolution. Returns (nil, nil) in local mode and on an unwired fixture (nil
// curator stores) — the caller then keeps the in-process path. On any error
// the partial state (network, sidecar) is already torn down; the caller does
// not Close a nil return.
//
// pinnedRepos is the turn's authorized GitHub set ("owner/repo"): it gates the
// sidecar's git proxy (pinned ∩ tracked) and, carried on RunInfo/AgentHostInfo,
// authorizes the agent's exec-gh verbs against the same set. userID is the
// requesting user; teamID the curated project's owning team.
func (s *Spawner) BringUpCuratorSandbox(ctx context.Context, orgID, conversationID, userID, teamID string, pinnedRepos []string) (*executorSandbox, error) {
	if runmode.Current() != runmode.ModeMulti {
		return nil, nil
	}
	// Not wired (a test fixture) — degrade like every other nil-store seam here.
	if s.runCredentials == nil || s.curatorStore == nil {
		return nil, nil
	}

	net, err := sandbox.SetupRunNetwork(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("set up curator turn network: %w", err)
	}
	sc, err := sandbox.LaunchSidecar(ctx, sandbox.SidecarConfig{RunID: conversationID, SubnetIdx: net.Idx})
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("launch curator credential sidecar: %w", err)
	}

	stores, storesSet := s.getStores()
	info := agenthost.RunInfo{
		OrgID:            orgID,
		UserID:           userID,
		RunID:            conversationID,
		TeamID:           teamID,
		IsEventTriggered: false,
		PinnedRepos:      pinnedRepos,
	}

	// Git gate for the sidecar's git proxy: authorize a fetch of a repo that is
	// pinned ∩ team-tracked (a curator turn has no conversation_worktrees ledger). The
	// sidecar resolves the real token from its own unsealed bundle; no
	// TokenSource here.
	var git *agentproc.GitProxyConfig
	if storesSet {
		git = s.executorCuratorGitGate(ctx, info, stores, pinnedRepos)
	}

	// The org commit identity's git-config pairs fold into the sandbox's single
	// GIT_CONFIG block. A curator turn never commits (pinned worktrees are
	// read-only), but resolving it keeps the block shape identical to a delegated
	// run. "manual" — every curator turn is user-driven.
	identity := s.resolveCommitIdentity(ctx, orgID, "manual", userID)

	params := agentproc.SidecarBringUpParams{
		HostVethIP:    net.HostIP,
		Git:           git,
		Relay:         agenthost.NewRelayServer(stores, info, git),
		IdentityPairs: githooks.IdentityConfigPairs(identity.Name, identity.Email),
		// The curator advertises `exec gh` + host-side pinned-repo fetches, so
		// the git + GitHub-REST proxies are always on.
		GitHubAPIEnabled:  true,
		GitHubAPIUpstream: s.githubAPIUpstreamFor(ctx, orgID),
		// The real-gh channel injector, same as a delegated run — the curator's
		// authorized set is the pinned ∩ tracked repos its CLIToken is scoped to.
		GHChannelEnabled:  true,
		GHChannelUpstream: s.githubAPIUpstreamFor(ctx, orgID),
		// Jira REST only when the org has Jira configured. A curator turn has no
		// task to read EntitySource off (the run path's signal), so the non-secret
		// org signal stands in — the bundle carries Jira creds on the same
		// condition, so the proxy never binds credential-less.
		JiraAPIEnabled: s.orgHasJira(ctx, orgID),
		// Relocate the exec-verb socket server into the sidecar, carrying the
		// turn's non-secret identity + its pinned authorized set.
		AgentHost: &sidecarproto.AgentHostInfo{
			OrgID:          info.OrgID,
			UserID:         info.UserID,
			TeamID:         info.TeamID,
			RunID:          info.RunID,
			EventTriggered: info.IsEventTriggered,
			PinnedRepos:    info.PinnedRepos,
		},
	}

	res, conn, err := agentproc.BringUpRunSidecar(ctx, sc, s.curatorSidecarProvisionFor(orgID, conversationID), params)
	if err != nil {
		_ = sc.Close()
		_ = net.Close()
		return nil, fmt.Errorf("bring up curator credential sidecar: %w", err)
	}

	es := &executorSandbox{net: net, sidecar: sc, conn: conn, res: res, stopRelay: make(chan struct{})}
	// Same mid-flight refresh relay as a delegated run: the brain re-mints and
	// re-seals the turn's short-lived credentials into claim_credentials
	// (keyed by the conversation's active claim), and this goroutine relays
	// each new sealed blob down so a long turn keeps signing with live
	// credentials.
	_, myBootEpoch := s.executorIdentity()
	go s.relayCredentialRefreshes(conversationID, func(ctx context.Context) (int64, []byte, bool, error) {
		_, be, sealed, ok, err := s.runCredentials.Get(ctx, orgID, conversationID)
		return be, sealed, ok, err
	}, myBootEpoch, conn, es.stopRelay)
	return es, nil
}

// curatorSidecarProvisionFor is the curator analog of sidecarProvisionFor: it
// publishes the sidecar's per-turn pubkey onto the conversation's ACTIVE
// claim (and — Postgres — fires the curator_cred_request doorbell, both
// inside PublishTurnCredPubKeySystem) so the brain seals THIS turn's bundle
// to it, then polls the shared claim_credentials channel for the OPAQUE
// sealed bytes. The orchestrator never opens the bundle; only the sidecar
// holds the matching private key.
func (s *Spawner) curatorSidecarProvisionFor(orgID, conversationID string) agentproc.SidecarProvisionFunc {
	_, myBootEpoch := s.executorIdentity()
	return func(provCtx context.Context, sidecarPubKeyB64 string) ([]byte, int64, error) {
		if _, err := s.curatorStore.PublishTurnCredPubKeySystem(provCtx, orgID, conversationID, sidecarPubKeyB64); err != nil {
			return nil, 0, fmt.Errorf("publish curator turn %s sidecar pubkey: %w", conversationID, err)
		}

		timeout, pollInterval := s.awaitingCredentialsKnobs()
		deadline := time.Now().Add(timeout)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			_, bootEpoch, sealed, ok, err := s.runCredentials.Get(provCtx, orgID, conversationID)
			if err != nil {
				dispatchLog.Warn("read curator turn credential bundle failed; retrying", "conversation", conversationID, "error", err)
			} else if ok && bootEpoch == myBootEpoch {
				return sealed, bootEpoch, nil
			}
			select {
			case <-provCtx.Done():
				return nil, 0, provCtx.Err()
			case <-ticker.C:
				if time.Now().After(deadline) {
					return nil, 0, fmt.Errorf("timed out waiting for curator turn %s credential bundle (brain not provisioning)", conversationID)
				}
			}
		}
	}
}

// executorCuratorGitGate builds the orchestrator-side git gate the curator
// sidecar's git proxy relays to: the non-secret org base as the
// insteadOf upstream, plus a DB-backed Authorize that admits a fetch of a repo
// that is pinned ∩ team-tracked. RecordPush is kept for symmetry (inert —
// pinned worktrees are read-only, so no push transits). No TokenSource — the
// sidecar resolves the real token from its own unsealed bundle. nil when the
// resolver is unwired.
func (s *Spawner) executorCuratorGitGate(ctx context.Context, info agenthost.RunInfo, stores db.Stores, pinnedRepos []string) *agentproc.GitProxyConfig {
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return nil
	}
	upstream := ""
	if base, err := resolver.BaseURLFor(ctx, info.OrgID); err != nil {
		delegateLog.Warn("resolve git host base for curator sidecar git proxy failed; leaving upstream empty (sidecar defaults to github.com)", "org", info.OrgID, "error", err)
	} else {
		upstream = base
	}
	denialHost := agenthost.NewLocal(stores, info)
	return &agentproc.GitProxyConfig{
		Upstream: upstream,
		Authorize: func(ctx context.Context, owner, repo string) (gitproxy.Decision, error) {
			return curatorGitAuthorizeDecision(ctx, stores, info, pinnedRepos, owner, repo)
		},
		RecordDenial: func(ctx context.Context, denied gitproxy.DeniedGitOp) {
			denialHost.RecordGitDenied(ctx, denied.Owner, denied.Repo, denied.Ref, denied.Op, denied.Reason)
		},
		RecordPush: gitPushRecorder(stores, info),
	}
}

// curatorGitAuthorizeDecision is the curator git proxy's live per-repo gate: a
// repo is authorized only if the team tracks it AND it is in the turn's pinned
// set. Returns Allowed with no AllowedRefs — enough for a fetch (upload-pack),
// which is all a curator turn does, while the empty ref allowlist means any
// push (receive-pack) is refused. Fails closed (deny) when a store it needs is
// absent or a lookup errors.
//
// Tracking is checked before pinned-set membership so the denial matches the
// exec-gh gate's ordering (cmd/exec/agenthost/local.go authorizeRepo): an
// untracked repo is an admin problem (repo-not-tracked) whether or not it was
// pinned, and only a tracked-but-unpinned repo is the curator-specific
// "not attached to this project" (repo-not-attached).
func curatorGitAuthorizeDecision(ctx context.Context, stores db.Stores, info agenthost.RunInfo, pinnedRepos []string, owner, repo string) (gitproxy.Decision, error) {
	if stores.TeamGitHubRepos == nil {
		return gitproxy.Decision{Allowed: false}, nil
	}
	repoID := owner + "/" + repo
	tracks, err := stores.TeamGitHubRepos.TracksRepoSystem(ctx, info.TeamID, owner, repo)
	if err != nil {
		return gitproxy.Decision{}, err
	}
	if !tracks {
		return gitDenyNotTracked(repoID), nil
	}
	for _, p := range pinnedRepos {
		if strings.EqualFold(p, repoID) {
			return gitproxy.Decision{Allowed: true}, nil
		}
	}
	return gitDenyNotAttached(repoID), nil
}

// orgHasJira reports whether the org has a Jira base URL configured — the
// non-secret org signal (org_settings, never Secrets) the curator sidecar uses
// to decide whether to bind its Jira proxy, standing in for the run path's
// task.EntitySource == "jira" signal. A read error assumes no Jira (fail
// closed), matching the bundle: a Jira-less org's bundle carries no Jira
// credential, so binding the proxy would fail.
func (s *Spawner) orgHasJira(ctx context.Context, orgID string) bool {
	if s.orgs == nil {
		return false
	}
	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("read org settings for curator jira proxy gate failed; assuming no jira", "org", orgID, "error", err)
		return false
	}
	return settings.JiraBaseURL != ""
}
