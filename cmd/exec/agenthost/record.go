package agenthost

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entityscope"
	"github.com/sky-ai-eng/triage-factory/internal/memoryentities"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
// every bot-attributed action from info, and names the run's agent as its
// performer. The action-specific fields (provider, action, target, credential,
// from/to, dedup) are set by the caller. The performer is overwritten rather
// than defaulted, like the identity beside it: a relayed row cannot claim a
// person performed it.
func stampActionIdentityInfo(act *domain.ExternalAction, info ConversationInfo) {
	act.OrgID = info.OrgID
	act.TeamID = info.TeamID
	act.ConversationID = info.ConversationID
	act.ActorUserID = info.UserID // empty for event-triggered → SQL NULL
	act.PerformedBy = domain.PerformedByAgent
}

// stampScopeInfo sets the Scope of a and act (either may be nil) to the
// org's current scope for each one's provider (domain.ExternalObjectScope).
// Every exec write goes to the provider the org is configured for now, so that
// is the namespace the object it wrote lives in. The scope is overwritten, not
// defaulted, like the run identity stamped beside it: the side that holds the
// stores decides it, so a relayed row cannot name a namespace the write did
// not reach.
func stampScopeInfo(ctx context.Context, stores db.Stores, orgID string, a *domain.Deliverable, act *domain.ExternalAction) error {
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

// errReviewKeyUnreadable refuses a review deliverable whose key names no pull
// request node id, so it cannot be rebuilt under the run's conversation.
var errReviewKeyUnreadable = errors.New("agenthost: a review deliverable's key names no pull request node id")

// keyReviewToConversation rebuilds review deliverable a's dedup key under
// info's conversation (domain.ReviewDedupKey), from the pull request node id
// its key carries (domain.DeliverablePullRequestNodeID). The key is this
// side's to build, like the identity stamped beside it: a review draft is
// keyed per conversation, so a key naming another conversation would conflict
// onto that conversation's draft and write over it. A review whose key carries
// no readable node id is refused with errReviewKeyUnreadable. Any other kind
// is left as it is.
func keyReviewToConversation(a *domain.Deliverable, info ConversationInfo) error {
	if a.Kind != domain.DeliverableKindReview {
		return nil
	}
	nodeID, ok := domain.DeliverablePullRequestNodeID(*a)
	if !ok {
		return errReviewKeyUnreadable
	}
	a.DedupKey = domain.ReviewDedupKey(nodeID, info.ConversationID)
	return nil
}

// errFunnelDeliverable refuses a deliverable the recording funnel does not
// write: anything but a GitHub pull request keyed on its node id.
var errFunnelDeliverable = errors.New("agenthost: the recording funnel records only a pull request create's deliverable")

// keyFunnelDeliverable admits a, a deliverable the recording funnel is to
// write, and rebuilds its key and node id from the pull request node id its
// key carries (domain.PullRequestDedupKey). Its one writer is the pull request
// create (recordGithubPR). The key is this side's to build, like the review
// key (keyReviewToConversation): a key carrying anything after the node id
// names the same pull request under a key no other row holds, so it would
// escape the insert's conflict with the row already recorded for it. A review
// draft is recorded through UpsertDeliverable and moved through its
// state-guarded writes, never through the funnel, so a review arriving here
// was not built by TF: refusing it keeps a relayed record from writing a
// draft whose state and review id the sender chose. Anything but a GitHub
// pull request keyed on its node id is refused with errFunnelDeliverable.
func keyFunnelDeliverable(a *domain.Deliverable) error {
	if a.Kind != domain.DeliverableKindPullRequest {
		return errFunnelDeliverable
	}
	nodeID, ok := domain.DeliverablePullRequestNodeID(*a)
	if !ok {
		return errFunnelDeliverable
	}
	a.DedupKey, a.NodeID = domain.PullRequestDedupKey(nodeID), nodeID
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
// runs), composing with the deliverable upsert. A nil act is a no-op.
func recordActionTx(ctx context.Context, ts db.TxStores, orgID string, act *domain.ExternalAction) error {
	if act == nil || ts.ExternalActions == nil {
		return nil
	}
	return ts.ExternalActions.Record(ctx, orgID, *act)
}

// recordActionAlone appends act in a write of its own, on the pool the run's
// writes take, retried like every write-stage write (withRecordRetry). It is
// what a funnel falls back to when the deliverable composed with act failed: a
// manual run's transaction rolled the action back with it, and an
// event-triggered run never reached it. The ledger row is the audit log of
// record and does not depend on the deliverable. The commonest way the deliverable
// fails while the action would not is RLS: deliverables are keyed org-wide but
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

// RecordExternalWrite is the shared recording funnel behind every exec
// choke-point write: it stamps the run's identity and the org's current scope
// for each provider (stampScopeInfo) onto a and act (if present), links a to
// the entity it is about (resolveDeliverableEntity) and act to the entity or
// repository it is about (resolveActionReferences), inserts a (if non-nil, and
// only when no row holds its key) and appends act (if non-nil) in the SAME write — admin pool for event-triggered
// runs, a synthetic-claims tx for manual ones — logs (never fails) a write
// error, and records the conversation's link to the act's entity once the write
// settles (recordActionMemoryLink). Both entity resolutions run before the write and
// outside its transaction (see recordEntityTouch).
//
// a is the pull request deliverable a create opened, and nil for every other
// write, which leaves its ledger row alone; any other deliverable is refused
// (keyFunnelDeliverable), and its action is recorded without it. act is nil for a
// write that carries no external action. The caller is responsible for
// setting a.Provider before calling — this function only stamps the
// run-identity fields common to every provider.
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
// can reach. A failed deliverable never costs the action: the action is then
// written on its own (recordActionAlone), and each failure is counted against
// what it lost.
func RecordExternalWrite(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Deliverable, act *domain.ExternalAction) {
	recordExternalWrite(ctx, stores, info, a, act, domain.EntityContainer{}, domain.GitHubIdentity{})
}

// recordExternalWrite is RecordExternalWrite carrying what the act's observer
// read. container is what the response the verb acted on says the touched
// issue sits in (a Linear issue's team, a Jira issue's project), which the
// touch needs to create that issue's entity. github is what GitHub answered
// for the objects a GitHub act names (domain.GitHubIdentity), which every
// GitHub entity and repository the write is linked to is resolved from. Each
// is zero for a write that has none.
func recordExternalWrite(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Deliverable, act *domain.ExternalAction, container domain.EntityContainer, github domain.GitHubIdentity) {
	if a != nil {
		if stores.Deliverables == nil {
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
		// The object is the writer's to name, but only one its action can
		// name: a relayed row cannot have a refusal claim it changed something,
		// or carry a kind the column's CHECK would refuse along with the
		// deliverable written beside it.
		if !domain.ActionNamesKind(act.Provider, act.Action, act.ObjectKind) {
			act.ObjectKind, act.ObjectKey = "", ""
		}
	}
	ctx, cancel := recordContext(ctx)
	defer cancel()

	// The deliverable write itself, under the relay op that carried it (a
	// sandboxed run) or under whatever ambient span exists. A CHILD rather
	// than a linked root: unlike the ops above it, this never runs on its own
	// — it is always inside a caller this trace already covers.
	//
	// The whole point is the swallow below. Every write here is best-effort
	// by contract, so a failure would otherwise surface only as a warn line;
	// the span makes it a trace with an error status and the drop counter
	// beside it makes it a number, which is how "the PR was opened but TF has
	// no deliverable for it" stops being invisible and starts being measurable.
	// The deliverable kind is a closed domain vocabulary; its target is a repo
	// path, and stays out.
	ctx, span := tracer.Start(ctx, "artifact.record",
		trace.WithAttributes(telemetry.ConversationID(info.ConversationID), telemetry.OrgID(info.OrgID)))
	defer span.End()
	if a != nil {
		span.SetAttributes(telemetry.Op(a.Kind))
		if kerr := keyFunnelDeliverable(a); kerr != nil {
			telemetry.RecordSpanError(span, kerr)
			recordWriteDrop(ctx, recordDropOp(a, nil), kerr)
			agenthostLog.Warn("refusing a deliverable that is not a pull request create's; recording the action without it",
				"conversation", info.ConversationID, "kind", a.Kind, "target", a.Target)
			if act == nil {
				return
			}
			a = nil
		}
	}

	// lost is what an error left in err cost: the deliverable and the action
	// written with it, or the action alone once the deliverable's failure has been
	// reported on its own.
	lost := a
	err := withRecordRetry(ctx, func() error { return stampScopeInfo(ctx, stores, info.OrgID, a, act) })
	var subject resolvedEntity
	if err == nil && a != nil {
		subject = resolveDeliverableEntity(ctx, stores, info, a, container, github)
	}
	var about resolvedEntity
	if act != nil {
		about = resolveActionReferences(ctx, stores, info, act, container, github, subject)
	}
	// The deliverable is a pull request create's (keyFunnelDeliverable),
	// written by an insert that does nothing when the row is there: GitHub
	// opens one pull request per node id, and a replay of the same create finds
	// its own row, so a create that finds a row recorded by another writer
	// leaves it as it was. written is whether an attempt's insert returned
	// without error; whose row the key holds is read once the write settles.
	var written bool
	if err == nil {
		var deliverableErr error
		err = withRecordRetry(ctx, func() error {
			return withWriteInfo(ctx, stores, info,
				func() error {
					deliverableErr = nil
					if a != nil {
						if _, e := stores.Deliverables.InsertDeliverableIfAbsentSystem(ctx, info.OrgID, *a); e != nil {
							deliverableErr = e
							return e
						}
						written = true
					}
					return recordActionSystemInfo(ctx, stores, info, act)
				},
				func(ts db.TxStores) error {
					deliverableErr = nil
					if a != nil {
						if _, e := ts.Deliverables.InsertDeliverableIfAbsent(ctx, info.OrgID, *a); e != nil {
							deliverableErr = e
							return e
						}
						written = true
					}
					return recordActionTx(ctx, ts, info.OrgID, act)
				},
			)
		})
		if deliverableErr != nil && act != nil {
			telemetry.RecordSpanError(span, deliverableErr)
			recordWriteDrop(ctx, recordDropOp(a, nil), deliverableErr)
			agenthostLog.Warn("deliverable recording failed; recording the action without it",
				"conversation", info.ConversationID, "kind", a.Kind, "target", a.Target, "error", deliverableErr)
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
		telemetry.RecordSpanError(span, err)
		recordWriteDrop(ctx, recordDropOp(lost, act), err)
		agenthostLog.Warn("external write recording failed (action already applied)",
			"conversation", info.ConversationID, "kind", kind, "target", target, "error", err)
	} else {
		countUnnamedComment(ctx, act)
	}
	// Link the conversation to the act's entity outside the audit write.
	recordActionMemoryLink(ctx, stores, info, act, about)
	// Same placement, same reason: an admin-pool write, so it must run after
	// withWriteInfo's tx has settled. A create attributes only a pull request
	// whose deliverable the run holds: one that found the row recorded by
	// another writer did not open it.
	if written && runHoldsDeliverable(ctx, stores, info, *a) {
		stampPRAttribution(ctx, stores, info, a)
	}
}

// runHoldsDeliverable reports whether the run's own deliverables hold a row
// under a's scope and key. It is read after the write settled, so no attempt
// that did not land — a rolled-back transaction, a retry, a lost
// acknowledgement — decides the answer. A read failure answers false:
// attribution is best-effort, and is stamped only for a row known to be the
// run's.
func runHoldsDeliverable(ctx context.Context, stores db.Stores, info ConversationInfo, a domain.Deliverable) bool {
	rows, err := stores.Deliverables.ListByConversationSystem(ctx, info.OrgID, info.ConversationID)
	if err != nil {
		agenthostLog.Warn("reading the run's deliverables failed; attributing nothing to the pull request",
			"conversation", info.ConversationID, "target", a.Target, "error", err)
		return false
	}
	for _, r := range rows {
		if r.Scope == a.Scope && r.DedupKey == a.DedupKey {
			return true
		}
	}
	return false
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
// that id, off the response the verb read or the deliverable's dedup key.
type entityCoordinate struct {
	provider, target, url string
	externalID            string
	// scope is the namespace the coordinate was recorded in, when it names
	// one: a Jira object key carries its site. "" means the org's current
	// scope.
	scope string
	// container is what the response says the object sits in, for a Linear
	// or Jira coordinate: a Linear entity is created carrying its team and a
	// Jira entity its project, and neither is created without it.
	container domain.EntityContainer
	// github is what GitHub answered for a GitHub coordinate's object and
	// repository (domain.GitHubIdentity), the only thing a GitHub entity is
	// resolved from. externalID is unused for GitHub.
	github domain.GitHubIdentity
}

// resolveTouchedEntityInfo maps a coordinate to an entities row, returning an
// existing entity or a freshly-minted snapshot-less stub, and "" for anything
// the touched-entity rule skips (a GitHub coordinate with no node id, an
// empty key, a Jira coordinate with no issue id, a coordinate from another
// scope than the org's current one, an unmapped provider). It is the
// free-function counterpart of the former LocalClient.resolveTouchedEntity;
// kept here so the write funnel and the read path both reach it without a
// LocalClient.
//
// A coordinate carrying a provider id resolves the entity carrying it,
// whatever key it is stored under: an agent touching a moved Jira issue by its
// old key resolves the issue's existing entity, never a second one. In local
// mode the coordinate's key is what the provider answered with at the time of
// the write or read, so the entity is renamed onto it (followsProviderKey). A
// GitHub coordinate is resolved by its ids instead (resolveGitHubEntity).
//
// The Slack case finds or mints a "message" entity for the thread target
// names — domain.SlackSourceID(channel, rootTS), which the Slack resolver
// splits into the channel, resolved to its registry row, and the root ts, the
// pair a thread is identified by — so a bot-authored write on a thread
// resolves the identical entity a human mention would.
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
	if c.provider == domain.ProviderGitHub {
		return resolveGitHubEntity(ctx, stores, info, c)
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
	return entityscope.Resolve(ctx, stores, info.OrgID, ref, c.container, kind, c.url, followsProviderKey())
}

// followsProviderKey reports whether a coordinate's key is taken as the key
// its provider answers with now, so an entity found by the coordinate's id
// under another key is renamed onto it (entityscope.Resolve with follow). Only
// in local mode. In multi mode the coordinate was relayed by the run's
// sidecar, which chose its key and its id alike, so following it would let a
// relayed write rename another issue's entity onto a key of its choosing. The
// entity is still resolved by its id and linked as stored; the poll cycle
// renames it when the provider's own answer says its key moved, for an issue
// in a tracked Jira project or Linear team.
//
// TODO(TFAC-1297): an issue outside every tracked set is never polled, so in
// multi mode its entity keeps the key it had when TF first saw it.
func followsProviderKey() bool {
	return runmode.Current() != runmode.ModeMulti
}

// resolveGitHubEntity maps a GitHub coordinate to the entity of the pull
// request or issue its identity names, by GitHub's ids alone. The node id
// finds the entity whatever key it is stored under. On a miss the entity is
// found or created under the key built from the registry row GitHub's
// repository id names (githubRepositoryRow), in the name that row holds now,
// and the number the coordinate's target ends in, carrying the node id and
// the kind the read answered; a stub under that key with no id adopts both,
// as the PR poller's stub enrichment does. So a write that spelled an old
// name of its repository, or a new one TF has not applied, lands on the one
// entity and the one registry row.
//
// nil, never a key lookup, for a coordinate with no node id (the observer's
// read failed, or the act names no pull request or issue), one whose
// repository id names no row the run may record against, one from another
// scope than the org's current GitHub host, and, in multi mode, one whose
// node id names an entity in a repository the run does not work in
// (runMayRecordAgainst): the node id was relayed too. No GitHub entity is found
// or created from a key alone: a key's owner/repo is a spelling, which GitHub
// may since have given to another repository.
func resolveGitHubEntity(ctx context.Context, stores db.Stores, info ConversationInfo, c entityCoordinate) (*domain.Entity, error) {
	id := c.github
	if id.NodeID == "" || !domain.IsGitHubObjectKind(id.Kind) {
		return nil, nil
	}
	scope, err := entityscope.Of(ctx, stores, info.OrgID, domain.ProviderGitHub)
	if err != nil {
		return nil, err
	}
	if scope == "" || (c.scope != "" && c.scope != scope) {
		return nil, nil
	}
	entity, err := stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, domain.ProviderGitHub, scope, id.NodeID)
	if err != nil {
		return nil, err
	}
	if entity != nil {
		if ok, err := relayedRepositoryAdmitted(ctx, stores, info, entity.RepositoryID); err != nil || !ok {
			return nil, err
		}
		return entity, nil
	}
	_, _, number, ok := domain.ParsePRTarget(c.target)
	if !ok {
		return nil, nil
	}
	row, err := githubRepositoryRow(ctx, stores, info, scope, id)
	if err != nil || row == nil {
		return nil, err
	}
	key := domain.PullRequestTarget(row.Slug(), number)
	entity, _, err = stores.Entities.FindOrCreateSystem(ctx, info.OrgID, domain.ProviderGitHub, scope, key, id.NodeID, id.Kind, "", c.url)
	if err != nil {
		return nil, err
	}
	if entity.ExternalID == "" {
		adopted, err := stores.Entities.AdoptGitHubIdentitySystem(ctx, info.OrgID, entity.ID, id.NodeID, id.Kind)
		if err != nil {
			return nil, err
		}
		if adopted != nil {
			entity = adopted
		}
	}
	return entity, nil
}

// githubRepositoryRow returns the registry row GitHub's repository id names on
// host, the org's GitHub host, for an act the run made; nil when the identity
// carries no id, or names no row the run may record against.
//
// In multi mode the id was read by the run's sidecar and relayed, so it must
// name a row the run may write to: one of its conversation_worktrees rows, or
// its task's own repository (IsTaskOwnRepository), the same two arms the
// repository gate admits a write by. Any other id is answered nil, which
// records the act unlinked, so a sidecar cannot choose the repository an act
// is filed under. Local mode has no relay and no gate, and a repository with
// no row there is registered by its id first (FindOrCreateByExternalID, under
// the name GitHub answered with it), never get-or-created from a name; a name
// a row holds under another id is a repository GitHub has given the name to
// since, which the poller has not seen yet, and is answered nil.
func githubRepositoryRow(ctx context.Context, stores db.Stores, info ConversationInfo, host string, id domain.GitHubIdentity) (*domain.Repository, error) {
	if id.RepositoryID == "" || host == "" || stores.Repos == nil {
		return nil, nil
	}
	row, err := stores.Repos.GetByExternalIDSystem(ctx, info.OrgID, host, id.RepositoryID)
	if err != nil {
		return nil, err
	}
	if runmode.Current() == runmode.ModeMulti {
		if row == nil {
			return nil, nil
		}
		if ok, err := relayedRepositoryAdmitted(ctx, stores, info, row.ID); err != nil || !ok {
			return nil, err
		}
		return row, nil
	}
	if row != nil {
		return row, nil
	}
	owner, repo, ok := targetRepository(id.Repository)
	if !ok {
		return nil, nil
	}
	registered, err := stores.Repos.FindOrCreateByExternalID(ctx, info.OrgID, domain.RepoRef{Host: host, Owner: owner, Repo: repo, ExternalID: id.RepositoryID})
	if errors.Is(err, db.ErrRepoSlugOccupied) {
		agenthostLog.Warn("GitHub's repository id names a repository whose name another registry row holds; recording unlinked",
			"conversation", info.ConversationID, "repo", id.Repository, "error", err)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if registered.RetiredAt != nil {
		// The registry answers a retired row by its id, but a retired row
		// answers to no name, so an entity keyed under its name would
		// get-or-create a second row for the repository. The act is recorded
		// unlinked instead.
		return nil, nil
	}
	return &registered, nil
}

// relayedRepositoryAdmitted reports whether an act may be filed under the
// repository whose registry row is repositoryID: always in local mode, and in
// multi mode, where the identity naming it was relayed by the run's sidecar,
// only for a repository the run works in (runMayRecordAgainst).
func relayedRepositoryAdmitted(ctx context.Context, stores db.Stores, info ConversationInfo, repositoryID string) (bool, error) {
	if runmode.Current() != runmode.ModeMulti {
		return true, nil
	}
	ok, err := runMayRecordAgainst(ctx, stores, info, repositoryID)
	if err == nil && !ok {
		agenthostLog.Info("a relayed GitHub id names a repository the run does not work in; recording unlinked",
			"conversation", info.ConversationID, "repository", repositoryID)
	}
	return ok, err
}

// runMayRecordAgainst reports whether the repository whose registry row is
// repositoryID is one the run works in: a conversation_worktrees row of the
// run's, or the run's task's own repository.
func runMayRecordAgainst(ctx context.Context, stores db.Stores, info ConversationInfo, repositoryID string) (bool, error) {
	if stores.ConversationWorktrees != nil {
		rows, err := stores.ConversationWorktrees.ListSystem(ctx, info.OrgID, info.ConversationID)
		if err != nil {
			return false, err
		}
		for _, w := range rows {
			if w.RepositoryID == repositoryID {
				return true, nil
			}
		}
	}
	return IsTaskOwnRepository(ctx, stores, info, repositoryID), nil
}

// recordEntityTouch resolves-or-creates the touched entity for a coordinate
// and, when it maps to a real entity, persists a durable
// (conversation_id, entity_id, role='touched') row. It is the addressed-read
// path's (Runtime.RecordReadTouch); the write funnel resolves its action's
// entity before the write (resolveActionReferences), links the audit row to it,
// and records the entity's row once the write settles (recordActionMemoryLink), as
// produced when the action names the object it changed.
//
// Best-effort throughout: a resolve or record failure is logged, counted
// (memoryentities.CountSkippedLink) and swallowed so it never fails the agent's
// already-applied write or the read that triggered it — the entity self-heals
// on a later touch or poll, and the touch row on the next addressed hit. Like
// every entity resolution here it MUST run outside any audit tx: local mode
// holds the single SQLite connection for the life of a synthetic-claims tx, so
// these ...System (admin-pool) writes inside that closure would deadlock. The
// read path carries no tx. It returns the entity it resolved (empty for a
// skipped or failed resolve).
func recordEntityTouch(ctx context.Context, stores db.Stores, info ConversationInfo, c entityCoordinate) string {
	id, err := resolveTouchedEntityInfo(ctx, stores, info, c)
	if err != nil {
		agenthostLog.Warn("touched-entity resolve failed (will retry on next poll)",
			"conversation", info.ConversationID, "target", c.target, "error", err)
		memoryentities.CountSkippedLink(ctx, info.OrgID, domain.MemoryRoleTouched, c.provider, err)
		return ""
	}
	recordMemoryLink(ctx, stores, info, c.provider, id, domain.MemoryRoleTouched)
	return id
}

// recordMemoryLink writes the (conversation, entity, role) row for an entity a
// coordinate of provider resolved to; an empty id is a skipped coordinate and
// writes nothing. The store keeps the higher of role and any role the pair
// already holds.
func recordMemoryLink(ctx context.Context, stores db.Stores, info ConversationInfo, provider, id, role string) {
	if id == "" || stores.TaskMemory == nil {
		return
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, info.OrgID, info.ConversationID, id, role); err != nil {
		agenthostLog.Warn("entity link record failed", "conversation", info.ConversationID, "entity", id, "role", role, "error", err)
		memoryentities.CountSkippedLink(ctx, info.OrgID, role, provider, err)
	}
}

// resolvedEntity is the funnel's resolution of the entity its deliverable or its
// action is about, carried to the action and the touch so a write whose action
// names the same object resolves it once. The coordinate rides along because
// reuse is only sound when the consumer wants the same key: the action and the
// touch resolve off the external action, the deliverable's entity off the
// deliverable.
type resolvedEntity struct {
	// attempted is whether a resolution of (provider, target) ran; entityID
	// is what it answered, empty when it skipped the coordinate or failed,
	// and err why it failed.
	attempted bool
	entityID  string
	// entityKind is the resolved entity's kind (a GitHub pull request or
	// issue: domain.EntityKindPullRequest, domain.EntityKindIssue), empty
	// when nothing resolved.
	entityKind string
	err        error
	provider   string
	target     string
}

// matches reports whether r resolved (or failed to resolve) the same
// (provider, target) another consumer is about to resolve. A zero r never
// matches.
func (r resolvedEntity) matches(provider, target string) bool {
	return r.attempted && r.provider == provider && r.target == target
}

// resolveDeliverableEntity links a to the entity it is about: the pull request a
// pull request or review deliverable is on. It resolves (and creates, like the
// touch) the entity for a's target, identified by the pull request's node id a
// is keyed on (deliverableGitHubIdentity), and sets a.EntityID to it and
// a.Target to its key: a deliverable's target is its entity's key, which every
// rename moves by entity_id from then on. A deliverable whose entity does not
// resolve is written unlinked, and a failure is logged. Best-effort, like the rest of the funnel: the write
// happened, and its row is recorded either way. A failure another attempt can
// get past is retried like the write after it (withRecordRetry); resolving
// only finds, creates or renames toward what the provider answered, so a
// second attempt lands on what a first one wrote. The link is overwritten, never
// taken from the caller, like the scope stamped beside it: the side that holds
// the stores decides it, so a relayed row cannot name an entity of its choosing.
func resolveDeliverableEntity(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Deliverable, container domain.EntityContainer, github domain.GitHubIdentity) resolvedEntity {
	a.EntityID = ""
	c := entityCoordinate{provider: a.Provider, target: a.Target, url: a.URL, container: container, github: deliverableGitHubIdentity(*a, github)}
	out := resolvedEntity{attempted: true, provider: a.Provider, target: a.Target}
	var entity *domain.Entity
	err := withRecordRetry(ctx, func() error {
		var e error
		entity, e = resolveEntityInfo(ctx, stores, info, c)
		return e
	})
	if err != nil {
		out.err = err
		agenthostLog.Warn("resolving the deliverable's entity failed; recording the deliverable unlinked",
			"conversation", info.ConversationID, "kind", a.Kind, "target", a.Target, "error", err)
		return out
	}
	if entity != nil {
		out.entityID, out.entityKind = entity.ID, entity.Kind
		a.EntityID, a.Target = entity.ID, entity.SourceID
	}
	return out
}

// deliverableGitHubIdentity is the identity a deliverable's entity is resolved
// from. A pull request or review deliverable is keyed on its pull request's
// node id, which wins over anything relayed beside it; the repository id comes
// from github, the observer's read.
func deliverableGitHubIdentity(a domain.Deliverable, github domain.GitHubIdentity) domain.GitHubIdentity {
	if nodeID, ok := domain.DeliverablePullRequestNodeID(a); ok {
		github.NodeID, github.Kind = nodeID, domain.EntityKindPullRequest
	}
	return github
}

// resolveActionReferences links act to what it was about, by id, before the
// act is written: EntityID to the entity its (provider, target) names, and,
// for a GitHub act with no entity, RepositoryID to the registry row of its
// repository (resolveActionRepository). Both are overwritten, never taken from
// the caller, like the scope stamped beside them: the side that holds the
// stores decides them, so a relayed row cannot name an entity or a repository
// of its choosing.
//
// An action about the object the deliverable is about reuses subject, the
// deliverable's entity, rather than resolving it again. Any other action resolves
// (and creates, like the touch) its own coordinate. A GitHub action is
// identified by github, the identity its observer read. A Jira or Linear
// action's object key names the issue it is about, a comment's key the issue
// the comment is on, so the coordinate is identified by the site and issue id
// or the UUID the key carries (domain.ObjectKeyEntityIdentity). In multi mode
// the key was relayed, so the entity it names is linked as stored and never
// renamed onto the act's target (followsProviderKey). A failure another attempt can
// get past is retried like the write (withRecordRetry); resolving only finds,
// creates or renames toward what the provider answered, so a second attempt
// lands on what a first one wrote. One that still fails is logged, and the act
// is recorded unlinked. The resolution is returned for the touch, which
// records it once the write settles.
func resolveActionReferences(ctx context.Context, stores db.Stores, info ConversationInfo, act *domain.ExternalAction, container domain.EntityContainer, github domain.GitHubIdentity, subject resolvedEntity) resolvedEntity {
	act.EntityID, act.RepositoryID = "", ""
	about := subject
	if !subject.matches(act.Provider, act.Target) {
		c := entityCoordinate{provider: act.Provider, target: act.Target, url: act.URL, container: container, github: github}
		if scope, externalID, ok := domain.ObjectKeyEntityIdentity(act.Provider, act.ObjectKey); ok {
			c.scope, c.externalID = scope, externalID
		}
		about = resolvedEntity{attempted: true, provider: act.Provider, target: act.Target}
		var entity *domain.Entity
		about.err = withRecordRetry(ctx, func() error {
			var e error
			entity, e = resolveEntityInfo(ctx, stores, info, c)
			return e
		})
		switch {
		case about.err != nil:
			agenthostLog.Warn("resolving the action's entity failed; recording the action unlinked",
				"conversation", info.ConversationID, "action", act.Action, "target", act.Target, "error", about.err)
		case entity != nil:
			about.entityID, about.entityKind = entity.ID, entity.Kind
		}
	}
	act.EntityID = about.entityID
	// GitHub numbers pull requests and issues in one sequence and serves both
	// through the issue endpoints, so the endpoint a writer named its object
	// kind from does not say which this one is. The entity does.
	if act.Provider == domain.ProviderGitHub && act.EntityID != "" &&
		(act.ObjectKind == domain.ObjectKindPullRequest || act.ObjectKind == domain.ObjectKindIssue) {
		if kind := domain.GitHubEntityObjectKind(about.entityKind); kind != "" {
			act.ObjectKind = kind
		}
	}
	if act.EntityID == "" {
		act.RepositoryID = resolveActionRepository(ctx, stores, info, act, github)
	}
	return about
}

// resolveActionRepository returns the registry row id of the repository a
// GitHub act is in, on act's scope, the org's current GitHub host: the row
// GitHub's repository id names (githubRepositoryRow), which the act's observer
// read when it made the act, so a repository renamed since, or under a name
// TF has not applied yet, is the row it is. "" for an act of another
// provider, an act whose observer read no id (its read failed, or the act
// names no repository), and an id naming no row the run may record against.
//
// A denial (git_denied, gh_write_denied) is the exception: its repository is
// whatever the refused request claimed, nothing verified it, and no read was
// made for it. It is matched to a live row by name, lookup only, so the
// Actions feed can filter it by repository; a lookup never registers one. A
// read failure is retried like the write, and one that persists is logged and
// leaves the act unlinked.
func resolveActionRepository(ctx context.Context, stores db.Stores, info ConversationInfo, act *domain.ExternalAction, github domain.GitHubIdentity) string {
	if act.Provider != domain.ProviderGitHub || act.Scope == "" || stores.Repos == nil {
		return ""
	}
	var row *domain.Repository
	err := withRecordRetry(ctx, func() error {
		var e error
		if act.Action == domain.ActionGitDenied || act.Action == domain.ActionGHWriteDenied {
			row, e = deniedRepository(ctx, stores, info, act)
		} else {
			row, e = githubRepositoryRow(ctx, stores, info, act.Scope, github)
		}
		return e
	})
	if err != nil {
		agenthostLog.Warn("resolving the action's repository failed; recording the action unlinked",
			"conversation", info.ConversationID, "action", act.Action, "target", act.Target, "error", err)
		return ""
	}
	if row == nil {
		return ""
	}
	return row.ID
}

// deniedRepository is a denial's repository: the live row the owner/repo its
// target leads with names on act's scope, or nil. A target that names no
// repository (a node id or an endpoint path, which a GraphQL or user-level
// request records) answers nil.
func deniedRepository(ctx context.Context, stores db.Stores, info ConversationInfo, act *domain.ExternalAction) (*domain.Repository, error) {
	owner, repo, ok := targetRepository(act.Target)
	if !ok {
		return nil, nil
	}
	return stores.Repos.GetByRefSystem(ctx, info.OrgID, domain.RepoRef{Host: act.Scope, Owner: owner, Repo: repo})
}

// targetRepository reads the owner/repo a GitHub act's target leads with,
// "owner/repo" or "owner/repo#N".
func targetRepository(target string) (owner, repo string, ok bool) {
	slug, _, _ := strings.Cut(target, "#")
	owner, repo, ok = strings.Cut(slug, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

// recordActionMemoryLink links the conversation to the entity the external
// action is about (resolveActionReferences), with the role actionMemoryRole
// gives the action: produced for an action that names the object it changed,
// touched for any other. The funnel runs only after the write landed, so the
// link is recorded as the change lands. A nil action (an audit-only write with
// no external action) links nothing; an action whose entity did not resolve is
// counted as a skipped link of its role. See recordEntityTouch for the
// best-effort + outside-the-tx contract.
func recordActionMemoryLink(ctx context.Context, stores db.Stores, info ConversationInfo, act *domain.ExternalAction, about resolvedEntity) {
	if act == nil || !about.attempted {
		return
	}
	role := actionMemoryRole(*act)
	if about.err != nil {
		memoryentities.CountSkippedLink(ctx, info.OrgID, role, act.Provider, about.err)
		return
	}
	recordMemoryLink(ctx, stores, info, act.Provider, about.entityID, role)
}

// actionMemoryRole is the memory role a funnel write gives the entity its
// action is about: produced when the action names the object it changed
// (domain.ObjectID), touched otherwise — a refusal, a failure, a catch-all row,
// or one whose object's identity did not resolve.
func actionMemoryRole(act domain.ExternalAction) string {
	if _, ok := domain.ObjectID(act); ok {
		return domain.MemoryRoleProduced
	}
	return domain.MemoryRoleTouched
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
// Scoped to kind=pull_request deliberately. A submitted-review deliverable shares
// the owner/repo#N target shape, and reviewing someone else's PR is the clearest
// case of touching an entity you must not come to own.
//
// Ordering against the poller does not matter. The entity is the one the
// deliverable was linked to (resolveDeliverableEntity), which resolves the pull
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
// the router's remaining tiers for good. A pull request deliverable whose entity
// did not resolve has nothing to stamp.
func stampPRAttribution(ctx context.Context, stores db.Stores, info ConversationInfo, a *domain.Deliverable) {
	if a == nil || a.Provider != domain.ProviderGitHub || a.Kind != domain.DeliverableKindPullRequest {
		return
	}
	// Defensive: auto-fire is gated on an owned task and deliverables.team_id is
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
