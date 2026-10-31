package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// handlerScopeMatchesEvent reports whether handler h's team is allowed
// to act on evt given the team's tracking scope — the team↔repo gate
// (GitHub), team↔project gate (Jira) and team↔Linear-team gate (Linear). It
// is the security teeth that keeps a team's handlers from firing on entities
// the team doesn't track once polling goes org-wide.
//
// Escape hatches return true (no drop):
//   - System/org-union handlers (NULL team_id) — they're scoped to the
//     org-wide union by construction; the event's entity is in the union
//     because *some* team tracks it, so gating them would always pass
//     anyway.
//   - The relevant tracking store unwired (nil) — callers from before the
//     gate / tests that don't exercise it; degrades to the pre-ticket
//     behavior where every team implicitly tracked every org-global
//     entity. Handled per source in teamScopeVerdict, whose undecided answer
//     the open posture reads as "tracks".
//   - Any source other than github:/jira:/linear: — its registered
//     TracksScope hook, or ungated when it has none.
//
// The per-event result is memoized in cache, keyed by team id. A single
// event has one source/entity, so the repo and project lookups for a
// given team collapse to one cache entry.
func (r *Router) handlerScopeMatchesEvent(ctx context.Context, evt domain.Event, h domain.EventHandler, cache teamScopeCache) bool {
	if h.TeamID == "" {
		return true
	}
	return r.teamTracksEventScopeCached(ctx, evt, h.TeamID, cache)
}

// teamScope is one team's answer to the team↔scope gate for one event, in the
// two postures the router applies it in. open is task routing's: every answer
// the gate cannot prove is "tracks". strict is the notify rules': every such
// answer is "does not track", because a notification about an entity the team
// does not track is a disclosure, and a missed one costs nothing a later event
// cannot tell the person.
type teamScope struct{ open, strict bool }

// teamScopeCache memoizes teamScope per team id for a single event. matchHandlers'
// per-handler gate, the owner ladder's identity-tier gate (trackingTeams) and
// the notify gate share one cache per event (created in routeEvent), so a team
// referenced by any of them costs at most one store lookup per event. The
// empty team id is never looked up — handlerScopeMatchesEvent and the notify
// gate short-circuit it, and ladder team sets never contain it.
type teamScopeCache map[string]teamScope

func (r *Router) teamScopeCached(ctx context.Context, evt domain.Event, teamID string, cache teamScopeCache) teamScope {
	if s, ok := cache[teamID]; ok {
		return s
	}
	s := r.scopeFor(ctx, evt, teamID)
	cache[teamID] = s
	return s
}

// teamTracksEventScopeCached is the open answer through cache.
func (r *Router) teamTracksEventScopeCached(ctx context.Context, evt domain.Event, teamID string, cache teamScopeCache) bool {
	return r.teamScopeCached(ctx, evt, teamID, cache).open
}

// errScopeUndecided is the team↔scope gate's "could not tell": a store that
// is not wired, or metadata or an entity that names nothing to look up. A read
// error is returned as itself.
var errScopeUndecided = errors.New("routing: the team-scope gate could not decide")

// errNoCoreGate is teamScopeVerdict's answer for a source core has no gate
// for: a registered event source's TracksScope hook answers for it, and only
// for task routing, since a hook's bool cannot say it was unsure.
var errNoCoreGate = errors.New("routing: no core team-scope gate for this source")

// scopeFor reads teamScopeVerdict once and derives both postures from it:
// github: → team↔repo, jira: → team↔project, linear: → team↔Linear-team, a
// registered event source (e.g. ee/slack) → its TracksScope hook, and any
// other source ungated. A source with no core gate is never strict. The open
// posture only narrows a set someone else computed, so failing open costs a
// briefly-too-wide set that the next event corrects (see teamTracksEventRepo),
// where failing closed would drop a legitimate task on a transient read error.
func (r *Router) scopeFor(ctx context.Context, evt domain.Event, teamID string) teamScope {
	tracks, err := r.teamScopeVerdict(ctx, evt, teamID)
	switch {
	case err == nil:
		return teamScope{open: tracks, strict: tracks}
	case errors.Is(err, errNoCoreGate):
		if hooks, ok := sourceHooksFor(evt.EventType); ok {
			return teamScope{open: hooks.TracksScope(ctx, evt, teamID)}
		}
		return teamScope{open: true}
	}
	return teamScope{open: true}
}

// teamScopeVerdict is the team↔scope gate with its uncertainty kept: tracks is
// an answer only when err is nil. The sources it gates are the ones
// domain.NotifyRuleSource admits notify rules on; every other source is
// errNoCoreGate.
func (r *Router) teamScopeVerdict(ctx context.Context, evt domain.Event, teamID string) (bool, error) {
	switch {
	case strings.HasPrefix(evt.EventType, "github:"):
		if r.teamRepos == nil {
			return false, fmt.Errorf("%w: no repository tracking store", errScopeUndecided)
		}
		return r.teamTracksEventRepo(ctx, evt, teamID)
	case strings.HasPrefix(evt.EventType, "jira:"):
		if r.jiraRules == nil {
			return false, fmt.Errorf("%w: no Jira tracking store", errScopeUndecided)
		}
		return r.teamTracksEventProject(ctx, evt, teamID)
	case strings.HasPrefix(evt.EventType, "linear:"):
		if r.linearRules == nil {
			return false, fmt.Errorf("%w: no Linear tracking store", errScopeUndecided)
		}
		return r.teamTracksEventLinearTeam(ctx, evt, teamID)
	}
	return false, fmt.Errorf("%w: %s", errNoCoreGate, evt.EventType)
}

// teamTracksEventRepo asks the store whether teamID tracks the repository of
// the pull request a GitHub event is about: the registry row its entity
// references (entities.repository_id). The id is what the team's tracked set
// references too, so the gate reads no name: a repository renamed since the
// event was emitted is still the one the team tracks, and a registry row lives
// on one GitHub host, so a team tracking acme/api on one host is not tracking
// the acme/api of another. An entity that cannot be read, or one that names no
// repository, is undecided.
//
// Task routing reads undecided as "tracks" (scopeFor's open posture): dropping a
// legitimate task on a transient DB blip is worse than the pre-ticket
// behavior, and the events feeding this path come from TF's own trusted
// poller. That is the deliberate posture split from the owning-team ladder
// next door, where a failed read propagates instead of degrading. The gate only
// ever REMOVES teams from a set someone else already computed, so failing open
// costs a briefly-too-wide gate that the next event corrects; the ladder's
// reads DECIDE the owner, and a wrong answer there sticks to the entity for the
// rest of its life.
func (r *Router) teamTracksEventRepo(ctx context.Context, evt domain.Event, teamID string) (bool, error) {
	if evt.EntityID == nil {
		return false, fmt.Errorf("%w: no entity to read", errScopeUndecided)
	}
	e, err := r.entities.GetSystem(ctx, evt.OrgID, *evt.EntityID)
	if err != nil {
		routerLog.Warn("team-repo gate entity read failed; undecided", "team_id", teamID, "entity_id", *evt.EntityID, "error", err)
		return false, err
	}
	if e == nil || e.RepositoryID == "" {
		return false, fmt.Errorf("%w: the entity names no repository", errScopeUndecided)
	}
	tracks, err := r.teamRepos.TracksRepositorySystem(ctx, teamID, e.RepositoryID)
	if err != nil {
		routerLog.Warn("team-repo gate lookup failed; undecided", "team_id", teamID, "repository_id", e.RepositoryID, "error", err)
		return false, err
	}
	return tracks, nil
}

// githubEventHost is the GitHub host a GitHub event is about: the "host" its
// metadata names when it carries one (github:pr:unreachable does, because it is
// emitted for a pull request on a host the org has left), else the scope of the
// entity it is about — the host the pull request was polled from — else the
// org's current host, for an event with neither.
func (r *Router) githubEventHost(ctx context.Context, evt domain.Event) (string, error) {
	var m struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err == nil && m.Host != "" {
		return m.Host, nil
	}
	if evt.EntityID != nil {
		e, err := r.entities.GetSystem(ctx, evt.OrgID, *evt.EntityID)
		if err != nil {
			return "", fmt.Errorf("read entity for github host: %w", err)
		}
		if e != nil && e.Source == "github" && e.Scope != "" {
			return e.Scope, nil
		}
	}
	return dbpkg.OrgGitHubHostSystem(ctx, r.orgs, evt.OrgID)
}

// teamTracksEventProject reads the Jira project an event is about off its
// metadata — jira_project_id, the jira_projects registry row the tracker
// stamps on every Jira event beside the project's display key (see
// internal/domain/events/jira.go) — and asks the store whether teamID tracks
// that row on the org's current Jira site. A project key rename moves no
// reference, so a renamed project's events match its teams whatever key they
// carry. A missing or malformed id is undecided, and a store error is
// returned: task routing reads both as "tracks", for the reason
// teamTracksEventRepo gives (an event recorded before the id rode the
// metadata names none).
//
// key_changed is the one event about two projects, the one the issue is in
// now and the one it left (old_jira_project_id), and it passes for a team that
// tracks either. A team whose issue moved to a project it does not track keeps
// its tasks on the entity, and this event is the only thing that tells it the
// issue left; a handler filtered on old_jira_project_id_in could never fire
// otherwise.
// Every other Jira event routes on its project alone.
func (r *Router) teamTracksEventProject(ctx context.Context, evt domain.Event, teamID string) (bool, error) {
	var m struct {
		JiraProjectID    string `json:"jira_project_id"`
		OldJiraProjectID string `json:"old_jira_project_id"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.JiraProjectID == "" {
		return false, fmt.Errorf("%w: the event names no Jira project", errScopeUndecided)
	}
	projects := []string{m.JiraProjectID}
	if evt.EventType == domain.EventJiraIssueKeyChanged && m.OldJiraProjectID != "" && m.OldJiraProjectID != m.JiraProjectID {
		projects = append(projects, m.OldJiraProjectID)
	}
	settings, err := r.orgs.GetSettingsSystem(ctx, evt.OrgID)
	if err != nil {
		routerLog.Warn("team-project gate: org settings read failed; undecided", "team_id", teamID, "jira_project_id", m.JiraProjectID, "error", err)
		return false, err
	}
	site := domain.EntityScope("jira", settings)
	for _, project := range projects {
		tracks, err := r.jiraRules.TracksJiraProjectSystem(ctx, teamID, site, project)
		if err != nil {
			routerLog.Warn("team-project gate lookup failed; undecided", "team_id", teamID, "jira_project_id", project, "error", err)
			return false, err
		}
		if tracks {
			return true, nil
		}
	}
	return false, nil
}

// teamTracksEventLinearTeam reads the Linear team id off an event's metadata
// and asks the store whether teamID tracks it in the org's current Linear
// workspace. The gate keys on the UUID rather than the team key, because a key
// can be renamed in Linear and the id cannot; and it reads only the current
// workspace's rules, because a rule saved under another workspace names a team
// no event from this one can carry. Malformed metadata is undecided and a
// store error is returned, as teamTracksEventRepo describes.
//
// identifier_changed is the one event about two Linear teams, the one the
// issue is in now and the one it left (old_linear_team_id), and it passes for a
// team that tracks either. A team whose issue moved to a Linear team it does
// not track keeps its tasks on the entity, and this event is the only thing
// that tells it the issue left; a handler filtered on old_linear_team_id_in
// could never fire otherwise.
//
// Metadata that parses but names no team is refused rather than allowed. The
// tracker sets linear_team_id on every Linear event from a fresh read of the
// issue or, on an unreachable one, from the entity's own team column, which a
// source pause does not clear. It is empty only on an unreachable issue whose
// row predates that column and has nothing else to name a team, so no team can
// be shown to track it. The event still closes the entity's tasks; the close
// relations do not read this gate.
func (r *Router) teamTracksEventLinearTeam(ctx context.Context, evt domain.Event, teamID string) (bool, error) {
	var m struct {
		LinearTeamID    string `json:"linear_team_id"`
		OldLinearTeamID string `json:"old_linear_team_id"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
		return false, fmt.Errorf("%w: unreadable Linear metadata", errScopeUndecided)
	}
	linearTeams := make([]string, 0, 2)
	if m.LinearTeamID != "" {
		linearTeams = append(linearTeams, m.LinearTeamID)
	}
	if evt.EventType == domain.EventLinearIssueIdentifierChanged && m.OldLinearTeamID != "" && m.OldLinearTeamID != m.LinearTeamID {
		linearTeams = append(linearTeams, m.OldLinearTeamID)
	}
	if len(linearTeams) == 0 {
		return false, nil
	}
	settings, err := r.orgs.GetSettingsSystem(ctx, evt.OrgID)
	if err != nil {
		routerLog.Warn("team-linear-team gate: org settings read failed; undecided", "team_id", teamID, "linear_team_id", m.LinearTeamID, "error", err)
		return false, err
	}
	scope := domain.EntityScope("linear", settings)
	for _, linearTeamID := range linearTeams {
		tracks, err := r.linearRules.TracksTeamSystem(ctx, teamID, scope, linearTeamID)
		if err != nil {
			routerLog.Warn("team-linear-team gate lookup failed; undecided", "team_id", teamID, "linear_team_id", linearTeamID, "error", err)
			return false, err
		}
		if tracks {
			return true, nil
		}
	}
	return false, nil
}

// trackingTeams filters an identity-derived team set — the owner ladder's
// tier-3 result (a PR author's / Jira assignee's member teams) — down to the
// teams that actually track the event's entity. It applies the same team↔repo
// (GitHub) / team↔project (Jira) gate matchHandlers applies to handlers, closing
// the one path that reached the visibility/owner set ungated: a team the author
// merely belongs to but that tracks none of this entity's repo would otherwise
// enter the set as an ambiguous co-owner, land in task_teams, and surface the
// entity on that team's board even though the team could never act on it (the
// task's automation can't fire, and a member can't claim work for a repo the
// team doesn't track).
//
// Input order is preserved (callers pass an already-deterministic set). Shares
// the event's scope cache with matchHandlers via teamTracksEventScopeCached, so a
// team already gated as a handler isn't re-queried when it also appears in the
// author's/assignee's team set. Fails open per team — an unwired tracking store,
// malformed metadata, or a transient store error keeps the team — so the gate is
// inert in exactly the wiring where handlerScopeMatchesEvent is (pre-config /
// tests that don't thread the tracking stores), and local N=1 is unaffected (the
// sole team tracks every configured repo).
func (r *Router) trackingTeams(ctx context.Context, evt domain.Event, teams []string, cache teamScopeCache) []string {
	if len(teams) == 0 {
		return teams
	}
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		if r.teamTracksEventScopeCached(ctx, evt, t, cache) {
			out = append(out, t)
		}
	}
	return out
}

// handlerTeamID resolves the team a matched event_handler routes its
// tasks to. Every handler is team-scoped — user-source
// rows carry the user's team, system-source rows are materialized
// into each team at boot / team-create time. An empty TeamID here
// indicates a legacy org-visibility row that survived a partial
// migration or a test fixture that bypassed the materialization
// path; we log it once per call but fall back to
// runmode.LocalDefaultTeamID so the router keeps functioning. In
// steady state this branch is unreachable.
func handlerTeamID(h domain.EventHandler) string {
	if h.TeamID != "" {
		return h.TeamID
	}
	routerLog.Warn("handler has no team_id, falling back to LocalDefaultTeamID; check that EventHandlerStore.Seed was called with a real teamID", "handler_id", h.ID, "kind", h.Kind)
	return runmode.LocalDefaultTeamID
}

// maxRuleDefaultPriority returns the highest default_priority across the
// matched rules, defaulting to the 0.5 trigger-fallback when none carry one.
// Used for the requested-party events (review_requested, mentioned), whose
// owner/visibility teams come from the identity the event names rather than
// the rule's team — so the per-owner-team teamRulePriorityScores can't supply
// the priority; the matched rule(s) do.
func maxRuleDefaultPriority(rules []domain.EventHandler) float64 {
	s := 0.5
	for _, rule := range rules {
		if rule.DefaultPriority != nil && *rule.DefaultPriority > s {
			s = *rule.DefaultPriority
		}
	}
	return s
}

// requestedParty reads the identity a requested-party event names: the
// requested reviewer of a review_requested, the mentioned user or team of a
// mentioned. At most one of login and team is set, team in "org/slug" form,
// and id is GitHub's id for that user or team — empty for a mention, which
// names a handle only, and for a request recorded before ids were. Metadata
// that does not decode names nobody.
func requestedParty(evt domain.Event) (login, team, id string) {
	if evt.EventType == domain.EventGitHubPRMentioned {
		var meta events.GitHubPRMentionedMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
			return "", "", ""
		}
		return meta.MentionedLogin, meta.MentionedTeam, ""
	}
	var meta events.GitHubPRReviewRequestedMetadata
	if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
		return "", "", ""
	}
	if meta.RequestedTeam != "" {
		return "", meta.RequestedTeam, meta.RequestedTeamID
	}
	return meta.RequestedLogin, "", meta.RequestedUserID
}

// requestedPartyTeams resolves the team(s) a requested-party task
// (review_requested, mentioned) is visible to from the identity on the event
// metadata — not from which teams' rules matched. A user routes to the union
// of teams over every TF user the identity resolves to (the set-valued reverse
// lookup, the regression guard for an account bound to two users); a github
// team routes to the TF team(s) the github-team→TF-team mapping returns. Both
// resolve by GitHub's id when the event carries one, so a renamed user or team
// still routes, and by handle when it does not (a mention, or an event
// recorded before ids were).
//
// scoped=true means "route via the requested-identity path"; the caller uses
// the returned teams (an empty set → drop the task: a TF-known identity that
// maps to no team is a config gap, not a reason to over-fan to handler teams).
// scoped=false means "fall back to handler-team visibility" — returned for
// legacy events that carry no identity, or when the required stores are
// unwired (test / pre-ticket wiring). Claims-free ...System lookups
// throughout: the router runs on the eventbus subscriber goroutine with no
// JWT context.
//
// err != nil means "could not find out" and is distinct from every ok outcome
// above. It matters because scoped-and-empty DROPS the task: a config gap and
// a failed identity read are indistinguishable in the returned set, so a
// degraded read would silently spend the event on a task that was never
// created. The caller propagates so the queue row is retried instead.
func (r *Router) requestedPartyTeams(ctx context.Context, orgID string, evt domain.Event) (teams []string, scoped bool, err error) {
	login, team, ghID := requestedParty(evt)
	if login == "" && team == "" {
		return nil, false, nil // legacy event, no identity captured
	}

	// Unwired mapping store is test / pre-ticket wiring only — fall back to
	// handler-team visibility (scoped=false) rather than dropping the task, so
	// pre-existing fixtures that don't thread these stores keep working.
	// Production always wires them, so a wired-but-unresolvable identity below
	// takes the scoped=true path (drop, no mis-attributed task).
	set := map[string]struct{}{}
	if team != "" {
		if r.githubGroups == nil {
			return nil, false, nil
		}
		orgLogin, slug, ok := strings.Cut(team, "/")
		if ghID == "" && (!ok || orgLogin == "" || slug == "") {
			return nil, true, nil // malformed handle and no id → no team
		}
		// The GitHub team lives on the pull request's host; a mapping saved
		// for the same org login on another host names another organization.
		host, err := r.githubEventHost(ctx, evt)
		if err != nil {
			routerLog.Error("requested party: resolve github host failed", "event_type", evt.EventType, "team", team, "error", err)
			return nil, false, fmt.Errorf("github host for %s: %w", team, err)
		}
		tids, err := r.githubGroups.TeamsForGitHubTeamSystem(ctx, orgID, host, ghID, orgLogin, slug)
		if err != nil {
			routerLog.Error("requested party: github-team mapping lookup failed", "event_type", evt.EventType, "team", team, "error", err)
			return nil, false, fmt.Errorf("github-team mapping for %s: %w", team, err)
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	} else {
		if r.users == nil {
			return nil, false, nil
		}
		orgSet, err := r.orgs.GetSettingsSystem(ctx, orgID)
		if err != nil {
			routerLog.Error("requested party: read org settings for host failed", "event_type", evt.EventType, "error", err)
			return nil, false, fmt.Errorf("read org settings for github host: %w", err)
		}
		// The canonical host, the one identities are captured under. An org
		// with no GitHub URL has no host and resolves nobody.
		host := dbpkg.EffectiveGitHubHost(orgSet.GitHubBaseURL)
		userIDs, err := r.usersForGitHubAccount(ctx, host, ghID, login)
		if err != nil {
			routerLog.Error("requested party: reverse identity lookup failed", "event_type", evt.EventType, "login", login, "github_user_id", ghID, "error", err)
			return nil, false, fmt.Errorf("reverse identity lookup for %s: %w", login, err)
		}
		// Collect rather than bail on the first failure, the fireMatchedTriggers
		// pattern: one unreadable user must not decide the shape of the answer,
		// and the joined error names every user the set is missing.
		var errs []error
		for _, uid := range userIDs {
			tids, terr := r.teams.TeamIDsForUserInOrgSystem(ctx, orgID, uid)
			if terr != nil {
				routerLog.Error("requested party: teams for user lookup failed", "event_type", evt.EventType, "user_id", uid, "error", terr)
				errs = append(errs, fmt.Errorf("teams for user %s: %w", uid, terr))
				continue
			}
			for _, t := range tids {
				set[t] = struct{}{}
			}
		}
		if len(errs) > 0 {
			return nil, false, errors.Join(errs...)
		}
	}

	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, true, nil
}

// authorCentricOwner runs the owning-team ladder for an author-centric github
// event (first hit wins). It returns the single owning team plus the set of
// teams the ladder produced:
//
//	owner == team, ownerSet == {team}   — a structural owner (tier 1
//	                                       owning_team_id override), a prior
//	                                       owned author-centric task (tier 2),
//	                                       or a single-team author (tier 3);
//	owner == "",   ownerSet == {A,B,…}  — the author maps to multiple teams
//	                                       (tier 3 ambiguous): no single owner,
//	                                       visible to all of them, NULL owner;
//	owner == "",   ownerSet == nil      — nothing resolved (external/non-TF
//	                                       author, or no member team tracks the
//	                                       entity's repo): no task unless an
//	                                       explicit watch rule pulls a team in.
//
// A non-nil error is none of those three: it means the ladder could not find
// out, and the caller must route nothing rather than pick an answer. Every tier
// propagates, because each one's failure mode is a demotion — a failed tier 1 or
// 2 read used to fall through to the tier below, quietly handing the entity to
// whichever team the NEXT rung named, and a failed tier 3 read used to look
// exactly like an external author. Both outcomes stick: tier 2 anchors every
// later event on the entity to whatever the first one landed on.
//
// The identity tier (tier 3) is gated to teams that track the entity's repo (see
// trackingTeams), so a team the author belongs to but which tracks nothing never
// becomes an owner candidate or lands in the visibility set. Tiers 1–2 (a
// structural override owner, a prior owned task) are already repo-relevant
// by construction and stay ungated — matching the forward-only tracking-change
// rule, which never retroactively reassigns an already-owned entity.
//
// Claims-free ...System lookups throughout: the router runs on the eventbus
// goroutine with no JWT context.
func (r *Router) authorCentricOwner(ctx context.Context, orgID string, evt domain.Event, entityID string, scopeCache teamScopeCache) (owner string, ownerSet []string, err error) {
	// Tier 1 — structural owner (owning_team_id override). One store query.
	t, err := r.entities.OwningTeamForEntitySystem(ctx, orgID, entityID)
	if err != nil {
		routerLog.Error("author-centric owner: structural lookup failed", "entity_id", entityID, "error", err)
		return "", nil, fmt.Errorf("structural owner for entity %s: %w", entityID, err)
	}
	if t != "" {
		return t, []string{t}, nil
	}

	// Tier 2 — the most recent prior owned author-centric task on the entity.
	// review_requested and mentioned are excluded by construction (not in the
	// type set) and
	// NULL-owned priors are excluded by the store's team_id filter, so this
	// can't fall into the review-first trap or be anchored by an unowned task.
	t, err = r.tasks.OwnerTeamForLatestTaskInTypesSystem(ctx, orgID, entityID, authorCentricGitHubEventTypes)
	if err != nil {
		routerLog.Error("author-centric owner: prior-task lookup failed", "entity_id", entityID, "error", err)
		return "", nil, fmt.Errorf("prior-task owner for entity %s: %w", entityID, err)
	}
	if t != "" {
		return t, []string{t}, nil
	}

	// Tier 3 — the PR author's identity → TF user(s) → teams, gated to the teams
	// that actually track this repo. Without the gate a team the author merely
	// belongs to (e.g. an unconfigured team that tracks no repos) would enter the
	// set as an ambiguous co-owner and surface the entity on that team's board,
	// even though it can never act on the repo — the queue-column leak this gate
	// closes. If the gate empties the set (no member team tracks the repo) the
	// author is treated like an external one: no member owner, deferring to any
	// explicit watch handler exactly as an external/non-TF author would.
	authored, err := r.authorTeams(ctx, orgID, evt)
	if err != nil {
		return "", nil, err
	}
	teams := r.trackingTeams(ctx, evt, authored, scopeCache)
	switch len(teams) {
	case 0:
		return "", nil, nil
	case 1:
		return teams[0], teams, nil
	default:
		return "", teams, nil
	}
}

// authorTeams resolves a github event's PR author to the union of teams over
// every TF user the author maps to, on the org's effective github host: by the
// author's GitHub id when the event carries one, by login when it does not (an
// event recorded before ids were). Mirrors requestedPartyTeams' user-identity
// branch (the set-valued reverse lookup is the regression guard for an account
// bound to two users).
// Returns an empty slice when the author isn't a TF user (dependabot /
// external) or the router has no Users store — both resolved data states, not
// failures.
//
// A store failure returns an error instead of an empty set, because the two are
// otherwise the same answer: "no member owns this PR" is what hands the entity
// to an applies_to_unowned watcher, and the watcher's fire consolidates
// ownership onto itself permanently. A router built without a Users store is
// wiring, not an outage, so it still answers empty.
func (r *Router) authorTeams(ctx context.Context, orgID string, evt domain.Event) ([]string, error) {
	if r.users == nil {
		return nil, nil
	}
	var m struct {
		Author   string `json:"author"`
		AuthorID string `json:"author_id"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || (m.Author == "" && m.AuthorID == "") {
		return nil, nil
	}
	orgSet, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		routerLog.Error("author-centric owner: read org settings for host failed", "error", err)
		return nil, fmt.Errorf("read org settings for github host: %w", err)
	}
	// The canonical host, the one identities are captured under. An org with
	// no GitHub URL has no host and resolves nobody.
	host := dbpkg.EffectiveGitHubHost(orgSet.GitHubBaseURL)
	userIDs, err := r.usersForGitHubAccount(ctx, host, m.AuthorID, m.Author)
	if err != nil {
		routerLog.Error("author-centric owner: reverse identity lookup failed", "author", m.Author, "author_id", m.AuthorID, "error", err)
		return nil, fmt.Errorf("reverse identity lookup for %s: %w", m.Author, err)
	}
	// Collect rather than drop the user and carry on: a partial team set is a
	// wrong answer that looks like a right one (one team instead of two reads
	// as an unambiguous owner), so every failure has to reach the caller.
	set := map[string]struct{}{}
	var errs []error
	for _, uid := range userIDs {
		tids, terr := r.teams.TeamIDsForUserInOrgSystem(ctx, orgID, uid)
		if terr != nil {
			routerLog.Error("author-centric owner: teams for user lookup failed", "user_id", uid, "error", terr)
			errs = append(errs, fmt.Errorf("teams for user %s: %w", uid, terr))
			continue
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return sortedKeys(set), nil
}

// assigneeCentricOwner runs the owning-team ladder for an assignee-centric
// jira or linear event (first hit wins) — the twin of authorCentricOwner.
// ownedTypes is the source's assignee-centric type set, which tier 2 anchors
// on. It returns the single owning team plus the set of teams the ladder
// produced, with the same NULL-owner semantics:
//
//	owner == team, ownerSet == {team}   — a structural owner (tier 1
//	                                       owning_team_id override), a prior
//	                                       owned assignee-centric task (tier 2),
//	                                       or a single-team assignee (tier 3);
//	owner == "",   ownerSet == {A,B,…}  — the assignee maps to multiple teams
//	                                       (tier 3 ambiguous): no single owner,
//	                                       visible to all of them, NULL owner;
//	owner == "",   ownerSet == nil      — nothing resolved (the assignee isn't a
//	                                       TF user, the issue is unassigned, or no
//	                                       member team tracks the entity's project):
//	                                       no task unless an explicit watch rule
//	                                       pulls a team in.
//
// As with the GitHub author ladder, the identity tier (tier 3) is gated to teams
// that track the entity's Jira project (trackingTeams); tiers 1 and 2 stay
// ungated (already project-relevant, forward-only).
//
// Tiers 1 and 2 are provider-agnostic and shared with authorCentricOwner; only
// the identity tier (3) differs — the issue assignee's account instead of the
// PR author login. OwningTeamForEntitySystem (tier 1) resolves only the
// owning_team_id override; Jira projects and Linear teams are multi-team-
// tracked, so neither confers a single owner and the assignee identity is the
// owner signal instead.
//
// A non-nil error means the ladder could not find out, exactly as in
// authorCentricOwner — see there for why a degraded read is worse than a
// retried event.
//
// Claims-free ...System lookups throughout: the router runs on the eventbus
// goroutine with no JWT context.
func (r *Router) assigneeCentricOwner(ctx context.Context, orgID string, evt domain.Event, entityID string, ownedTypes map[string]bool, scopeCache teamScopeCache) (owner string, ownerSet []string, err error) {
	// Tier 1 — structural owner (owning_team_id override). One store query.
	t, err := r.entities.OwningTeamForEntitySystem(ctx, orgID, entityID)
	if err != nil {
		routerLog.Error("assignee-centric owner: structural lookup failed", "entity_id", entityID, "error", err)
		return "", nil, fmt.Errorf("structural owner for entity %s: %w", entityID, err)
	}
	if t != "" {
		return t, []string{t}, nil
	}

	// Tier 2 — the most recent ACTIVE owned assignee-centric task on the
	// entity. Unlike GitHub (a PR's authorship is stable for life, so a closed
	// CI task still anchors its owner), a Jira issue is reassignable: a CLOSED
	// prior task — e.g. one the reassignment close-check just retired when the
	// issue left its owner — must NOT anchor the new assignee's event, or a
	// reassignment to another member (or to a non-member) would keep minting
	// tasks for the stale owner. Only a still-active owned task anchors, which
	// gives related events on a stably-assigned issue (a comment whose
	// metadata omits the assignee, an ambiguous-but-pinned owner) the same
	// consistency GitHub gets without re-attaching to retired ownership. NULL-
	// owned active tasks are skipped (an unresolved owner can't anchor).
	t, err = r.latestActiveOwnedTaskTeam(ctx, orgID, entityID, ownedTypes)
	if err != nil {
		return "", nil, fmt.Errorf("prior-task owner for entity %s: %w", entityID, err)
	}
	if t != "" {
		return t, []string{t}, nil
	}

	// Tier 3 — the issue assignee's identity → TF user(s) → teams, gated to the
	// teams that actually track this Jira project (same reason as the GitHub
	// author ladder: a team the assignee merely belongs to but that tracks none
	// of this project must not become a co-owner in the visibility set). An empty
	// gated set falls through to the external-assignee semantics below.
	assigned, err := r.assigneeTeams(ctx, orgID, evt)
	if err != nil {
		return "", nil, err
	}
	teams := r.trackingTeams(ctx, evt, assigned, scopeCache)
	switch len(teams) {
	case 0:
		return "", nil, nil
	case 1:
		return teams[0], teams, nil
	default:
		return "", teams, nil
	}
}

// assigneeTeams resolves an issue event's assignee to the union of teams over
// every TF user the assignee maps to — a Jira account on the org's Jira host,
// or a Linear user in the org's Linear workspace. The twin of authorTeams (the
// set-valued reverse lookup is the regression guard for one account bound to
// two users). Returns an empty slice when the issue is unassigned, the
// assignee isn't a TF user (external collaborator), or the router has no
// Users store — all resolved data states. A store failure returns an error
// instead, for the reason authorTeams documents.
func (r *Router) assigneeTeams(ctx context.Context, orgID string, evt domain.Event) ([]string, error) {
	if r.users == nil {
		return nil, nil
	}
	var userIDs []string
	var err error
	if eventSourcePrefix(evt.EventType) == "linear" {
		userIDs, err = r.linearAssigneeUsers(ctx, orgID, evt)
	} else {
		userIDs, err = r.jiraAssigneeUsers(ctx, orgID, evt)
	}
	if err != nil || len(userIDs) == 0 {
		return nil, err
	}
	set := map[string]struct{}{}
	var errs []error
	for _, uid := range userIDs {
		tids, terr := r.teams.TeamIDsForUserInOrgSystem(ctx, orgID, uid)
		if terr != nil {
			routerLog.Error("assignee-centric owner: teams for user lookup failed", "user_id", uid, "error", terr)
			errs = append(errs, fmt.Errorf("teams for user %s: %w", uid, terr))
			continue
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return sortedKeys(set), nil
}

// linearAssigneeUsers resolves a linear event's assignee_user_id to the TF
// users bound to it in the org's Linear workspace. The workspace is the
// org's, learned from its Linear credential; with none bound there is no
// member identity to resolve, so nobody is returned.
func (r *Router) linearAssigneeUsers(ctx context.Context, orgID string, evt domain.Event) ([]string, error) {
	var m struct {
		AssigneeUserID string `json:"assignee_user_id"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.AssigneeUserID == "" {
		return nil, nil
	}
	orgSet, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		routerLog.Error("assignee-centric owner: read org settings for linear workspace failed", "error", err)
		return nil, fmt.Errorf("read org settings for linear workspace: %w", err)
	}
	userIDs, err := r.users.UserIDsForLinearAccountSystem(ctx, orgSet.LinearWorkspaceID, m.AssigneeUserID)
	if err != nil {
		routerLog.Error("assignee-centric owner: reverse linear account lookup failed", "assignee_user_id", m.AssigneeUserID, "error", err)
		return nil, fmt.Errorf("reverse linear account lookup for %s: %w", m.AssigneeUserID, err)
	}
	return userIDs, nil
}

// jiraAssigneeUsers resolves a jira event's assignee_account_id to the TF
// users bound to it on the org's Jira host.
func (r *Router) jiraAssigneeUsers(ctx context.Context, orgID string, evt domain.Event) ([]string, error) {
	// Every assignee-centric jira metadata struct carries assignee_account_id
	// (the Atlassian stable identifier) — a minimal unmarshal is enough.
	var m struct {
		AssigneeAccountID string `json:"assignee_account_id"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.AssigneeAccountID == "" {
		return nil, nil
	}
	orgSet, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		routerLog.Error("assignee-centric owner: read org settings for host failed", "error", err)
		return nil, fmt.Errorf("read org settings for jira host: %w", err)
	}
	// The reverse lookup normalizes the host (db.NormalizeJiraHost) the same
	// way the capture paths stored it, so the raw org_settings value resolves
	// the identities bound under the canonical host.
	//
	// org_settings.jira_base_url is the org's Jira site and the only host key:
	// Jira identity capture (handleJiraIdentityPAT) refuses to bind unless
	// jira_base_url resolves to a valid host (jira.CanonicalHost), so every
	// user_jira_identities row is keyed under NormalizeJiraHost(jira_base_url).
	// An empty host (Jira not configured for identity) therefore resolves
	// nobody, correctly: there are no member identities to route to.
	userIDs, err := r.users.UserIDsForJiraAccountSystem(ctx, orgSet.JiraBaseURL, m.AssigneeAccountID)
	if err != nil {
		routerLog.Error("assignee-centric owner: reverse account lookup failed", "assignee_account_id", m.AssigneeAccountID, "error", err)
		return nil, fmt.Errorf("reverse jira account lookup for %s: %w", m.AssigneeAccountID, err)
	}
	return userIDs, nil
}

// ownerLadderRouting computes the visibility set, owner, firing order, and seed
// priority for an owning-team-ladder event (author-centric GitHub or
// assignee-centric Jira / Linear) from the ladder's (owner, ownerSet) result and the
// matched rules. Both provider branches share it so the NULL-owner and
// visibility-union semantics stay identical:
//
//   - visibility = {owner set} ∪ {teams whose matched handler set
//     applies_to_unowned=true}. A flag-off handler gates creation + the owner's
//     automation but never grants visibility on its own; a team widens
//     visibility beyond ownership only by opting in with the explicit
//     applies_to_unowned flag (TFAC-517 — the deliberate watch);
//   - ownerTeam is "" when the identity maps to multiple teams (or to none with
//     a watch rule): the owner is unresolved, stored as NULL — resolving on the
//     fire or the first human claim;
//   - firing order has three cases, all preserving the TFAC-514 no-steal
//     invariant — a non-member watcher NEVER beats a member team to ownership:
//     1. resolved owner → orderedTeams = {owner}. Watchers see the card only.
//     2. ambiguous MEMBER ownership (identity on multiple member teams) →
//     orderedTeams = ownerSet ranked. Watchers are NOT candidates while any
//     member team exists; the first member to win the exclusive claim
//     consolidates the NULL owner onto itself.
//     3. no member owns it (ownerSet empty — a truly external author:
//     dependabot, renovate, an outside-fork contributor) → orderedTeams =
//     the opted-in watcher teams ranked (TFAC-517). A watcher that set
//     applies_to_unowned (on a rule OR a trigger) AND has a configured
//     auto-delegation may now fire the orphan PR and consolidate ownership
//     onto itself — the deliberate, eyes-open behavior that supersedes the
//     TFAC-514 blanket "external never fires." A watcher whose opt-in is only
//     a rule (no trigger) surfaces the card NULL-owned and nothing fires.
//
// ok=false means the visibility set is empty (external identity, no watching
// handler) → no task; the caller returns without creating one.
//
// matchedRules and matchedTriggers are both consumed for the watch set: the
// applies_to_unowned reach flag is per-handler, so a trigger can opt a team into
// orphan reach (and, since it's a trigger, firing) on its own — see
// explicitWatchTeams. Priority ranking still reads default_priority off the rules
// (triggers carry none; a trigger-only watcher falls back to the 0.5 default in
// teamRulePriorityScores).
func ownerLadderRouting(owner string, ownerSet []string, matchedRules, matchedTriggers []domain.EventHandler) (visibleTeams []string, ownerTeam string, orderedTeams []string, taskPriority float64, ok bool) {
	vis := map[string]struct{}{}
	for _, t := range ownerSet {
		vis[t] = struct{}{}
	}
	watchTeams := explicitWatchTeams(matchedRules, matchedTriggers)
	for t := range watchTeams {
		vis[t] = struct{}{}
	}
	if len(vis) == 0 {
		return nil, "", nil, 0, false
	}
	switch {
	case owner != "":
		// Unambiguous owner: only the owner's automation fires. Watcher teams
		// (in vis via an applies_to_unowned handler) see the card but don't fire.
		orderedTeams = []string{owner}
	case len(ownerSet) > 0:
		// Ambiguous MEMBER ownership (the author maps to multiple member teams):
		// fire deterministically rather than not at all — but only among the
		// IDENTITY's teams (ownerSet), NOT the full visibility set. A watcher
		// (non-member, in vis via an applies_to_unowned handler but not in
		// ownerSet) is NEVER an ownership candidate while a member team exists —
		// otherwise a pure watcher could win the claim and consolidate itself as
		// owner of a PR a member owns (the TFAC-514 no-steal invariant). Order
		// ownerSet by the same signal the handler-team path uses — max matched-rule
		// DefaultPriority desc, then lowest team id — so the first member to win
		// the exclusive claim is deterministic and the fire consolidates the NULL
		// owner onto it (the firing transaction's owner consolidation).
		orderedTeams = orderTeamsByRulePriority(setOf(ownerSet), matchedRules)
	default:
		// No member owns it (ownerSet empty — a truly external author:
		// dependabot, renovate, an outside-fork contributor). The orphan task's
		// only claimants are the teams that explicitly opted into reaching
		// unowned entities (an applies_to_unowned rule OR trigger) — they may now
		// fire their configured auto-delegations and the first to claim
		// consolidates ownership onto itself (TFAC-517, superseding the TFAC-514
		// blanket "external never fires"). This runs ONLY when no member team is a
		// candidate, so the member-over-watcher invariant holds. Firing still
		// requires a matched trigger + auto_delegate enabled downstream: a watcher
		// whose opt-in is a trigger fires that trigger; a watcher whose opt-in is
		// only a rule (no trigger) yields an empty teamTriggers and nothing fires —
		// the card stays NULL-owned.
		orderedTeams = orderTeamsByRulePriority(watchTeams, matchedRules)
	}
	return sortedKeys(vis), owner, orderedTeams, maxRuleDefaultPriority(matchedRules), true
}

// teamRulePriorityScores computes, for each team in vis, the per-team ranking
// signal for the deterministic firing order: the max DefaultPriority across that
// team's matched rules, falling back to the 0.5 trigger-default when the team
// contributed only triggers (no rule carries a priority). Built in a SINGLE pass
// over matchedRules so handlerTeamID — which logs a warning on a legacy/fixture
// empty team_id — runs once per rule, never once per sort comparison (a
// comparator calling it would log O(n log n) times per event). Shared by the
// handler-team path (resolveTeamRouting) and the ambiguous owner-ladder path
// (orderTeamsByRulePriority) so both rank teams the same way.
func teamRulePriorityScores(vis map[string]struct{}, matchedRules []domain.EventHandler) map[string]float64 {
	scores := make(map[string]float64, len(vis))
	for t := range vis {
		scores[t] = 0.5
	}
	for _, rule := range matchedRules {
		tid := handlerTeamID(rule)
		if cur, ok := scores[tid]; ok && rule.DefaultPriority != nil && *rule.DefaultPriority > cur {
			scores[tid] = *rule.DefaultPriority
		}
	}
	return scores
}

// orderTeamsByScores sorts teams into the deterministic firing order from a
// precomputed score map: priority desc, ties broken by lowest team id. The sort
// reads only the map, so it does no per-comparison work beyond a lookup.
func orderTeamsByScores(teams []string, scores map[string]float64) []string {
	sort.SliceStable(teams, func(i, j int) bool {
		if scores[teams[i]] != scores[teams[j]] {
			return scores[teams[i]] > scores[teams[j]]
		}
		return teams[i] < teams[j]
	})
	return teams
}

// orderTeamsByRulePriority orders a visibility set into the deterministic firing
// order (max matched-rule DefaultPriority desc, ties broken by lowest team id).
// It mirrors the handler-team path's ranking (resolveTeamRouting) so an
// ambiguous owner-ladder task (NULL owner, multiple visible teams) fires in the
// same order a multi-team handler-team task would — the first team wins the
// exclusive claim and consolidates the owner via the fire.
func orderTeamsByRulePriority(vis map[string]struct{}, matchedRules []domain.EventHandler) []string {
	return orderTeamsByScores(sortedKeys(vis), teamRulePriorityScores(vis, matchedRules))
}

// latestActiveOwnedTaskTeam returns the owning team of the most recently
// created ACTIVE task on the entity whose event_type is in types and whose
// owner (team_id) is non-NULL, or "" when none qualifies. It is the
// active-only tier-2 anchor for the assignee-centric Jira ladder — the
// reassignable-entity counterpart to the all-statuses
// OwnerTeamForLatestTaskInTypesSystem the GitHub ladder uses. A store failure
// returns an error rather than "" so the ladder is not demoted a rung by a
// blip. Claims-free (...System); the router runs on the eventbus goroutine with
// no JWT context.
func (r *Router) latestActiveOwnedTaskTeam(ctx context.Context, orgID, entityID string, types map[string]bool) (string, error) {
	active, err := r.tasks.FindActiveByEntitySystem(ctx, orgID, entityID)
	if err != nil {
		routerLog.Error("assignee-centric owner: active-task lookup failed", "entity_id", entityID, "error", err)
		return "", fmt.Errorf("list active tasks: %w", err)
	}
	best := ""
	var bestAt time.Time
	for i := range active {
		t := &active[i]
		if t.TeamID == nil || *t.TeamID == "" || !types[t.EventType] {
			continue
		}
		if best == "" || t.CreatedAt.After(bestAt) {
			best, bestAt = *t.TeamID, t.CreatedAt
		}
	}
	return best, nil
}

// explicitWatchTeams returns the teams whose matched handler opted into
// reaching unowned entities (applies_to_unowned=true) — the only handlers that
// widen an owner-ladder task's visibility beyond the entity owner. A flag-off
// handler gates creation and the owner's automation but never grants visibility
// on its own, so the scoping stays the owner unless a team deliberately opts in
// with this explicit per-handler flag (TFAC-517 — replacing the prior
// source==user heuristic).
//
// It is a per-HANDLER flag, not a rule-only one: both kinds create tasks, so the
// reach concept applies to triggers too. Pass every matched handler group (rules
// AND triggers); a trigger that opts in lets a team configure orphan reach +
// auto-delegation entirely on the prompts page, no companion rule needed. A
// watcher always gets visibility; it gains firing rights only in
// ownerLadderRouting's external branch (ownerSet empty, no member owner), never
// while any member team is an ownership candidate (the TFAC-514 no-steal
// invariant).
func explicitWatchTeams(handlerGroups ...[]domain.EventHandler) map[string]struct{} {
	out := map[string]struct{}{}
	for _, group := range handlerGroups {
		for _, h := range group {
			if h.AppliesToUnowned {
				out[handlerTeamID(h)] = struct{}{}
			}
		}
	}
	return out
}

// setOf collects a slice into a set, dropping duplicates. Used to turn the
// ladder's ownerSet slice into the keyed form orderTeamsByRulePriority consumes.
func setOf(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, it := range items {
		out[it] = struct{}{}
	}
	return out
}

// sortedKeys returns the map's keys in ascending order — used to make the
// task_teams write order deterministic.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// teamIDValue derefs a task's nullable owning team to "" when the owner is
// unresolved (team_id NULL), so the router's string-keyed comparisons and the
// effectiveActingTeam/auto-fire gates read a NULL owner as the empty team.
func teamIDValue(t *domain.Task) string {
	if t == nil || t.TeamID == nil {
		return ""
	}
	return *t.TeamID
}

// teamIDPtr maps a team id back to the nullable column convention: "" → nil
// (unresolved owner), a real team → a pointer to it.
func teamIDPtr(teamID string) *string {
	if teamID == "" {
		return nil
	}
	return &teamID
}

// effectiveActingTeam normalizes a handler's acting team for the
// auto-delegate gates and the bot claim. An org-visible handler routes
// the LocalDefaultTeamID sentinel through handlerTeamID, but in
// multi-mode that sentinel has no teams / team_agents / team_settings
// row of its own — the store resolves it to the org's canonical team
// for the task's owner and visibility rows. The task's owner team_id
// already carries that resolution, so fall back to it. In local mode
// the sentinel IS the real team and task.TeamID equals it, so this is a
// no-op. Without this, multi-mode org-visible auto-delegation reads the
// sentinel's (missing) team_agents row and is wrongly skipped.
func effectiveActingTeam(actingTeamID, taskTeamID string) string {
	if actingTeamID == "" || actingTeamID == runmode.LocalDefaultTeamID {
		return taskTeamID
	}
	return actingTeamID
}

// usersForGitHubAccount is the reverse identity lookup for a GitHub actor an
// event names: the TF users bound to GitHub's id for the account when the event
// carries one (UserIDsForGitHubUserIDSystem, which also takes a binding that
// recorded no id by its login), and to its login when it does not.
func (r *Router) usersForGitHubAccount(ctx context.Context, host, githubUserID, login string) ([]string, error) {
	if githubUserID != "" {
		return r.users.UserIDsForGitHubUserIDSystem(ctx, host, githubUserID, login)
	}
	return r.users.UserIDsForGitHubLoginSystem(ctx, host, login)
}
