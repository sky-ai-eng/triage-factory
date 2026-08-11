package agenthost

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
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
	act.ConversationID = info.RunID
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
// already-applied action. Swallowed, but counted: this is the last stage of
// the audit funnel auditstats.go measures, and the only one a local-mode
// deployment (no sidecar, no relay) can reach.
func RecordExternalWrite(ctx context.Context, stores db.Stores, info RunInfo, a *domain.Artifact, act *domain.ExternalAction) {
	if a != nil {
		if stores.Artifacts == nil {
			return
		}
		a.ConversationID = info.RunID
		a.OrgID = info.OrgID
		a.TeamID = info.TeamID
	} else if act == nil || stores.ExternalActions == nil {
		return
	}
	if act != nil {
		stampActionIdentityInfo(act, info)
	}

	// The artifact write itself, under the relay op that carried it (a
	// sandboxed run) or under whatever ambient span exists. A CHILD rather
	// than a linked root: unlike the ops above it, this never runs on its own
	// — it is always inside a caller this trace already covers.
	//
	// The whole point is the swallow below. Every write here is best-effort
	// by contract, so a failure would otherwise surface only as a warn line;
	// the span makes it a trace with an error status and the drop counter
	// beside it makes it a number, which is how "the PR was opened but TF has
	// no artifact for it" stops being invisible and starts being measurable.
	// The artifact kind is a closed domain vocabulary; its target is a repo
	// path, and stays out.
	ctx, span := tracer.Start(ctx, "artifact.record",
		trace.WithAttributes(telemetry.ConversationID(info.RunID), telemetry.OrgID(info.OrgID)))
	defer span.End()
	if a != nil {
		span.SetAttributes(telemetry.Op(a.Kind))
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
		recordSpanError(span, err)
		recordAuditDrop(ctx, dropStageRecord, recordDropOp(a, act))
		agenthostLog.Warn("external write recording failed (action already applied)",
			"run", info.RunID, "kind", kind, "target", target, "error", err)
	}
	// Resolve the touched entity outside the audit write (TFAC-513 §2).
	recordTouchInfo(ctx, stores, info, act)
	// Same placement, same reason: an admin-pool write, so it must run after
	// withWriteInfo's tx has settled.
	stampPROwnership(ctx, stores, info, a)
}

// --- touched-entity resolution (TFAC-513 §2) ---
//
// Every org-credential write the bot makes lands on an external object — a PR,
// a Jira issue, a Slack thread. resolveTouchedEntityInfo turns that object's
// (provider, target) into an entities row, returning an existing entity or a
// freshly-minted snapshot-less stub.

// resolveTouchedEntityInfo maps a (provider, target, url) triple to an entities
// row, returning an existing entity or a freshly-minted snapshot-less stub, and
// "" for anything the touched-entity rule skips (a repo-level GitHub target with
// no '#N', an empty key, an unmapped provider). It is the free-function
// counterpart of the former LocalClient.resolveTouchedEntity; kept here so the
// write funnel and the read path both reach it without a LocalClient.
//
// The Slack case mints a "message" entity keyed on target — expected to already
// be domain.SlackSourceID(channel, rootTS), the same key the ingest pipeline
// uses (ee/slack/ingest.go), so a bot-authored write on a thread
// resolves/creates the identical entity a human mention would.
func resolveTouchedEntityInfo(ctx context.Context, stores db.Stores, info RunInfo, provider, target, url string) (string, error) {
	if stores.Entities == nil {
		return "", nil
	}
	source, sourceID, kind, ok := domain.EntityRefForExternal(provider, target)
	if !ok {
		return "", nil
	}
	// title is left empty — neither an ExternalAction nor an addressed read
	// carries a human title, and the poll cycle (or, for Slack, the ingest
	// pipeline) seeds it from context. url rides through when present.
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, source, sourceID, kind, "", url)
	if err != nil {
		return "", err
	}
	return entity.ID, nil
}

// recordEntityTouch resolves-or-creates the touched entity for (provider,
// target, url) and, when it maps to a real entity, persists a durable
// (run_id, entity_id, role='touched') row. Shared by the write funnel
// (recordTouchInfo) and the addressed-read path (Runtime.RecordReadTouch).
//
// Best-effort throughout: a resolve or record failure is logged and swallowed
// so it never fails the agent's already-applied write or the read that
// triggered it — the entity self-heals on a later touch or poll, and the touch
// row on the next addressed hit. It MUST run outside any audit tx: local mode
// holds the single SQLite connection for the life of a synthetic-claims tx, so
// these ...System (admin-pool) writes inside that closure would deadlock. Every
// funnel caller invokes it only after withWriteInfo returns; the read path
// carries no tx.
func recordEntityTouch(ctx context.Context, stores db.Stores, info RunInfo, provider, target, url string) {
	id, err := resolveTouchedEntityInfo(ctx, stores, info, provider, target, url)
	if err != nil {
		agenthostLog.Warn("touched-entity resolve failed (will retry on next poll)",
			"run", info.RunID, "target", target, "error", err)
		return
	}
	if id == "" || stores.TaskMemory == nil {
		return
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, info.OrgID, info.RunID, id, domain.MemoryRoleTouched); err != nil {
		agenthostLog.Warn("touched-entity record failed", "run", info.RunID, "entity", id, "error", err)
	}
}

// recordTouchInfo persists the write funnel's touched entity: it unwraps the
// external action into its (provider, target, url) and records the run→entity
// touch. A nil action (an audit-only write with no external action) touches
// nothing. See recordEntityTouch for the best-effort + outside-the-tx contract.
func recordTouchInfo(ctx context.Context, stores db.Stores, info RunInfo, act *domain.ExternalAction) {
	if act == nil {
		return
	}
	recordEntityTouch(ctx, stores, info, act.Provider, act.Target, act.URL)
}

// --- owning-team stamp for a bot-opened PR ---

// stampPROwnership records the commissioning team on the entity behind a PR the
// bot just opened, so the router can route that PR's later events to the team
// whose work produced it.
//
// The gap it fills: routing resolves an owner from the entity's author, and the
// author of a PR TF opened is a bot that maps to no TF user. Nothing else on the
// PR carries the answer either — the run that opened it does, and this is the
// only moment the two are in the same place. Left unstamped, the PR's first
// ci_check_failed finds no owner and mints a task for nobody unless the repo
// happens to be project-attached.
//
// Scoped to kind=pull_request deliberately. A submitted-review artifact shares
// the owner/repo#N target shape, and reviewing someone else's PR is the clearest
// case of touching an entity you must not come to own.
//
// Ordering against the poller does not matter. FindOrCreate resolves the same
// natural key the tracker mints on (both go through domain.PullRequestTarget's
// owner/repo#N), so this either back-fills the row the poller already created or
// creates the stub the poller later enriches; the stamp itself is if-NULL, so
// neither ordering can overwrite an owner set deliberately elsewhere.
//
// Best-effort, like everything else downstream of an already-applied write: the
// PR is open on GitHub regardless, and a failure here costs routing precision on
// that PR, never the run. It is not retried — the entity is durable and the
// event that would need the owner has not arrived yet, but nothing re-derives
// the stamp later either, so a failure means that PR's events fall through to
// the router's remaining tiers for good.
func stampPROwnership(ctx context.Context, stores db.Stores, info RunInfo, a *domain.Artifact) {
	if a == nil || a.Provider != domain.ArtifactProviderGitHub || a.Kind != domain.ArtifactKindPullRequest {
		return
	}
	// Defensive: auto-fire is gated on an owned task and artifacts.team_id is
	// NOT NULL, so a run without a team should not reach here. If one does,
	// there is no owner to record and the entity keeps its NULL.
	if info.TeamID == "" || stores.Entities == nil {
		return
	}
	entityID, err := resolveTouchedEntityInfo(ctx, stores, info, a.Provider, a.Target, a.URL)
	if err != nil {
		agenthostLog.Warn("owning-team stamp skipped: entity resolve failed",
			"run", info.RunID, "target", a.Target, "error", err)
		return
	}
	if entityID == "" {
		return
	}
	stamped, err := stores.Entities.StampOwningTeamIfUnsetSystem(ctx, info.OrgID, entityID, info.TeamID)
	if err != nil {
		agenthostLog.Warn("owning-team stamp failed",
			"run", info.RunID, "entity", entityID, "target", a.Target, "error", err)
		return
	}
	if stamped {
		agenthostLog.Info("stamped owning team on bot-opened PR",
			"run", info.RunID, "entity", entityID, "target", a.Target, "team", info.TeamID)
	}
}

// loadEntityMemory is the host side of `exec memory load`: it looks up the
// entity for (source, sourceID) by its natural key — LOOKUP ONLY, never
// FindOrCreate, so a load of something unknown is a miss, not a stub mint — and
// on a hit returns that entity's prior run memory scoped to the run's team,
// plus records a best-effort run→entity 'touched' row (loading IS an address).
//
// Unlike recordEntityTouch's resolve-or-create, a miss here records NOTHING and
// returns an empty result with an empty EntityID — the deliberate asymmetry the
// ticket calls out: an addressed WRITE/read on a live object legitimately mints
// a stub the poll cycle enriches, but reading another entity's memory must not
// conjure the entity into existence.
//
// Content is composed by the store read (agent narrative + the
// "## Human feedback (post-run)" separator), the same materialization the
// spawn-time materializer emits — so the on-demand pull reads identically to
// the auto-staged files. Count is the pre-limit scoped total (its dedicated
// count method); Memories is the most recent `limit`, capped IN the query (not
// fetched-all-then-sliced) so a hot entity's long history isn't transferred to
// keep only the tail. The touch is best-effort — a read never fails on its
// touch.
func loadEntityMemory(ctx context.Context, stores db.Stores, info RunInfo, source, sourceID string, limit int) (*MemoryLoadResult, error) {
	res := &MemoryLoadResult{Source: source, SourceID: sourceID, Memories: []MemoryLoadEntry{}}
	if stores.Entities == nil {
		return res, nil
	}
	entity, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, source, sourceID)
	if err != nil {
		return nil, err
	}
	if entity == nil {
		// Miss: unknown entity. No touch, no entity minted.
		return res, nil
	}
	res.EntityID = entity.ID
	res.Title = entity.Title

	if stores.TaskMemory != nil {
		count, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, info.OrgID, entity.ID, info.TeamID)
		if err != nil {
			return nil, err
		}
		res.Count = count

		// A non-positive limit — a direct Client caller bypassing the CLI's
		// positive-int guard — means "the default", never unbounded (which the
		// prior fetch-all-then-slice quietly did) and never a negative SQL LIMIT.
		if limit <= 0 {
			limit = defaultMemoryLoadLimit
		}
		mems, err := stores.TaskMemory.GetRecentMemoriesForEntitySystem(ctx, info.OrgID, entity.ID, info.TeamID, limit)
		if err != nil {
			return nil, err
		}
		for _, m := range mems {
			res.Memories = append(res.Memories, MemoryLoadEntry{
				RunID:          m.RunID,
				BlueprintRunID: m.BlueprintRunID,
				CreatedAt:      m.CreatedAt,
				Content:        m.Content,
			})
		}

		// Record the touch strictly after the reads (loading is an address).
		// Best-effort: a load never fails on its touch — the row self-heals on the
		// next addressed hit, and on the sidecar a dropped relay costs one row.
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, info.OrgID, info.RunID, entity.ID, domain.MemoryRoleTouched); err != nil {
			agenthostLog.Warn("memory-load touch record failed", "run", info.RunID, "entity", entity.ID, "error", err)
		}
	}
	return res, nil
}

// defaultMemoryLoadLimit is the cap loadEntityMemory applies when a caller
// passes a non-positive limit (the CLI never does — it defaults to and enforces
// a positive value — so this only guards direct Client / IPC / relay callers).
// Mirrors the CLI's default so both surfaces agree on "20 most recent".
const defaultMemoryLoadLimit = 20
