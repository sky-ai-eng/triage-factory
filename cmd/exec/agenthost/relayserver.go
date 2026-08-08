package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"go.opentelemetry.io/otel/trace"
)

// RelayServer is the orchestrator-side op server the sidecar's relay envelope
// dispatches to (it implements agentproc.RelayDispatcher). It holds the run's
// db.Stores + RunInfo + git gate — everything the capless sidecar cannot — and
// binds identity from RunInfo on every op, so a relayed op is structurally
// unable to address another org's data (the wire carries no org id). One
// RelayServer per run, built by the delegate at sidecar bring-up.
//
// It serves three op families:
//   - core git ops (authorize_repo / record_denial / record_push) backed by the
//     injected GitProxyConfig;
//   - core DB ops (the exec verb-trace reads/writes) served through the SAME
//     directRuntime an all/local LocalClient uses, so a relayed op and an
//     in-process one are byte-identical DB effects;
//   - provider policy ops (a Slack channel authz) dispatched to the registered
//     provider under its own namespace.
type RelayServer struct {
	// rt is the in-process runtime the core DB ops execute through — the same
	// directRuntime an all/local LocalClient holds, so the tx/pool branch is
	// identical whether a run is sandboxed or not.
	rt *directRuntime

	stores db.Stores
	info   RunInfo

	// audit is the host-side client the relayed audit-only ops record through —
	// the same seam the in-process git gate uses (executorGitGate's
	// denialHost), so a sandbox's denial and a local run's land the identical
	// row through the identical method.
	audit *LocalClient

	// git is the run's push authz + audit gate (the delegate's executorGitGate).
	// nil for a run with no git surface (a Jira-only run) — authorize_repo then
	// fails closed (deny), the audit ops no-op.
	git *agentproc.GitProxyConfig

	// proxyCreds are the run's per-run proxy coordinates (REST + git proxy, with
	// placeholder tokens), set once the sidecar bring-up returns them (SetProxyCreds).
	// The workspace materialization served here clones + GetPRs through them, so the
	// orchestrator holds no real credential for either. nil until bring-up completes.
	proxyCreds *ProxyCredentials

	// credRefresh is the sealed-bundle freshness gate the workspace
	// materialization waits on. Optional: only the delegated executor path
	// wires it (SetCredentialRefresh) — local mode has no sealed bundle at all
	// and the curator's own RelayServer never serves a `workspace add`, so both
	// leave it nil and the wait no-ops.
	credRefresh *CredentialRefresh

	// engagement is the trace context of the engagement that built this
	// server, captured once at bring-up (SetEngagementSpanContext). It is how
	// a relayed op — which arrives long after the setup span ended, over a
	// wire that deliberately carries no trace context — still names the run it
	// belongs to. The zero value (local mode, the curator, an untraced
	// process) simply produces unlinked spans.
	engagement trace.SpanContext
}

// SetEngagementSpanContext records the engagement's trace root so every op
// dispatched here links back to it. Injected late for the same reason
// SetProxyCreds is: the run's identity in the trace only exists once the
// engagement that owns the sidecar has started.
func (s *RelayServer) SetEngagementSpanContext(sc trace.SpanContext) { s.engagement = sc }

// CredentialRefresh is the orchestrator's read-only handle on the run's sealed
// credential bundle. Neither half opens it: SealedAt reports only
// claim_credentials.created_at, and Relay hands the opaque bytes to the
// executor goroutine that already owns the sidecar hop. The orchestrator's
// no-unseal posture (Property B) is therefore untouched by the wait below.
type CredentialRefresh struct {
	// SealedAt reports when the run's current bundle was sealed, and whether
	// one exists for this executor's claim at all.
	SealedAt func(ctx context.Context) (time.Time, bool, error)

	// Relay asks the credential-refresh relay to re-read the bundle channel and
	// push any new blob down to the sidecar now, returning once that push has
	// landed. The 30s refresh tick would get there eventually; a clone starting
	// milliseconds after the reservation cannot wait for it.
	Relay func(ctx context.Context) error
}

// SetCredentialRefresh wires the sealed-bundle freshness gate. Optional and
// nil-safe, injected late for the same reason SetProxyCreds is: the bundle
// channel's coordinates only exist once the sidecar is up.
func (s *RelayServer) SetCredentialRefresh(cr *CredentialRefresh) { s.credRefresh = cr }

// SetProxyCreds records the run's proxy coordinates once the sidecar bring-up
// has returned them — the create_workspace_checkout op clones and fetches PRs
// through them. Bring-up completes before the agent runs, so the coords are
// always set by the time a `workspace add` arrives; a materialize before that
// (proxyCreds still nil) fails clean rather than cloning unauthenticated.
func (s *RelayServer) SetProxyCreds(pc *ProxyCredentials) { s.proxyCreds = pc }

// NewRelayServer builds the run's relay op server. git may be nil (no git
// surface); stores/info are the run's own, admin-pool + RunInfo-bound.
func NewRelayServer(stores db.Stores, info RunInfo, git *agentproc.GitProxyConfig) *RelayServer {
	return &RelayServer{
		rt:     newDirectRuntime(stores, info),
		stores: stores,
		info:   info,
		git:    git,
		audit:  NewLocal(stores, info),
	}
}

// recordPushRelayTimeout bounds the orchestrator-side artifact write kicked off
// by a relayed record_push, mirroring the in-process backstop's own cap
// (gitproxy.recordPushTimeout) so a wedged store can't hold the supervision
// channel's per-frame goroutine open indefinitely.
const recordPushRelayTimeout = 30 * time.Second

// DispatchCall serves a request/response relay op. See agentproc.RelayDispatcher.
func (s *RelayServer) DispatchCall(ctx context.Context, namespace, op string, args json.RawMessage) (json.RawMessage, error) {
	ctx, span := s.startRelayOp(ctx, "relay.call", namespace, op)
	defer span.End()

	res, err := s.dispatch(ctx, namespace, op, args)
	recordSpanError(span, err)
	return res, err
}

// dispatch routes one request/response op to its family, so DispatchCall is
// the single instrumented choke point rather than two.
func (s *RelayServer) dispatch(ctx context.Context, namespace, op string, args json.RawMessage) (json.RawMessage, error) {
	if namespace == agentproc.RelayNamespaceCore {
		return s.dispatchCoreCall(ctx, op, args)
	}
	// A provider policy op (e.g. slack/authorize_channel): served against the
	// run's stores + RunInfo, identity bound orchestrator-side.
	return dispatchProviderOp(ctx, s.stores, s.info, namespace, op, args)
}

// DispatchNotify serves a fire-and-forget audit relay op best-effort.
func (s *RelayServer) DispatchNotify(ctx context.Context, namespace, op string, args json.RawMessage) {
	ctx, span := s.startRelayOp(ctx, "relay.notify", namespace, op)
	defer span.End()

	if namespace == agentproc.RelayNamespaceCore {
		s.dispatchCoreNotify(ctx, op, args)
		return
	}
	if _, err := dispatchProviderOp(ctx, s.stores, s.info, namespace, op, args); err != nil {
		recordSpanError(span, err)
		agenthostLog.Warn("relay provider notify failed", "namespace", namespace, "op", op, "error", err)
	}
}

// dispatchCoreCall serves the "core" request/response ops: the git proxy's push
// authorization, plus the exec verb-trace DB reads/writes routed through the
// directRuntime.
func (s *RelayServer) dispatchCoreCall(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	switch op {
	case agentproc.OpAuthorizeRepo:
		var a agentproc.AuthorizeRepoArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// No authorize gate wired (a Jira-only run, or a partial fixture) fails
		// closed — a git push the orchestrator can't adjudicate is denied.
		if s.git == nil || s.git.Authorize == nil {
			return json.Marshal(agentproc.AuthorizeRepoReply{Allowed: false})
		}
		dec, err := s.git.Authorize(ctx, a.Owner, a.Repo)
		if err != nil {
			return nil, err
		}
		return json.Marshal(agentproc.AuthorizeRepoReply{
			Allowed:       dec.Allowed,
			AllowedRefs:   dec.AllowedRefs,
			ProtectedRefs: dec.ProtectedRefs,
			DenyReason:    dec.DenyReason,
			DenyMessage:   dec.DenyMessage,
		})

	case opGetConversation:
		run, err := s.rt.GetConversation(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(agentRunResult{Run: run})

	case opGetTask:
		var a getTaskArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		t, err := s.rt.GetTask(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(taskResult{Task: t})

	case opListRepos:
		repos, err := s.rt.ListRepos(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(reposResult{Repos: repos})

	case opGetRepo:
		var a getRepoArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		r, err := s.rt.GetRepo(ctx, a.RepoID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(repoResult{Repo: r})

	case opTeamTracksRepo:
		var a teamTracksRepoArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		tracks, err := s.rt.TeamTracksRepo(ctx, a.Owner, a.Repo)
		if err != nil {
			return nil, err
		}
		return json.Marshal(teamTracksRepoResult{Tracks: tracks})

	case opGetRunWorktreeByRepoRef:
		var a runWorktreeByRepoRefArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		w, err := s.rt.GetRunWorktreeByRepoRef(ctx, a.RepoID, a.Ref)
		if err != nil {
			return nil, err
		}
		return json.Marshal(runWorktreeResult{Worktree: w})

	case opListRunWorktrees:
		w, err := s.rt.ListRunWorktrees(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(runWorktreesResult{Worktrees: w})

	case opInsertRunWorktree:
		var a insertRunWorktreeArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		inserted, winningPath, err := s.rt.InsertRunWorktree(ctx, a.Row)
		if err != nil {
			return nil, err
		}
		return json.Marshal(insertRunWorktreeResult{Inserted: inserted, WinningPath: winningPath})

	case opDeleteRunWorktree:
		var a deleteRunWorktreeByRepoRefArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		if err := s.rt.DeleteRunWorktree(ctx, a.RepoID, a.Ref); err != nil {
			return nil, err
		}
		return nil, nil

	case opListRunArtifacts:
		arts, err := s.rt.ListRunArtifacts(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(listRunArtifactsResult{Artifacts: arts})

	case opOrgJiraBase:
		url, err := s.rt.OrgJiraBaseURL(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(orgJiraBaseResult{URL: url})

	case opBuildAgentFooter:
		var a buildAgentFooterArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		footer, err := s.rt.AgentFooter(ctx, a.Kind)
		if err != nil {
			return nil, err
		}
		return json.Marshal(buildAgentFooterResult{Footer: footer})

	case opUpsertArtifact:
		var a upsertArtifactArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		stored, err := s.rt.UpsertArtifact(ctx, a.Artifact)
		if err != nil {
			return nil, err
		}
		return json.Marshal(upsertArtifactResult{Artifact: stored})

	case opUpdateReviewDetails:
		var a updateReviewDetailsArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		updated, err := s.rt.UpdateReviewDetailsIfPending(ctx, a.ArtifactID, a.DetailsJSON)
		if err != nil {
			return nil, err
		}
		return json.Marshal(updateReviewDetailsResult{Updated: updated})

	case opTransitionReviewState:
		var a transitionReviewStateArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// Relayed as a CALL, never a notify: the sidecar's auto-post path blocks
		// on the claim's answer — publishing without knowing whether it won the
		// CAS is exactly the double-submit this guards.
		moved, err := s.rt.TransitionReviewState(ctx, a.ArtifactID, a.From, a.To, a.ExternalID, a.URL, a.DetailsJSON)
		if err != nil {
			return nil, err
		}
		return json.Marshal(transitionReviewStateResult{Moved: moved})

	case opReviewPosture:
		var a reviewPostureArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// The sidecar relays here because neither input exists in its process:
		// the team settings live behind the stores it doesn't hold, and its own
		// gh clients speak to per-run REST proxies that report a descriptive
		// identity, not the real App-vs-PAT tier. Identity is the run's own
		// RunInfo, so a sidecar cannot read another team's posture.
		res, err := s.rt.ReviewPosture(ctx, a.Owner, a.Repo)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)

	case opCheckEntitlement:
		var a checkEntitlementArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// The orchestrator holds the entitlements provider; the sidecar (whose
		// process has only the Static default) relays here so an extension is
		// gated on the run's real org entitlement.
		allowed, err := s.rt.CheckEntitlement(ctx, a.Feature)
		if err != nil {
			return nil, err
		}
		return json.Marshal(checkEntitlementResult{Allowed: allowed})

	case opMemoryLoad:
		var a memoryLoadArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// The sidecar relays `memory load` here so the entity lookup, the team-
		// scoped memory read, and the best-effort touch all land where the stores
		// live. Identity is the run's own RunInfo (s.rt), so a sidecar cannot
		// steer the read at another org/team's memory.
		res, err := s.rt.MemoryLoad(ctx, a.Source, a.SourceID, a.Limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(memoryLoadResult{Result: res})

	case opCreateWorkspaceCheckout:
		var a createWorkspaceCheckoutArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		if s.proxyCreds == nil {
			return nil, fmt.Errorf("agenthost: workspace materialization requested before the run's git proxy was ready")
		}
		// The reservation this checkout answers widened the run's authorized
		// repo set; the sidecar's bundle predates it. Block until a bundle
		// sealed AFTER the reservation has reached the sidecar, or the clone
		// authenticates with a token the new repo has no entry for.
		if err := s.awaitCredentialsForRepo(ctx, a.Owner, a.Repo); err != nil {
			return nil, err
		}
		// Materialize on the orchestrator, which owns the bare cache + run-root
		// and clones / fetches PRs through the run's proxies. Identity is the
		// run's own RunInfo, so the repo-config + team-tracking gate re-binds to
		// THIS run's team — a sidecar cannot steer it at another team's repo.
		client := &LocalClient{
			stores:     s.stores,
			info:       s.info,
			rt:         s.rt,
			proxyCreds: s.proxyCreds,
			gateWired:  s.stores.TeamGitHubRepos != nil && s.stores.RunWorktrees != nil,
		}
		path, err := client.materializeWorkspaceCheckout(ctx, a.Owner, a.Repo, a.Ref, a.PR)
		if err != nil {
			return nil, err
		}
		return json.Marshal(createWorkspaceCheckoutResult{Path: path})

	default:
		return nil, fmt.Errorf("agenthost: unsupported core relay call op %q", op)
	}
}

// workspaceCredWaitTimeout bounds the wait for a re-sealed credential bundle
// after a `workspace add` widened the run's repo set. Generous against the
// whole chain the doorbell kicks off (NOTIFY → mint → seal → read → relay) and
// well inside the executor's own 2-minute awaiting-credentials deadline, so a
// wedged brain surfaces here as an actionable error rather than as a 502
// halfway through a clone.
const workspaceCredWaitTimeout = 15 * time.Second

// workspaceCredPollInterval is how often the wait re-reads the bundle's seal
// timestamp. The doorbell fired one relay round trip ago (`workspace add`
// reserves the row and materializes in two separate ops), so the brain already
// has a head start and the poll is just the pickup.
const workspaceCredPollInterval = 250 * time.Millisecond

// workspaceCredRelayTimeout bounds handing the re-sealed bundle to the sidecar.
// Budgeted separately from the wait rather than carved out of what's left of
// it: the wait is for the brain to produce a bundle, the push is a local hop
// that shouldn't fail merely because the seal landed on the last poll. The
// enclosing checkout op's own cap (4 minutes) still covers both.
const workspaceCredRelayTimeout = 10 * time.Second

// awaitCredentialsForRepo blocks until the run's sealed credential bundle is
// newer than the conversation_worktrees reservation for repoID — then pushes it
// to the sidecar, which is what actually holds the token the clone will use.
//
// Freshness is decided from claim_credentials.created_at alone, never from the
// bundle's contents: the provisioner recomputes a run's authorized repo set
// from scratch on every pass and the seal timestamp is replaced on every
// re-seal, so "sealed after the row exists" implies "covers the row's repo" by
// construction. That keeps the orchestrator's zero-unseal posture intact.
//
// No-ops when unwired (local mode, curator) or when the run has no reservation
// for this repo — nothing widened, so there is nothing to wait for. A repeat
// `workspace add` finds its original row, whose reservation any seal since then
// already postdates, and returns on the first poll.
//
// One race is knowingly left open: a provision that read the authorized set
// BEFORE the reservation but wrote its bundle after it satisfies the comparison
// without covering the new repo. The clone then fails with the sandbox's own
// no-git-credentials error, and the provision our doorbell kicked off lands
// right behind it, so a retry succeeds. Not papered over with a retry loop —
// closing it properly means recording the covered repo set on the row, which is
// a schema change deliberately deferred.
func (s *RelayServer) awaitCredentialsForRepo(ctx context.Context, owner, repo string) error {
	if s.credRefresh == nil || s.credRefresh.SealedAt == nil {
		return nil
	}
	repoID := owner + "/" + repo
	reservedAt, ok, err := s.repoReservedAt(ctx, repoID)
	if err != nil {
		return fmt.Errorf("agenthost: read worktree reservations for %s: %w", repoID, err)
	}
	if !ok {
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, workspaceCredWaitTimeout)
	defer cancel()
	ticker := time.NewTicker(workspaceCredPollInterval)
	defer ticker.Stop()
	// The poll is fast enough that logging every failed read would bury the
	// signal; one line names the condition and the timeout reports the outcome.
	warned := false
	for {
		sealedAt, ok, err := s.credRefresh.SealedAt(waitCtx)
		if err != nil {
			if !warned {
				warned = true
				agenthostLog.Warn("read sealed credential bundle timestamp failed; retrying until the workspace wait expires", "run", s.info.RunID, "repo", repoID, "error", err)
			}
		} else if ok && sealedAt.After(reservedAt) {
			if s.credRefresh.Relay == nil {
				return nil
			}
			// The bundle is fresh in the database; the sidecar still holds the
			// old one until the relay pushes it down. Bounded off the caller's
			// ctx, not waitCtx — a seal that landed on the final poll deserves a
			// full push budget, not whatever milliseconds the wait had left.
			relayCtx, relayCancel := context.WithTimeout(ctx, workspaceCredRelayTimeout)
			err := s.credRefresh.Relay(relayCtx)
			relayCancel()
			if err != nil {
				return fmt.Errorf("agenthost: hand the re-sealed credential bundle for %s to the run's credential sidecar: %w", repoID, err)
			}
			return nil
		}
		select {
		case <-ticker.C:
		case <-waitCtx.Done():
			return fmt.Errorf("agenthost: %s was added to this run but its git credentials have not been re-issued yet "+
				"(the request is filed and still in flight) — retry `workspace add %s` in a moment", repoID, repoID)
		}
	}
}

// repoReservedAt reports when repoID entered this run's authorized repo set —
// the OLDEST conversation_worktrees row the run holds for it.
//
// Oldest, not newest, because the two ledgers are keyed differently:
// reservations are per (repo, ref) while credentials are per repo (the
// provisioner dedups by repo id). The repo joined the authorized set at its
// first reservation, so a bundle sealed after that covers it — and every later
// ref in the same repo, which adds no grant to wait for. Keying on the newest
// row would make a run's second checkout in a repo it already holds sit out a
// re-seal that changes nothing.
//
// Repo ids are compared case-insensitively because the reservation records the
// agent's spelling while the checkout op carries the repo profile's — the same
// rule the insert's doorbell gate uses, so the two halves agree on what counts
// as the same repo.
func (s *RelayServer) repoReservedAt(ctx context.Context, repoID string) (time.Time, bool, error) {
	rows, err := s.rt.ListRunWorktrees(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	var oldest time.Time
	found := false
	for _, w := range rows {
		if !strings.EqualFold(w.RepoID, repoID) {
			continue
		}
		if !found || w.CreatedAt.Before(oldest) {
			oldest = w.CreatedAt
			found = true
		}
	}
	return oldest, found, nil
}

// ObservationArtifact turns a gh-injector observation into the domain artifact
// the recording side upserts. The run identity (ConversationID/OrgID/TeamID) is
// stamped downstream by RecordExternalWrite from the caller's RunInfo — never
// from the wire — so a sidecar cannot attribute an artifact to another run. A
// review anchors to runID for its dedup key (the same run-scoped key a TF-side
// draft would use, so a gh submit migrates a draft in place). ok is false for a
// malformed observation (missing coordinates), which is dropped.
//
// Exported because both channel placements need the identical mapping: the
// sidecar relays its observations here through the RelayServer, and a local-mode
// run — whose injector runs in this very process — records them directly with no
// relay hop at all.
func ObservationArtifact(a agentproc.RecordObservationArgs, runID string) (domain.Artifact, bool) {
	if a.Owner == "" || a.Repo == "" {
		return domain.Artifact{}, false
	}
	repoPath := a.Owner + "/" + a.Repo
	switch a.Kind {
	case domain.ArtifactKindPullRequest:
		if a.Number == 0 {
			return domain.Artifact{}, false
		}
		return domain.NewPullRequestArtifact(repoPath, a.Number, a.NodeID, a.Head, a.Base, a.URL, a.Title, a.Body, a.Draft), true
	case domain.ArtifactKindReview:
		if a.Number == 0 || a.ReviewID == 0 {
			return domain.Artifact{}, false
		}
		return domain.NewSubmittedReviewArtifact(repoPath, a.Number, a.ReviewID, a.ReviewState, a.URL, runID), true
	}
	return domain.Artifact{}, false
}

// dispatchCoreNotify serves the "core" fire-and-forget audit ops.

func (s *RelayServer) dispatchCoreNotify(ctx context.Context, op string, args json.RawMessage) {
	switch op {
	case agentproc.OpRecordDenial:
		var a agentproc.RecordDenialArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed git denial failed", "error", err)
			return
		}
		if s.git != nil && s.git.RecordDenial != nil {
			s.git.RecordDenial(ctx, gitproxy.DeniedGitOp{
				Owner:  a.Owner,
				Repo:   a.Repo,
				Ref:    a.Ref,
				Op:     a.Op,
				Reason: a.Reason,
			})
		}

	case agentproc.OpRecordEgressDenial:
		var a agentproc.RecordEgressDenialArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed egress denial failed", "error", err)
			return
		}
		// Unconditional, unlike the git denial above: every sandbox has an egress
		// proxy, so there is no run shape where this op should be dropped. The
		// conversation comes from THIS server's client, never the wire, so a
		// sidecar cannot file a probe against another run — which also makes the
		// dedup key unforgeable.
		recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
		defer cancel()
		s.audit.RecordEgressDenied(recCtx, a.Target, a.Reason)

	case agentproc.OpRecordGHWrite:
		var a agentproc.RecordGHWriteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed gh write failed", "error", err)
			return
		}
		// The request already transited; recording it is best-effort and capped
		// like the other audit ops. Classification happens here rather than in
		// the sidecar, so the wire carries only what was observed and the row's
		// vocabulary stays on the side that owns it.
		recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
		defer cancel()
		obs := ghwrite.Observation{
			Method:         a.Method,
			Path:           a.Path,
			Status:         a.Status,
			ExternalID:     a.ExternalID,
			URL:            a.URL,
			Errored:        a.Errored,
			ResponseUnread: a.ResponseUnread,
		}
		if a.GraphQL != nil {
			obs.GraphQL = &ghwrite.GraphQLFacts{
				Operation:  a.GraphQL.Operation,
				Fields:     a.GraphQL.Mutations,
				Subject:    a.GraphQL.Subject,
				Unreadable: a.GraphQL.Unreadable,
			}
		}
		s.audit.RecordGHChannelWrite(recCtx, obs)

	case agentproc.OpRecordPush:
		var a agentproc.RecordPushArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed push failed", "error", err)
			return
		}
		if s.git != nil && s.git.RecordPush != nil {
			// The relayed push already completed upstream; the DB write here must
			// not run unbounded on the shared control channel, so cap it at the
			// same window the in-process backstop uses.
			recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
			defer cancel()
			s.git.RecordPush(recCtx, gitproxy.PushedRef{
				Repo:    a.Repo,
				Ref:     a.Ref,
				NewSHA:  a.NewSHA,
				Created: a.Created,
				Status:  a.Status,
			})
		}

	case opRecordExternalWrite:
		var a recordExternalWriteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed external write failed", "error", err)
			return
		}
		// The write already landed upstream; Record is best-effort and self-bounds
		// its own recording. Cap it so a wedged store can't pin the frame goroutine.
		recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
		defer cancel()
		s.rt.Record(recCtx, a.Artifact, a.Action)

	case agentproc.OpRecordObservation:
		var a agentproc.RecordObservationArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed gh observation failed", "error", err)
			return
		}
		art, ok := ObservationArtifact(a, s.info.RunID)
		if !ok {
			return
		}
		// The gh mutation already landed upstream; Record stamps the run identity
		// from THIS server's RunInfo (never the wire) and upserts. Best-effort,
		// capped like the other audit ops.
		recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
		defer cancel()
		s.rt.Record(recCtx, &art, nil)

	case opRecordReadTouch:
		var a recordReadTouchArgs
		if err := json.Unmarshal(args, &a); err != nil {
			agenthostLog.Warn("decode relayed read touch failed", "error", err)
			return
		}
		// The read already returned; RecordReadTouch is best-effort. Cap it like
		// the external-write path so a wedged store can't pin the frame goroutine.
		recCtx, cancel := context.WithTimeout(ctx, recordPushRelayTimeout)
		defer cancel()
		s.rt.RecordReadTouch(recCtx, a.Provider, a.Target, a.URL)

	default:
		agenthostLog.Warn("unsupported core relay notify op", "op", op)
	}
}
