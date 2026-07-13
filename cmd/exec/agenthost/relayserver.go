package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
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

	// git is the run's push authz + audit gate (the delegate's executorGitGate).
	// nil for a run with no git surface (a Jira-only run) — authorize_repo then
	// fails closed (deny), the audit ops no-op.
	git *agentproc.GitProxyConfig
}

// NewRelayServer builds the run's relay op server. git may be nil (no git
// surface); stores/info are the run's own, admin-pool + RunInfo-bound.
func NewRelayServer(stores db.Stores, info RunInfo, git *agentproc.GitProxyConfig) *RelayServer {
	return &RelayServer{rt: newDirectRuntime(stores, info), stores: stores, info: info, git: git}
}

// recordPushRelayTimeout bounds the orchestrator-side artifact write kicked off
// by a relayed record_push, mirroring the in-process backstop's own cap
// (gitproxy.recordPushTimeout) so a wedged store can't hold the supervision
// channel's per-frame goroutine open indefinitely.
const recordPushRelayTimeout = 30 * time.Second

// DispatchCall serves a request/response relay op. See agentproc.RelayDispatcher.
func (s *RelayServer) DispatchCall(ctx context.Context, namespace, op string, args json.RawMessage) (json.RawMessage, error) {
	if namespace == agentproc.RelayNamespaceCore {
		return s.dispatchCoreCall(ctx, op, args)
	}
	// A provider policy op (e.g. slack/authorize_channel): served against the
	// run's stores + RunInfo, identity bound orchestrator-side.
	return dispatchProviderOp(ctx, s.stores, s.info, namespace, op, args)
}

// DispatchNotify serves a fire-and-forget audit relay op best-effort.
func (s *RelayServer) DispatchNotify(ctx context.Context, namespace, op string, args json.RawMessage) {
	if namespace == agentproc.RelayNamespaceCore {
		s.dispatchCoreNotify(ctx, op, args)
		return
	}
	if _, err := dispatchProviderOp(ctx, s.stores, s.info, namespace, op, args); err != nil {
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
		return json.Marshal(agentproc.AuthorizeRepoReply{Allowed: dec.Allowed, AllowedRefs: dec.AllowedRefs})

	case opGetAgentRun:
		run, err := s.rt.GetAgentRun(ctx)
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

	case opBuildAgentRunFooter:
		var a buildAgentRunFooterArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		footer, err := s.rt.AgentRunFooter(ctx, a.Kind)
		if err != nil {
			return nil, err
		}
		return json.Marshal(buildAgentRunFooterResult{Footer: footer})

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

	default:
		return nil, fmt.Errorf("agenthost: unsupported core relay call op %q", op)
	}
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

	default:
		agenthostLog.Warn("unsupported core relay notify op", "op", op)
	}
}
