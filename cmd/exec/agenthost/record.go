package agenthost

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// This file is the shared recording funnel behind every exec choke-point
// write — gh, jira, and ee-registered extension verbs (e.g. Slack, TFAC-596)
// alike. It used to live as unexported *LocalClient methods; those methods
// now delegate here so an ee package (which can't reach LocalClient's
// internals) gets the identical stamp/write/touch discipline via
// RecordExternalWrite. Behavior for gh/jira is unchanged — only the call
// site moved.

// withWriteInfo picks the per-run routing strategy off info directly (the
// free-function counterpart of LocalClient.withWrite, for callers — like an
// ee extension handler — that hold a RunInfo but not a LocalClient).
func withWriteInfo(
	ctx context.Context,
	stores db.Stores,
	info RunInfo,
	system func() error,
	user func(ts db.TxStores) error,
) error {
	if info.IsEventTriggered {
		return system()
	}
	return stores.Tx.SyntheticClaimsWithTx(ctx, info.OrgID, info.UserID, user)
}

// stampActionIdentityInfo fills the run/org/team/actor common to every
// bot-attributed action from info. The action-specific fields (provider,
// action, target, credential, from/to, dedup) are set by the caller.
func stampActionIdentityInfo(act *domain.ExternalAction, info RunInfo) {
	act.OrgID = info.OrgID
	act.TeamID = info.TeamID
	act.RunID = info.RunID
	act.ActorUserID = info.UserID // empty for event-triggered → SQL NULL
}

// recordActionSystemInfo appends act on the admin pool (event-triggered
// runs). A nil act — or a partial test wiring with no ExternalActions store —
// is a no-op.
func recordActionSystemInfo(ctx context.Context, stores db.Stores, info RunInfo, act *domain.ExternalAction) error {
	if act == nil || stores.ExternalActions == nil {
		return nil
	}
	return stores.ExternalActions.RecordSystem(ctx, info.OrgID, *act)
}

// recordActionTx appends act inside the caller's synthetic-claims tx (manual
// runs), composing with the artifact upsert. A nil act is a no-op.
func recordActionTx(ctx context.Context, ts db.TxStores, orgID string, act *domain.ExternalAction) error {
	if act == nil || ts.ExternalActions == nil {
		return nil
	}
	return ts.ExternalActions.Record(ctx, orgID, *act)
}

// RecordExternalWrite is the shared recording funnel behind every exec
// choke-point write: it stamps the run's identity onto act (if present),
// upserts a (if non-nil) and appends act (if non-nil) in the SAME write —
// admin pool for event-triggered runs, a synthetic-claims tx for manual ones
// — logs (never fails) a write error, and resolves the touched entity
// strictly after the write settles (see recordTouchInfo's ordering doc).
//
// a is nil for an audit-only write that produces no artifact to compose
// with (a Slack reaction, a review-thread reply) — mirrors the former
// recordBotAction. act is nil when the write carries no distinct external
// action (a review-draft snapshot re-upserting its artifact with no new
// external write) — mirrors the former upsertGithubArtifact/upsertJiraArtifact
// with act == nil. The caller is responsible for setting a.Provider (and any
// other polymorphic fields) before calling — this function only stamps the
// run-identity fields common to every provider.
//
// Best-effort throughout: every write here already happened against the
// external system (GitHub/Jira/Slack) before this is called, so a recording
// failure is logged and swallowed — it must never unwind the agent's
// already-applied action.
func RecordExternalWrite(ctx context.Context, stores db.Stores, info RunInfo, a *domain.Artifact, act *domain.ExternalAction) {
	if a != nil {
		if stores.Artifacts == nil {
			return
		}
		a.RunID = info.RunID
		a.OrgID = info.OrgID
		a.TeamID = info.TeamID
	} else if act == nil || stores.ExternalActions == nil {
		return
	}
	if act != nil {
		stampActionIdentityInfo(act, info)
	}

	err := withWriteInfo(ctx, stores, info,
		func() error {
			if a != nil {
				if _, e := stores.Artifacts.UpsertSystem(ctx, info.OrgID, *a); e != nil {
					return e
				}
			}
			return recordActionSystemInfo(ctx, stores, info, act)
		},
		func(ts db.TxStores) error {
			if a != nil {
				if _, e := ts.Artifacts.Upsert(ctx, info.OrgID, *a); e != nil {
					return e
				}
			}
			return recordActionTx(ctx, ts, info.OrgID, act)
		},
	)
	if err != nil {
		target := ""
		kind := ""
		if a != nil {
			target, kind = a.Target, a.Kind
		} else if act != nil {
			target = act.Target
		}
		agenthostLog.Warn("external write recording failed (action already applied)",
			"run", info.RunID, "kind", kind, "target", target, "error", err)
	}
	// Resolve the touched entity outside the audit write (TFAC-513 §2).
	recordTouchInfo(ctx, stores, info, act)
}

// --- touched-entity resolution (TFAC-513 §2) ---
//
// Every org-credential write the bot makes lands on an external object — a PR,
// a Jira issue, a Slack thread. resolveTouchedEntityInfo turns that object's
// (provider, target) into an entities row, returning an existing entity or a
// freshly-minted snapshot-less stub.

// resolveTouchedEntityInfo is the free-function counterpart of
// LocalClient.resolveTouchedEntity. See that method's doc for the full
// rationale; kept here so RecordExternalWrite (and any ee caller reaching
// this seam) can use it without a LocalClient.
//
// The Slack case mints a "message" entity keyed on act.Target — expected to
// already be domain.SlackSourceID(channel, rootTS), the same key the ingest
// pipeline uses (ee/slack/ingest.go), so a bot-authored write on a thread
// resolves/creates the identical entity a human mention would.
func resolveTouchedEntityInfo(ctx context.Context, stores db.Stores, info RunInfo, act *domain.ExternalAction) (string, error) {
	if act == nil || stores.Entities == nil {
		return "", nil
	}
	var kind string
	switch act.Provider {
	case domain.ArtifactProviderGitHub:
		// owner/repo#N is a PR entity; bare owner/repo is a repo-level action.
		// NOTE: this assumes every github target is a PR (kind="pr"). Exec only
		// writes PRs/reviews today, so that holds — but a GitHub *issue* shares
		// the "owner/repo#N" shape, and the poller's resolveStubNodeID would then
		// 404 against /pulls/{n} every cycle. GitHub issue support must branch on
		// kind here (and give the poller an issue-aware enrichment path).
		if _, _, _, ok := domain.ParsePRTarget(act.Target); !ok {
			return "", nil
		}
		kind = "pr"
	case domain.ArtifactProviderJira:
		if act.Target == "" {
			return "", nil
		}
		kind = "issue"
	case domain.ArtifactProviderSlack:
		if act.Target == "" {
			return "", nil
		}
		kind = "message"
	default:
		return "", nil
	}
	// title is left empty — ExternalAction carries no human title, and the poll
	// cycle (or, for Slack, the ingest pipeline) seeds it from context. url
	// rides through when present.
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, act.Provider, act.Target, kind, "", act.URL)
	if err != nil {
		return "", err
	}
	return entity.ID, nil
}

// recordTouchInfo resolves-or-creates the touched entity as a best-effort
// side step in the recording funnels: a failure is logged and swallowed so it
// never fails the agent's already-applied write (the entity self-heals on a
// later touch or poll). It MUST run outside the audit tx — local mode holds
// the single SQLite connection for the life of a synthetic-claims tx, so a
// System write inside that closure would deadlock; every caller invokes this
// only after withWriteInfo returns.
func recordTouchInfo(ctx context.Context, stores db.Stores, info RunInfo, act *domain.ExternalAction) {
	if act == nil {
		return
	}
	id, err := resolveTouchedEntityInfo(ctx, stores, info, act)
	if err != nil {
		agenthostLog.Warn("touched-entity resolve failed (will retry on next poll)",
			"run", info.RunID, "target", act.Target, "error", err)
		return
	}
	if id != "" {
		agenthostLog.Debug("resolved touched entity", "run", info.RunID, "entity", id, "target", act.Target)
	}
}
