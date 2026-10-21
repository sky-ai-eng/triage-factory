package agenthost

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entityscope"
	"github.com/sky-ai-eng/triage-factory/internal/memoryentities"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// This file is the shared recording funnel behind every exec choke-point
// write — gh, jira, and ee-registered extension verbs (e.g. Slack) alike.
// LocalClient's recording methods delegate here, and an ee package, which
// can't reach LocalClient's internals, gets the same stamp/write/touch
// discipline through RecordExternalWrite.

// withWriteInfo picks the per-run routing strategy off info directly (the
// free-function counterpart of LocalClient.withWrite, for callers — like an
// ee extension handler — that hold a ConversationInfo but not a LocalClient).
func withWriteInfo(
	ctx context.Context,
	stores db.Stores,
	info ConversationInfo,
	system func() error,
	user func(ts db.TxStores) error,
) error {
	if info.IsEventTriggered {
		return system()
	}
	return stores.Tx.SyntheticClaimsWithTx(ctx, info.OrgID, info.UserID, user)
}

// stampActionIdentityInfo fills the conversation/org/team/actor common to
// every bot-attributed action from info. The action-specific fields
// (provider, action, target, credential, from/to, dedup) are set by the
// caller.
func stampActionIdentityInfo(act *domain.ExternalAction, info ConversationInfo) {
	act.OrgID = info.OrgID
	act.TeamID = info.TeamID
	act.ConversationID = info.ConversationID
	act.ActorUserID = info.UserID // empty for event-triggered → SQL NULL
}

// stampScopeInfo sets the Scope of a and act (either may be nil) to the
// org's current scope for each one's provider (domain.ExternalObjectScope).
// Every exec write goes to the provider the org is configured for now, so that
// is the namespace the object it wrote lives in. The scope is overwritten, not
// defaulted, like the run identity stamped beside it: the side that holds the
// stores decides it, so a relayed row cannot name a namespace the write did
// not reach.
func stampScopeInfo(ctx context.Context, stores db.Stores, orgID string, a *domain.Artifact, act *domain.ExternalAction) error {
	if a == nil && act == nil {
		return nil
	}
	if stores.Orgs == nil {
		return errors.New("stamp external object scope: no org store to read settings from")
	}
	settings, err := stores.Orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("stamp external object scope: load org settings: %w", err)
	}
	if a != nil {
		a.Scope = domain.ExternalObjectScope(a.Provider, settings)
	}
	if act != nil {
		act.Scope = domain.ExternalObjectScope(act.Provider, settings)
	}
	return nil
}

// recordActionSystemInfo appends act on the admin pool (event-triggered
// runs). A nil act — or a partial test wiring with no ExternalActions store —
// is a no-op.
func recordActionSystemInfo(ctx context.Context, stores db.Stores, info ConversationInfo, act *domain.ExternalAction) error {
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

// recordActionAlone appends act in a write of its own, on the pool the run's
// writes take, retried like every write-stage write (withRecordRetry). It is
// what a funnel falls back to when the artifact composed with act failed: a
// manual run's transaction rolled the action back with it, and an
// event-triggered run never reached it. The ledger row is the audit log of
// record and does not depend on the artifact. The commonest way the artifact
// fails while the action would not is RLS: artifacts are keyed org-wide but
// updated team-scoped, so a manual run acting on an object another team's run
// recorded first cannot update that row, while the ledger row it appends is
// only org-scoped.
func recordActionAlone(ctx context.Context, stores db.Stores, info ConversationInfo, act *domain.ExternalAction) error {
	return withRecordRetry(ctx, func() error {
		return withWriteInfo(ctx, stores, info,
			func() error { return recordActionSystemInfo(ctx, stores, info, act) },
			func(ts db.TxStores) error { return recordActionTx(ctx, ts, info.OrgID, act) },
		)
	})
}

// deletesUnrecordedComment reports whether a retires a comment that has no
// row. A comment row exists only for a comment TF posted, so a delete never
// creates one: a delete of a person's comment, or of one whose post was never
// recorded, is recorded by its audit row alone. A failed read is answered the
// same way and counted, since a row the delete created would claim the comment
// was TF's.
func deletesUnrecordedComment(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Artifact) bool {
	if a == nil || a.Kind != domain.ArtifactKindComment || a.State != domain.ArtifactStateCommentDeleted {
		return false
	}
	var present map[string]bool
	err := withRecordRetry(ctx, func() error {
		var e error
		present, e = stores.Artifacts.PresentDedupKeysSystem(ctx, info.OrgID, a.Scope, []string{a.DedupKey})
		return e
	})
	if err != nil {
		recordWriteDrop(ctx, recordDropOp(a, nil), err)
		agenthostLog.Warn("reading a deleted comment's artifact failed; recording the action without it",
			"conversation", info.ConversationID, "error", err)
		return true
	}
	return !present[a.DedupKey]
}

// RecordExternalWrite is the shared recording funnel behind every exec
// choke-point write: it stamps the run's identity and the org's current scope
// for each provider (stampScopeInfo) onto a and act (if present), links a to
// the entity it is about (resolveArtifactEntity), upserts a (if non-nil) and
// appends act (if non-nil) in the SAME write — admin pool for event-triggered
// runs, a synthetic-claims tx for manual ones — logs (never fails) a write
// error, and records the touched entity once the write settles. Both entity
// resolutions run outside the write's transaction (see recordEntityTouch).
//
// a is nil for an audit-only write that produces no artifact to compose
// with (a Slack reaction, a review-thread reply) — mirrors the former
// recordBotAction. act is nil when the write carries no distinct external
// action (a review-draft snapshot re-upserting its artifact with no new
// external write) — mirrors the former upsertGithubArtifact/upsertJiraArtifact
// with act == nil. The caller is responsible for setting a.Provider (and any
// other polymorphic fields) before calling — this function only stamps the
// run-identity fields common to every provider. A comment's delete is written
// only over the comment's existing row (deletesUnrecordedComment).
//
// Best-effort throughout: every write here already happened against the
// external system (GitHub/Jira/Slack) before this is called, so a recording
// failure is logged and swallowed — it must never unwind the agent's
// already-applied action. Nobody else will retry it either, so the funnel does
// (record_retry.go): it runs on a context of its own, detached from the
// caller's, fixes the action's dedup key before the first attempt, and retries
// each write that fails on a retryable error. What still fails is swallowed,
// but counted: this is the last stage of the audit funnel auditstats.go
// measures, and the only one a local-mode deployment (no sidecar, no relay)
// can reach. A failed artifact never costs the action: the action is then
// written on its own (recordActionAlone), and each failure is counted against
// what it lost.
func RecordExternalWrite(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Artifact, act *domain.ExternalAction) {
	recordExternalWrite(ctx, stores, info, a, act, domain.EntityContainer{})
}

// recordExternalWrite is RecordExternalWrite carrying container: what the
// response the verb acted on says the touched issue sits in (a Linear issue's
// team, a Jira issue's project), which the touch needs to create that issue's
// entity. Zero for every other write.
func recordExternalWrite(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Artifact, act *domain.ExternalAction, container domain.EntityContainer) {
	if a != nil {
		if stores.Artifacts == nil {
			return
		}
		a.ConversationID = info.ConversationID
		a.OrgID = info.OrgID
		a.TeamID = info.TeamID
	} else if act == nil || stores.ExternalActions == nil {
		return
	}
	if act != nil {
		stampActionIdentityInfo(act, info)
		mintActionDedupKey(ctx, info, act)
	}
	ctx, cancel := recordContext(ctx)
	defer cancel()

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
		trace.WithAttributes(telemetry.ConversationID(info.ConversationID), telemetry.OrgID(info.OrgID)))
	defer span.End()
	if a != nil {
		span.SetAttributes(telemetry.Op(a.Kind))
	}

	// lost is what an error left in err cost: the artifact and the action
	// written with it, or the action alone once the artifact's failure has been
	// reported on its own.
	lost := a
	err := withRecordRetry(ctx, func() error { return stampScopeInfo(ctx, stores, info.OrgID, a, act) })
	if err == nil && deletesUnrecordedComment(ctx, stores, info, a) {
		a, lost = nil, nil
	}
	var subject resolvedEntity
	if err == nil && a != nil {
		subject = resolveArtifactEntity(ctx, stores, info, a, container)
	}
	if err == nil {
		var artifactErr error
		err = withRecordRetry(ctx, func() error {
			return withWriteInfo(ctx, stores, info,
				func() error {
					artifactErr = nil
					if a != nil {
						if _, e := stores.Artifacts.UpsertSystem(ctx, info.OrgID, *a); e != nil {
							artifactErr = e
							return e
						}
					}
					return recordActionSystemInfo(ctx, stores, info, act)
				},
				func(ts db.TxStores) error {
					artifactErr = nil
					if a != nil {
						if _, e := ts.Artifacts.Upsert(ctx, info.OrgID, *a); e != nil {
							artifactErr = e
							return e
						}
					}
					return recordActionTx(ctx, ts, info.OrgID, act)
				},
			)
		})
		if artifactErr != nil && act != nil {
			recordSpanError(span, artifactErr)
			recordWriteDrop(ctx, recordDropOp(a, nil), artifactErr)
			agenthostLog.Warn("artifact recording failed; recording the action without it",
				"conversation", info.ConversationID, "kind", a.Kind, "target", a.Target, "error", artifactErr)
			err = recordActionAlone(ctx, stores, info, act)
			lost = nil
		}
	}
	if err != nil {
		target := ""
		kind := ""
		if lost != nil {
			target, kind = lost.Target, lost.Kind
		} else if act != nil {
			target = act.Target
		}
		recordSpanError(span, err)
		recordWriteDrop(ctx, recordDropOp(lost, act), err)
		agenthostLog.Warn("external write recording failed (action already applied)",
			"conversation", info.ConversationID, "kind", kind, "target", target, "error", err)
	}
	// Record the touched entity outside the audit write (TFAC-513 §2).
	recordTouchInfo(ctx, stores, info, a, act, container, subject)
	// Same placement, same reason: an admin-pool write, so it must run after
	// withWriteInfo's tx has settled.
	stampPRAttribution(ctx, stores, info, a)
}

// --- touched-entity resolution (TFAC-513 §2) ---
//
// Every org-credential write the bot makes lands on an external object — a PR,
// a Jira issue, a Slack thread. resolveTouchedEntityInfo turns that object's
// (provider, target) into an entities row, returning an existing entity or a
// freshly-minted snapshot-less stub.

// entityCoordinate is an external object as the exec funnel saw it: the
// (provider, target) a write or an addressed read named, its link, and — for a
// provider whose entities are identified by an id a key change does not move —
// that id, off the response the verb read or the artifact's dedup key.
type entityCoordinate struct {
	provider, target, url string
	externalID            string
	// scope is the namespace the coordinate was recorded in, when it names
	// one: a Jira artifact's dedup key carries its site. "" means the org's
	// current scope.
	scope string
	// container is what the response says the object sits in, for a Linear
	// or Jira coordinate: a Linear entity is created carrying its team and a
	// Jira entity its project, and neither is created without it.
	container domain.EntityContainer
}

// resolveTouchedEntityInfo maps a coordinate to an entities row, returning an
// existing entity or a freshly-minted snapshot-less stub, and "" for anything
// the touched-entity rule skips (a repo-level GitHub target with no '#N', an
// empty key, a Jira coordinate with no issue id, a coordinate from another
// scope than the org's current one, an unmapped provider). It is the
// free-function counterpart of the former LocalClient.resolveTouchedEntity;
// kept here so the write funnel and the read path both reach it without a
// LocalClient.
//
// The coordinate's key is what the provider answered with at the time of the
// write or read, so an entity stored under another key is renamed onto it
// (entityscope.Resolve with follow): an agent touching a moved Jira issue by
// its old key resolves the issue's existing entity, never a second one.
//
// The Slack case mints a "message" entity keyed on target — expected to already
// be domain.SlackSourceID(channel, rootTS), the same key the ingest pipeline
// uses (ee/slack/ingest.go), so a bot-authored write on a thread
// resolves/creates the identical entity a human mention would.
func resolveTouchedEntityInfo(ctx context.Context, stores db.Stores, info ConversationInfo, c entityCoordinate) (string, error) {
	entity, err := resolveEntityInfo(ctx, stores, info, c)
	if err != nil || entity == nil {
		return "", err
	}
	return entity.ID, nil
}

// resolveEntityInfo is resolveTouchedEntityInfo returning the entity itself,
// nil where that returns "".
func resolveEntityInfo(ctx context.Context, stores db.Stores, info ConversationInfo, c entityCoordinate) (*domain.Entity, error) {
	if stores.Entities == nil {
		return nil, nil
	}
	ref, kind, ok := domain.EntityRefForExternal(c.provider, c.target, c.externalID)
	if !ok {
		return nil, nil
	}
	scope, err := entityscope.Of(ctx, stores, info.OrgID, ref.Source)
	if err != nil {
		return nil, err
	}
	if scope == "" || (c.scope != "" && c.scope != scope) {
		// The source has no scope in this org — it is not configured — or the
		// object belongs to a namespace the org has since left, so the object
		// has no address an entity could be keyed under.
		return nil, nil
	}
	ref.Scope = scope
	// The poll cycle (or, for Slack, the ingest pipeline) seeds the title from
	// context; neither an ExternalAction nor an addressed read carries one.
	// url rides through when present.
	return entityscope.Resolve(ctx, stores, info.OrgID, ref, c.container, kind, c.url, true)
}

// recordEntityTouch resolves-or-creates the touched entity for a coordinate
// and, when it maps to a real entity, persists a durable
// (conversation_id, entity_id, role='touched') row. Shared by the write funnel
// (recordTouchInfo) and the addressed-read path (Runtime.RecordReadTouch).
//
// Best-effort throughout: a resolve or record failure is logged, counted
// (memoryentities.CountSkippedLink) and swallowed so it never fails the agent's
// already-applied write or the read that triggered it — the entity self-heals
// on a later touch or poll, and the touch row on the next addressed hit. It
// MUST run outside any audit tx: local mode holds the single SQLite connection
// for the life of a synthetic-claims tx, so these ...System (admin-pool)
// writes inside that closure would deadlock. Every funnel caller invokes it
// only after withWriteInfo returns; the read path carries no tx.
// It returns the entity it resolved (empty for a skipped or failed resolve).
func recordEntityTouch(ctx context.Context, stores db.Stores, info ConversationInfo, c entityCoordinate) string {
	id, err := resolveTouchedEntityInfo(ctx, stores, info, c)
	if err != nil {
		agenthostLog.Warn("touched-entity resolve failed (will retry on next poll)",
			"conversation", info.ConversationID, "target", c.target, "error", err)
		memoryentities.CountSkippedLink(ctx, info.OrgID, domain.MemoryRoleTouched, c.provider, err)
		return ""
	}
	recordTouchRow(ctx, stores, info, c.provider, id)
	return id
}

// recordTouchRow writes the (conversation, entity, role='touched') row for an
// entity a coordinate of provider resolved to; an empty id is a skipped
// coordinate and writes nothing.
func recordTouchRow(ctx context.Context, stores db.Stores, info ConversationInfo, provider, id string) {
	if id == "" || stores.TaskMemory == nil {
		return
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, info.OrgID, info.ConversationID, id, domain.MemoryRoleTouched); err != nil {
		agenthostLog.Warn("touched-entity record failed", "conversation", info.ConversationID, "entity", id, "error", err)
		memoryentities.CountSkippedLink(ctx, info.OrgID, domain.MemoryRoleTouched, provider, err)
	}
}

// resolvedEntity is the funnel's resolution of the entity its artifact is
// about, carried to the touch so a write whose action names the same object
// resolves it once. The coordinate rides along because reuse is only sound
// when the touch wants the same key: the touch resolves off the external
// action, the artifact's entity off the artifact.
type resolvedEntity struct {
	// attempted is whether a resolution of (provider, target) ran; entityID
	// is what it answered, empty when it skipped the coordinate or failed,
	// and err why it failed.
	attempted bool
	entityID  string
	err       error
	provider  string
	target    string
}

// matches reports whether r resolved (or failed to resolve) the same
// (provider, target) another consumer is about to resolve. A zero r never
// matches.
func (r resolvedEntity) matches(provider, target string) bool {
	return r.attempted && r.provider == provider && r.target == target
}

// resolveArtifactEntity links a to the entity it is about: the pull request a
// GitHub pull request, review or comment artifact is on, the Jira or Linear
// issue, the Slack thread. It resolves (and creates, like the touch) the
// entity for a's provider and target, identified by the provider id a's key
// carries (domain.ArtifactEntityIdentity), and sets a.EntityID to it and
// a.Target to its key: an artifact's target is its entity's key, which every
// rename moves by entity_id from then on. A branch, which has no entity, and
// an artifact whose entity does not resolve are written unlinked, and the
// failure is logged. Best-effort, like the rest of the funnel: the write
// happened, and its row is recorded either way. The link is overwritten, never
// taken from the caller, like the scope stamped beside it: the side that holds
// the stores decides it, so a relayed row cannot name an entity of its choosing.
func resolveArtifactEntity(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Artifact, container domain.EntityContainer) resolvedEntity {
	a.EntityID = ""
	c := entityCoordinate{provider: a.Provider, target: a.Target, url: a.URL, container: container}
	if scope, externalID, ok := domain.ArtifactEntityIdentity(*a); ok {
		c.scope, c.externalID = scope, externalID
	}
	out := resolvedEntity{attempted: true, provider: a.Provider, target: a.Target}
	entity, err := resolveEntityInfo(ctx, stores, info, c)
	if err != nil {
		out.err = err
		agenthostLog.Warn("resolving the artifact's entity failed; recording the artifact unlinked",
			"conversation", info.ConversationID, "kind", a.Kind, "target", a.Target, "error", err)
		return out
	}
	if entity != nil {
		out.entityID = entity.ID
		a.EntityID, a.Target = entity.ID, entity.SourceID
	}
	return out
}

// recordTouchInfo persists the write funnel's touched entity: it unwraps the
// external action into its (provider, target, url) and records the
// conversation→entity touch. An action about the object the artifact is about
// reuses subject, the artifact's entity, rather than resolving it again. Any
// other action resolves its own coordinate, identified by the provider id
// off the artifact when it is the same provider's (a Jira artifact's key names
// its site and issue id, a pull request's artifact carries its node id; see
// domain.ArtifactEntityIdentity). A nil action (an audit-only write with no
// external action) touches nothing. See recordEntityTouch for the best-effort
// + outside-the-tx contract.
func recordTouchInfo(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Artifact, act *domain.ExternalAction, container domain.EntityContainer, subject resolvedEntity) {
	if act == nil {
		return
	}
	if subject.matches(act.Provider, act.Target) {
		if subject.err != nil {
			memoryentities.CountSkippedLink(ctx, info.OrgID, domain.MemoryRoleTouched, act.Provider, subject.err)
			return
		}
		recordTouchRow(ctx, stores, info, act.Provider, subject.entityID)
		return
	}
	c := entityCoordinate{provider: act.Provider, target: act.Target, url: act.URL, container: container}
	// A Jira or Linear artifact's key names the issue every action beside it
	// is about. A pull request artifact's node id names one pull request, and
	// an action about that one matched subject above, so a GitHub action
	// here is about another object and takes nothing off the artifact.
	if a != nil && a.Provider == act.Provider && a.Provider != domain.ArtifactProviderGitHub {
		if scope, externalID, ok := domain.ArtifactEntityIdentity(*a); ok {
			c.scope, c.externalID = scope, externalID
		}
	}
	recordEntityTouch(ctx, stores, info, c)
}

// --- attribution stamps for a bot-opened PR ---

// stampPRAttribution records, on the entity behind a PR the bot just opened,
// the two facts only the run knows: the team that commissioned it
// (owning_team_id) and the human who asked for it
// (commissioned_by_user_id).
//
// The gap they fill is the same one twice. The author of a PR TF opened is a
// bot that maps to no TF user, so neither routing's owner nor the personal
// list's "mine" can be derived from the object itself — the run that opened it
// carries both answers, and this is the only moment the run and the entity are
// in the same place. Left unstamped, the PR's first ci_check_failed finds no
// owner and mints a task for nobody unless the repo happens to be
// project-attached, and the person who pressed delegate never sees the pull
// request their own ask produced.
//
// The two columns are written together and mean different things: the team is
// the structural OWNER routing reads, the user is PROVENANCE — who asked —
// and is never read as a second owner. Each is independently guarded, so a run
// with a team and no human (an event-triggered one) stamps only the owner.
//
// Scoped to kind=pull_request deliberately. A submitted-review artifact shares
// the owner/repo#N target shape, and reviewing someone else's PR is the clearest
// case of touching an entity you must not come to own.
//
// Ordering against the poller does not matter. The entity is the one the
// artifact was linked to (resolveArtifactEntity), which resolves the pull
// request by its node id and its key as the tracker does, so it is either the
// row the poller already created or the stub the poller later enriches; the
// stamp itself is if-NULL, so neither ordering can overwrite an owner set
// deliberately elsewhere.
//
// Best-effort, like everything else downstream of an already-applied write: the
// PR is open on GitHub regardless, and a failure here costs routing precision on
// that PR, never the run. It is not retried — the entity is durable and the
// event that would need the owner has not arrived yet, but nothing re-derives
// the stamp later either, so a failure means that PR's events fall through to
// the router's remaining tiers for good. A pull request artifact whose entity
// did not resolve has nothing to stamp.
func stampPRAttribution(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Artifact) {
	if a == nil || a.Provider != domain.ArtifactProviderGitHub || a.Kind != domain.ArtifactKindPullRequest {
		return
	}
	// Defensive: auto-fire is gated on an owned task and artifacts.team_id is
	// NOT NULL, so a conversation without a team should not reach here. A run
	// with neither a team nor a creator has nothing to attribute.
	if (info.TeamID == "" && info.UserID == "") || stores.Entities == nil {
		return
	}
	entityID := a.EntityID
	if entityID == "" {
		return
	}
	stamped, err := stores.Entities.StampOwningTeamIfUnsetSystem(ctx, info.OrgID, entityID, info.TeamID)
	if err != nil {
		agenthostLog.Warn("owning-team stamp failed",
			"conversation", info.ConversationID, "entity", entityID, "target", a.Target, "error", err)
	} else if stamped {
		agenthostLog.Info("stamped owning team on bot-opened PR",
			"conversation", info.ConversationID, "entity", entityID, "target", a.Target, "team", info.TeamID)
	}
	// info.UserID is conversations.creator_user_id — the accountable asker, not
	// whoever drove the turn that ran the create. Empty for an event-triggered
	// run, where the stamp is correctly a no-op. A failure here is independent
	// of the one above: neither column is worth losing because the other's
	// write failed.
	stamped, err = stores.Entities.StampCommissionedByIfUnsetSystem(ctx, info.OrgID, entityID, info.UserID)
	if err != nil {
		agenthostLog.Warn("commissioned-by stamp failed",
			"conversation", info.ConversationID, "entity", entityID, "target", a.Target, "error", err)
		return
	}
	if stamped {
		agenthostLog.Info("stamped commissioning user on bot-opened PR",
			"conversation", info.ConversationID, "entity", entityID, "target", a.Target, "user", info.UserID)
	}
}

// loadEntityMemory is the host side of `exec memory load`: it looks up the
// entity for (source, sourceID) in the org's current scope for the source — by
// externalID, the provider id the entity is identified by, when the caller has
// one, else by its natural key — LOOKUP ONLY, never
// FindOrCreate, so a load of something unknown is a miss, not a stub mint — and
// on a hit returns that entity's prior conversation memory scoped to the conversation's team,
// plus records a best-effort conversation→entity 'touched' row (loading IS an address).
//
// Unlike recordEntityTouch's resolve-or-create, a miss here records NOTHING and
// returns an empty result with an empty EntityID — the deliberate asymmetry the
// ticket calls out: an addressed WRITE/read on a live object legitimately mints
// a stub the poll cycle enriches, but reading another entity's memory must not
// conjure the entity into existence.
//
// Content is the agent's own narrative verbatim, the same thing the spawn-time
// materializer writes into a run tree — so the on-demand pull reads identically
// to the auto-staged files. Count is the pre-limit scoped total (its dedicated
// count method); Memories is the most recent `limit`, capped IN the query (not
// fetched-all-then-sliced) so a hot entity's long history isn't transferred to
// keep only the tail. The touch is best-effort — a read never fails on its
// touch.
func loadEntityMemory(ctx context.Context, stores db.Stores, info ConversationInfo, source, sourceID, externalID string, limit int) (*MemoryLoadResult, error) {
	res := &MemoryLoadResult{Source: source, SourceID: sourceID, Memories: []MemoryLoadEntry{}}
	if stores.Entities == nil {
		return res, nil
	}
	scope, err := entityscope.Of(ctx, stores, info.OrgID, source)
	if err != nil {
		return nil, err
	}
	var entity *domain.Entity
	if externalID != "" {
		entity, err = stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, source, scope, externalID)
	} else {
		entity, err = stores.Entities.GetBySourceSystem(ctx, info.OrgID, source, scope, sourceID)
	}
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
				ConversationID: m.ConversationID,
				BlueprintRunID: m.BlueprintRunID,
				CreatedAt:      m.CreatedAt,
				Content:        m.Content,
			})
		}

		// Record the touch strictly after the reads (loading is an address).
		// Best-effort: a load never fails on its touch — the row self-heals on the
		// next addressed hit, and on the sidecar a dropped relay costs one row.
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, info.OrgID, info.ConversationID, entity.ID, domain.MemoryRoleTouched); err != nil {
			agenthostLog.Warn("memory-load touch record failed", "conversation", info.ConversationID, "entity", entity.ID, "error", err)
			memoryentities.CountSkippedLink(ctx, info.OrgID, domain.MemoryRoleTouched, entity.Source, err)
		}
	}
	return res, nil
}

// defaultMemoryLoadLimit is the cap loadEntityMemory applies when a caller
// passes a non-positive limit (the CLI never does — it defaults to and enforces
// a positive value — so this only guards direct Client / IPC / relay callers).
// Mirrors the CLI's default so both surfaces agree on "20 most recent".
const defaultMemoryLoadLimit = 20
