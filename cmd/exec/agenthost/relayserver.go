package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"go.opentelemetry.io/otel/trace"
)

// RelayServer is the orchestrator-side op server the sidecar's relay envelope
// dispatches to (it implements agentproc.RelayDispatcher). It holds the run's
// db.Stores + ConversationInfo + git gate — everything the capless sidecar cannot — and
// binds identity from ConversationInfo on every op, so a relayed op is structurally
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
	info   ConversationInfo

	// audit is the host-side client the relayed audit-only ops record through —
	// the same seam the in-process git gate uses (executorGitGate's
	// auditHost), so a sandbox's denial and a local run's land the identical
	// row through the identical method.
	audit *LocalClient

	// git is the run's push authz + audit gate (the delegate's executorGitGate).
	// nil for a run with no git surface (a Jira-only run) — authorize_repo then
	// fails closed (deny), the audit ops no-op.
	git *agentproc.GitProxyConfig

	// proxyCreds are the run's per-run proxy coordinates (REST + git proxy, with
	// placeholder tokens), set once the sidecar bring-up returns them (SetProxyCreds).
	// The workspace materialization served here clones + fetches PRs through them, so the
	// orchestrator holds no real credential for either. nil until bring-up completes.
	proxyCreds *ProxyCredentials

	// credRefresh is the sealed-bundle freshness gate the workspace
	// materialization waits on. Optional: only the delegated executor path
	// wires it (SetCredentialRefresh) — local mode has no sealed bundle at all,
	// so it leaves credRefresh nil and the wait no-ops.
	credRefresh *CredentialRefresh

	// engagement is the trace context of the engagement that built this
	// server, captured once at bring-up (SetEngagementSpanContext). It is how
	// a relayed op — which arrives long after the setup span ended, over a
	// wire that deliberately carries no trace context — still names the run it
	// belongs to. The zero value (local mode, an untraced process) simply
	// produces unlinked spans.
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

// SetGitHubCredential records which GitHub credential this run's writes act
// under, so every audit row served here — the relayed gh-channel writes and
// refusals, the git denials, the branch push composed into an artifact upsert —
// names the credential that made them.
//
// Injected late for the same reason SetProxyCreds is, and from the same place:
// the classification lives in the sealed bundle, which opens only in the
// sidecar, so it arrives with the bring-up result. Bring-up completes before
// the agent runs, so it is always settled before a relayed write can arrive.
func (s *RelayServer) SetGitHubCredential(credential string) {
	if credential == "" {
		return
	}
	s.rt.ghCredential = credential
	s.audit.SetGitHubCredential(credential)
}

// NewRelayServer builds the run's relay op server. git may be nil (no git
// surface); stores/info are the run's own, admin-pool + ConversationInfo-bound.
func NewRelayServer(stores db.Stores, info ConversationInfo, git *agentproc.GitProxyConfig) *RelayServer {
	return &RelayServer{
		rt:     newDirectRuntime(stores, info),
		stores: stores,
		info:   info,
		git:    git,
		audit:  NewLocal(stores, info),
	}
}

// relayWriteTimeout bounds a relayed record's whole application
// (DispatchRecord): the record lane serves one record at a time and waits on
// each. The same window as the in-process push backstop's cap
// (gitproxy.recordPushTimeout). It reaches what honours its context; the
// recording funnel writes on a budget of its own (recordWriteBudget).
const relayWriteTimeout = 30 * time.Second

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
	// run's stores + ConversationInfo, identity bound orchestrator-side.
	return dispatchProviderOp(ctx, s.stores, s.info, namespace, op, args)
}

// errRecordDeferred is DispatchRecord's answer for a record one of whose
// writes failed on a retryable error past the write stage's own retries. It
// tells the sidecar the record was not applied, and to send it again.
var errRecordDeferred = errors.New("agenthost: audit record not applied; a write failed on a retryable error")

// errRecordPanicked marks the span of a record whose application panicked.
var errRecordPanicked = errors.New("agenthost: applying an audit record panicked")

// DispatchRecord applies one audit record the sidecar relayed, and is the
// single place a relayed record is accounted for, the way DispatchCall is the
// single instrumented point for the request/response half. See
// agentproc.RelayDispatcher for what its answer tells the sidecar.
//
// A record can be applied more than once — the sidecar resends one it has not
// seen acknowledged — and key is what makes that safe: it is the same on every
// resend, and the record's ledger row takes its dedup key from it
// (mintActionDedupKey), so a second application conflicts with the first
// instead of appending a second row.
//
// Three outcomes. A record that never becomes a write — undecodable, an op
// with no case here, a provider handler that errored — is counted at its relay
// stage and acknowledged, since sending it again would fail the same way. A
// record one of whose writes failed on a retryable error past the write
// stage's own retries is deferred: nothing is counted, and the sidecar is told
// to send it again. Anything else is acknowledged, after counting whatever the
// write stage lost for good (relayedRecord.countHeld). So every record is
// counted at most once, at the stage that finally lost it.
//
// The two relay stages differ in whose fault the failure is: the core half can
// only fail before a handler runs (an undecodable payload, an op this binary
// has no case for), while a provider op fails inside a handler that did run.
// A handler that panics is the dispatch stage too, and is acknowledged like
// any other failure that would repeat: answered with an error, the record
// would be resent and panic again, holding every record queued behind it for
// the rest of the run.
//
// Every application is capped at relayWriteTimeout, for the same reason:
// the record lane serves one record at a time, so one that never finished
// would hold every record behind it. The cap reaches only what honours its
// context — the write funnel runs on a budget of its own (recordContext), and
// nothing can stop a handler that ignores both.
func (s *RelayServer) DispatchRecord(ctx context.Context, namespace, op, key string, args json.RawMessage) (answer error) {
	ctx, span := s.startRelayOp(ctx, "relay.record", namespace, op)
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			recordSpanError(span, errRecordPanicked)
			recordAuditDrop(ctx, dropStageRelayDispatch, relayDropOp(namespace, op))
			agenthostLog.Error("applying a relayed audit record panicked; record dropped",
				"namespace", namespace, "op", op, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			answer = nil
		}
	}()

	rec := &relayedRecord{key: key}
	var err error
	stage := dropStageRelayDispatch
	switch {
	case uuid.Validate(key) != nil:
		// The key is the record's idempotency, so a record without a usable
		// one cannot be applied safely; only a broken sidecar sends one.
		stage = dropStageRelayDecode
		err = fmt.Errorf("agenthost: relayed record %s/%s carries no valid key", namespace, op)
	case namespace == agentproc.RelayNamespaceCore:
		stage = dropStageRelayDecode
		err = s.dispatchCoreRecord(withRelayedRecord(ctx, rec), op, args)
	default:
		_, err = dispatchProviderOp(withRelayedRecord(ctx, rec), s.stores, s.info, namespace, op, args)
	}
	if err != nil {
		recordSpanError(span, err)
		recordAuditDrop(ctx, stage, relayDropOp(namespace, op))
		agenthostLog.Warn("relayed audit record failed; record dropped",
			"namespace", namespace, "op", op, "error", err)
		return nil
	}
	if rec.deferred {
		recordSpanError(span, errRecordDeferred)
		agenthostLog.Warn("relayed audit record deferred; the sidecar will send it again",
			"namespace", namespace, "op", op, "conversation", s.info.ConversationID)
		return errRecordDeferred
	}
	rec.countHeld(ctx)
	return nil
}

// CountUndeliveredRecords counts the audit records a run's sidecar gave up on,
// as its end-of-run drain reported them (agentproc.DrainRecords), at the send
// stage: none of them was ever applied here. Both the names and the counts
// came off the wire from the one process deliberately exposed to hostile
// text, so the names are clamped like every relay-stage label and a count
// that is not positive is ignored.
func CountUndeliveredRecords(ctx context.Context, conversationID string, undelivered []sidecarproto.UndeliveredRecords) {
	for _, u := range undelivered {
		if u.Count <= 0 {
			continue
		}
		recordAuditDrops(ctx, dropStageRelaySend, relayDropOp(u.Namespace, u.Op), int64(u.Count))
		agenthostLog.Warn("sidecar never delivered audit records",
			"conversation", conversationID, "namespace", u.Namespace, "op", u.Op, "count", u.Count)
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

	case agentproc.OpAuthorizeGHWrite:
		var a agentproc.AuthorizeGHWriteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		return json.Marshal(agentproc.AuthorizeGHWriteReply{Allowed: s.authorizeGHWrite(ctx, a)})

	case opGetConversation:
		conv, err := s.rt.GetConversation(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(getConversationResult{Conversation: conv})

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

	case opRepoSlugByExternalID:
		var a repoSlugByExternalIDArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		slug, err := s.rt.RepoSlugByExternalID(ctx, a.ExternalID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(repoSlugResult{Slug: slug})

	case opTeamTracksRepository:
		var a repositoryIDArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		tracks, err := s.rt.TeamTracksRepository(ctx, a.RepositoryID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(teamTracksRepositoryResult{Tracks: tracks})

	case opGetConversationWorktreeByRepoRef:
		var a conversationWorktreeByRepoRefArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		w, err := s.rt.GetConversationWorktreeByRepoRef(ctx, a.RepositoryID, a.Ref)
		if err != nil {
			return nil, err
		}
		return json.Marshal(conversationWorktreeResult{Worktree: w})

	case opTaskOwnRepository:
		var a repositoryIDArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		isTaskRepo, err := s.rt.TaskOwnRepository(ctx, a.RepositoryID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(taskOwnRepoResult{IsTaskRepo: isTaskRepo})

	case opListConversationWorktrees:
		w, err := s.rt.ListConversationWorktrees(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(conversationWorktreesResult{Worktrees: w})

	case opInsertConversationWorktree:
		var a insertConversationWorktreeArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		inserted, winningPath, err := s.rt.InsertConversationWorktree(ctx, a.Row)
		if err != nil {
			return nil, err
		}
		return json.Marshal(insertConversationWorktreeResult{Inserted: inserted, WinningPath: winningPath})

	case opDeleteConversationWorktree:
		var a deleteConversationWorktreeByRepoRefArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		if err := s.rt.DeleteConversationWorktree(ctx, a.RepositoryID, a.Ref); err != nil {
			return nil, err
		}
		return nil, nil

	case opListConversationArtifacts:
		arts, err := s.rt.ListConversationArtifacts(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(listConversationArtifactsResult{Artifacts: arts})

	case opOrgJiraBase:
		url, err := s.rt.OrgJiraBaseURL(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(orgJiraBaseResult{URL: url})

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
		// Relayed as a CALL, never a record: the sidecar's auto-post path blocks
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
		// ConversationInfo, so a sidecar cannot read another team's posture.
		res, err := s.rt.ReviewPosture(ctx, a.Owner, a.Repo)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)

	case opArtifactPullRequest:
		var a artifactPullRequestArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// The entity and registry rows live behind the stores the sidecar does
		// not hold; the artifact is looked up among the run's own, from the
		// orchestrator's ConversationInfo.
		pr, err := s.rt.ArtifactPullRequest(ctx, a.ArtifactID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(artifactPullRequestResult{
			RepositoryID: pr.RepositoryID,
			Owner:        pr.Owner,
			Repo:         pr.Repo,
			Number:       pr.Number,
			Retired:      pr.Retired,
		})

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

	case opSourceDisabled:
		var a sourceDisabledArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// The orchestrator holds the policy row; the sidecar relays for it on
		// every credentialed GitHub/Jira call so an admin's switch lands on a
		// run that is already in flight.
		off, err := s.rt.SourceDisabled(ctx, a.Kind)
		if err != nil {
			return nil, err
		}
		return json.Marshal(sourceDisabledResult{Disabled: off})

	case opMemoryLoad:
		var a memoryLoadArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		// The sidecar relays `memory load` here so the entity lookup, the team-
		// scoped memory read, and the best-effort touch all land where the stores
		// live. Identity is the run's own ConversationInfo (s.rt), so a sidecar cannot
		// steer the read at another org/team's memory.
		res, err := s.rt.MemoryLoad(ctx, a.Source, a.SourceID, a.ExternalID, a.Limit)
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
		// run's own ConversationInfo, so the repo-config + team-tracking gate re-binds to
		// THIS run's team — a sidecar cannot steer it at another team's repo.
		client := &LocalClient{
			stores:     s.stores,
			info:       s.info,
			rt:         s.rt,
			proxyCreds: s.proxyCreds,
			gateWired:  s.stores.TeamGitHubRepos != nil && s.stores.ConversationWorktrees != nil,
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
// No-ops when unwired (local mode) or when the run has no reservation
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
	reservedAt, ok, err := s.repoReservedAt(ctx, owner, repo)
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
				agenthostLog.Warn("read sealed credential bundle timestamp failed; retrying until the workspace wait expires", "conversation", s.info.ConversationID, "repo", repoID, "error", err)
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

// repoReservedAt reports when owner/repo entered this run's authorized repo
// set — the OLDEST conversation_worktrees row the run holds for it.
//
// Oldest, not newest, because the two ledgers are keyed differently:
// reservations are per (repo, ref) while credentials are per repo (the
// provisioner dedups by repo id). The repo joined the authorized set at its
// first reservation, so a bundle sealed after that covers it — and every later
// ref in the same repo, which adds no grant to wait for. Keying on the newest
// row would make a run's second checkout in a repo it already holds sit out a
// re-seal that changes nothing.
//
// A ledger row names its repository by registry row id, so owner/repo is
// resolved to the row on the org's current GitHub host (GetRepo, which matches
// the name case-insensitively) and the rows are matched on that id, as
// authorizeRepo matches them. A reservation of the same name on another host is
// another repository's and widened nothing for this one.
func (s *RelayServer) repoReservedAt(ctx context.Context, owner, repo string) (time.Time, bool, error) {
	rows, err := s.rt.ListConversationWorktrees(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(rows) == 0 {
		return time.Time{}, false, nil
	}
	row, err := s.rt.GetRepo(ctx, owner+"/"+repo)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("resolve %s/%s: %w", owner, repo, err)
	}
	if row == nil {
		return time.Time{}, false, nil
	}
	var oldest time.Time
	found := false
	for _, w := range rows {
		if w.RepositoryID != row.ID {
			continue
		}
		if !found || w.CreatedAt.Before(oldest) {
			oldest = w.CreatedAt
			found = true
		}
	}
	return oldest, found, nil
}

// authorizeGHWrite decides one gh-channel request the injector recognized as a
// gated shape, and writes the refusal row when the answer is no.
//
// The decision is re-derived here from the shared classifier rather than taken
// from the sidecar: policy belongs on the side that holds it, and the sidecar's
// recognition is a filter on which requests need asking about, not the answer.
// The two readings agree by construction — one table, one binary — so a
// disagreement can only mean this side found the request ungated, which lets it
// through exactly as it would have gone through unnoticed.
//
// V1 has no authorized case: a gated shape is refused whatever the run, the
// mission, or the trigger. The only true return is the ungated one.
func (s *RelayServer) authorizeGHWrite(ctx context.Context, a agentproc.AuthorizeGHWriteArgs) bool {
	req := ghwrite.Request{
		Method:  a.Method,
		Path:    a.Path,
		GraphQL: graphQLFactsFromWire(a.GraphQL),
	}
	ref, gated := ghwrite.Gate(req)
	if !gated {
		return true
	}
	// The agent is waiting on this call, and the recording funnel bounds the
	// row's write at its own budget (recordWriteBudget). A store that cannot be
	// written costs the row; the refusal itself is this function's return value
	// and does not depend on it.
	s.audit.RecordGHWriteDenied(ctx, req, ref)
	return false
}

// graphQLFactsFromWire rebuilds the classifier's facts from the relay payload.
// nil in, nil out — a REST request carries no GraphQL member.
func graphQLFactsFromWire(w *agentproc.GraphQLWriteFacts) *ghwrite.GraphQLFacts {
	if w == nil {
		return nil
	}
	return &ghwrite.GraphQLFacts{
		Operation:   w.Operation,
		Fields:      w.Mutations,
		Subject:     w.Subject,
		Unreadable:  w.Unreadable,
		NamesReview: w.NamesReview,
	}
}

// dispatchCoreRecord applies the "core" audit records.
//
// Its error return is narrow on purpose: every op below either records
// something or is void by contract, so the only way this returns non-nil is
// that the payload never became a record at all — an undecodable frame or an op
// with no case here. That is what makes DispatchRecord's decode stage a real
// stage rather than a catch-all. A write that fails reports itself through the
// relayed record on ctx instead (recordWriteDrop).
//
// No op here sets a timeout of its own. ctx carries DispatchRecord's cap
// (relayWriteTimeout), which bounds everything that runs on it — the push's
// artifact upsert, the read touch, the gh write's lookups — and the recording
// funnel the ops end in writes on its own detached budget (recordWriteBudget).
func (s *RelayServer) dispatchCoreRecord(ctx context.Context, op string, args json.RawMessage) error {
	switch op {
	case agentproc.OpRecordDenial:
		var a agentproc.RecordDenialArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("agenthost: decode relayed git denial: %w", err)
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
			return fmt.Errorf("agenthost: decode relayed egress denial: %w", err)
		}
		// Unconditional, unlike the git denial above: every sandbox has an egress
		// proxy, so there is no run shape where this op should be dropped. The
		// conversation comes from THIS server's client, never the wire, so a
		// sidecar cannot file a probe against another run — which also makes the
		// dedup key unforgeable.
		s.audit.RecordEgressDenied(ctx, a.Target, a.Reason)

	case agentproc.OpRecordGHWrite:
		var a agentproc.RecordGHWriteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("agenthost: decode relayed gh write: %w", err)
		}
		// The request already transited; recording it is best-effort.
		// Classification happens here rather than in the sidecar, so the wire
		// carries only what was observed and the row's vocabulary stays on the
		// side that owns it.
		s.audit.RecordGHChannelWrite(ctx, ghwrite.Observation{
			Method:         a.Method,
			Path:           a.Path,
			Status:         a.Status,
			ExternalID:     a.ExternalID,
			NodeID:         a.NodeID,
			URL:            a.URL,
			Errored:        a.Errored,
			ResponseUnread: a.ResponseUnread,
			GraphQL:        graphQLFactsFromWire(a.GraphQL),
		})

	case agentproc.OpRecordPush:
		var a agentproc.RecordPushArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("agenthost: decode relayed push: %w", err)
		}
		if s.git != nil && s.git.RecordPush != nil {
			s.git.RecordPush(ctx, gitproxy.PushedRef{
				Repo:          a.Repo,
				Ref:           a.Ref,
				NewSHA:        a.NewSHA,
				Created:       a.Created,
				Status:        a.Status,
				Rejected:      a.Rejected,
				Reason:        a.Reason,
				RemoteMessage: a.RemoteMessage,
			})
		}

	case opRecordExternalWrite:
		var a recordExternalWriteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("agenthost: decode relayed external write: %w", err)
		}
		// The ledger key is this side's to choose, like the identity and scope
		// the funnel stamps over whatever the wire said: it is derived from the
		// record's key under this run's conversation (mintActionDedupKey). A key
		// the sidecar chose could equal another run's deterministic one — a
		// branch push's, an egress denial's — and the row it wrote first would
		// suppress that run's real one. No action the sidecar builds carries a
		// key of its own; the ops whose rows have one build them here.
		if a.Action != nil {
			a.Action.DedupKey = ""
		}
		s.rt.Record(ctx, a.Artifact, a.Action, a.EntityContainer)

	case opRecordReadTouch:
		var a recordReadTouchArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Errorf("agenthost: decode relayed read touch: %w", err)
		}
		s.rt.RecordReadTouch(ctx, a.Provider, a.Target, a.ExternalID, a.EntityContainer, a.URL)

	default:
		return fmt.Errorf("agenthost: unsupported core relay record op %q", op)
	}
	return nil
}
