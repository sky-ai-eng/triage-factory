package routing

import (
	"context"
	"encoding/json"
	"log"
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
// (SKY-375, GitHub) and team↔project gate (SKY-376, Jira). It is the
// security teeth that keeps a team's handlers from firing on entities the
// team doesn't track once polling goes org-wide.
//
// Escape hatches return true (no drop):
//   - System/org-union handlers (NULL team_id) — they're scoped to the
//     org-wide union by construction; the event's entity is in the union
//     because *some* team tracks it, so gating them would always pass
//     anyway.
//   - The relevant tracking store unwired (nil) — callers from before the
//     gate / tests that don't exercise it; degrades to the pre-ticket
//     behavior where every team implicitly tracked every org-global
//     entity. Handled per-source in teamTracksEventScope.
//   - Any source other than github:/jira: — ungated (no tracking concept).
//
// The per-event result is memoized in cache, keyed by team id. A single
// event has one source/entity, so the repo and project lookups for a
// given team collapse to one cache entry.
func (r *Router) handlerScopeMatchesEvent(evt domain.Event, h domain.EventHandler, cache map[string]bool) bool {
	if h.TeamID == "" {
		return true
	}
	if allowed, ok := cache[h.TeamID]; ok {
		return allowed
	}
	allowed := r.teamTracksEventScope(evt, h.TeamID)
	cache[h.TeamID] = allowed
	return allowed
}

// teamTracksEventScope dispatches the tracking lookup on the event's
// source: github: → team↔repo (SKY-375), jira: → team↔project (SKY-376).
// Each branch fails open when its store is unwired so the gate degrades
// to "no drop" in pre-ticket / test wiring; any other source is ungated.
func (r *Router) teamTracksEventScope(evt domain.Event, teamID string) bool {
	switch {
	case strings.HasPrefix(evt.EventType, "github:"):
		if r.teamRepos == nil {
			return true
		}
		return r.teamTracksEventRepo(evt, teamID)
	case strings.HasPrefix(evt.EventType, "jira:"):
		if r.jiraRules == nil {
			return true
		}
		return r.teamTracksEventProject(evt, teamID)
	default:
		return true
	}
}

// teamTracksEventRepo extracts the repo from a GitHub event's metadata
// and asks the store whether teamID tracks it. Every GitHub PR metadata
// struct carries a top-level "repo" ("owner/name"), so a minimal
// unmarshal is enough — no per-type decoding. Fail-open on a missing /
// malformed repo or a store error: dropping a legitimate task on a
// transient DB blip or an unexpected metadata shape is worse than the
// pre-ticket behavior, and the events feeding this path come from TF's
// own trusted poller.
func (r *Router) teamTracksEventRepo(evt domain.Event, teamID string) bool {
	var m struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Repo == "" {
		return true
	}
	owner, name, ok := strings.Cut(m.Repo, "/")
	if !ok || owner == "" || name == "" {
		return true
	}
	tracks, err := r.teamRepos.TracksRepoSystem(context.Background(), teamID, owner, name)
	if err != nil {
		log.Printf("[router] team↔repo gate lookup failed for team %s repo %s: %v — allowing", teamID, m.Repo, err)
		return true
	}
	return tracks
}

// teamTracksEventProject extracts the Jira project key from an event's
// metadata and asks the store whether teamID tracks it (SKY-376). Every
// Jira issue metadata struct carries a top-level "project" (the project
// key, e.g. "SKY" — see internal/domain/events/jira.go), so a minimal
// unmarshal is enough. Fail-open on a missing / malformed project or a
// store error: dropping a legitimate task on a transient DB blip or an
// unexpected metadata shape is worse than the pre-ticket behavior, and
// the events feeding this path come from TF's own trusted poller.
func (r *Router) teamTracksEventProject(evt domain.Event, teamID string) bool {
	var m struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Project == "" {
		return true
	}
	tracks, err := r.jiraRules.TracksProjectSystem(context.Background(), teamID, m.Project)
	if err != nil {
		log.Printf("[router] team↔project gate lookup failed for team %s project %s: %v — allowing", teamID, m.Project, err)
		return true
	}
	return tracks
}

// handlerTeamID resolves the team a matched event_handler routes its
// tasks to. Post-SKY-295 every handler is team-scoped — user-source
// rows carry the user's team, system-source rows are materialized
// into each team at boot / team-create time. An empty TeamID here
// indicates a pre-SKY-295 org-visibility row that survived a partial
// migration or a test fixture that bypassed the materialization
// path; we log it once per call but fall back to
// runmode.LocalDefaultTeamID so the router keeps functioning. In
// steady state this branch is unreachable.
func handlerTeamID(h domain.EventHandler) string {
	if h.TeamID != "" {
		return h.TeamID
	}
	log.Printf("[router] WARNING handler %s (%s) has no team_id — falling back to LocalDefaultTeamID; check that EventHandlerStore.Seed was called with a real teamID", h.ID, h.Kind)
	return runmode.LocalDefaultTeamID
}

// maxRuleDefaultPriority returns the highest default_priority across the
// matched rules, defaulting to the 0.5 trigger-fallback when none carry one.
// Used for review_requested, whose owner/visibility teams come from the
// requested identity rather than the rule's team — so the per-owner-team
// teamScore can't supply the priority; the matched rule(s) do.
func maxRuleDefaultPriority(rules []domain.EventHandler) float64 {
	s := 0.5
	for _, rule := range rules {
		if rule.DefaultPriority != nil && *rule.DefaultPriority > s {
			s = *rule.DefaultPriority
		}
	}
	return s
}

// reviewRequestVisibilityTeams resolves the team(s) a review_requested task is
// visible to from the *requested identity* on the event metadata — not from
// which teams' rules matched. A requested user routes to the union of teams
// over every TF user the login resolves to (the set-valued reverse lookup, the
// regression guard for a login bound to two users); a requested github team
// routes to the TF team(s) the github-team→TF-team mapping returns.
//
// scoped=true means "route via the requested-identity path"; the caller uses
// the returned teams (an empty set → drop the task: a TF-known reviewer that
// maps to no team is a config gap, not a reason to over-fan to handler teams).
// scoped=false means "fall back to handler-team visibility" — returned for
// legacy events that carry no requested identity, or when the required stores
// are unwired (test / pre-ticket wiring). Claims-free ...System lookups
// throughout: the router runs on the eventbus subscriber goroutine with no
// JWT context.
func (r *Router) reviewRequestVisibilityTeams(orgID string, evt domain.Event) (teams []string, scoped bool) {
	var meta events.GitHubPRReviewRequestedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return nil, false
	}
	if meta.RequestedLogin == "" && meta.RequestedTeam == "" {
		return nil, false // legacy event, no requested identity captured
	}

	// Unwired mapping store is test / pre-ticket wiring only — fall back to
	// handler-team visibility (scoped=false) rather than dropping the task, so
	// pre-existing review_requested fixtures that don't thread these stores
	// keep working. Production always wires them, so a wired-but-unresolvable
	// identity below takes the scoped=true path (drop, no mis-attributed task).
	set := map[string]struct{}{}
	if meta.RequestedTeam != "" {
		if r.githubGroups == nil {
			return nil, false
		}
		orgLogin, slug, ok := strings.Cut(meta.RequestedTeam, "/")
		if !ok || orgLogin == "" || slug == "" {
			return nil, true // malformed handle → no team
		}
		tids, err := r.githubGroups.TeamsForGroupSystem(context.Background(), orgID, orgLogin, slug)
		if err != nil {
			log.Printf("[router] review_requested: github-team mapping lookup for %s: %v", meta.RequestedTeam, err)
			return nil, true
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	} else {
		if r.users == nil || r.teams == nil || r.orgs == nil {
			return nil, false
		}
		orgSet, err := r.orgs.GetSettingsSystem(context.Background(), orgID)
		if err != nil {
			log.Printf("[router] review_requested: read org settings for host: %v", err)
			return nil, true
		}
		// Resolve to the effective host (empty → github.com) so the reverse
		// lookup matches identities captured under the canonical host (the
		// OAuth login-claim binds to github.com literally); the raw empty
		// setting would look up host="" and drop the task.
		host := dbpkg.EffectiveGitHubHost(orgSet.GitHubBaseURL)
		userIDs, err := r.users.UserIDsForGitHubLoginSystem(context.Background(), host, meta.RequestedLogin)
		if err != nil {
			log.Printf("[router] review_requested: reverse login lookup for %s: %v", meta.RequestedLogin, err)
			return nil, true
		}
		for _, uid := range userIDs {
			tids, terr := r.teams.TeamIDsForUserInOrgSystem(context.Background(), orgID, uid)
			if terr != nil {
				log.Printf("[router] review_requested: teams for user %s: %v", uid, terr)
				continue
			}
			for _, t := range tids {
				set[t] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, true
}

// authorCentricOwner runs the owning-team ladder for an author-centric github
// event (first hit wins). It returns the single owning team plus the set of
// teams the ladder produced:
//
//	owner == team, ownerSet == {team}   — a structural owner (tier 1 override
//	                                       or tier 2 project), a prior owned
//	                                       author-centric task (tier 3), or a
//	                                       single-team author (tier 4);
//	owner == "",   ownerSet == {A,B,…}  — the author maps to multiple teams
//	                                       (tier 4 ambiguous): no single owner,
//	                                       visible to all of them, NULL owner;
//	owner == "",   ownerSet == nil      — nothing resolved (external/non-TF
//	                                       author): no task unless an explicit
//	                                       watch rule pulls a team in.
//
// Claims-free ...System lookups throughout: the router runs on the eventbus
// goroutine with no JWT context.
func (r *Router) authorCentricOwner(orgID string, evt domain.Event, entityID string) (owner string, ownerSet []string) {
	// Tiers 1+2 — structural owner (owning_team_id override, else a
	// team-visibility project's team). One store query.
	if r.entities != nil {
		if t, err := r.entities.OwningTeamForEntitySystem(context.Background(), orgID, entityID); err != nil {
			log.Printf("[router] author-centric owner: structural lookup for entity %s: %v", entityID, err)
		} else if t != "" {
			return t, []string{t}
		}
	}

	// Tier 3 — the most recent prior owned author-centric task on the entity.
	// review_requested is excluded by construction (not in the type set) and
	// NULL-owned priors are excluded by the store's team_id filter, so this
	// can't fall into the review-first trap or be anchored by an unowned task.
	if r.tasks != nil {
		if t, err := r.tasks.OwnerTeamForLatestTaskInTypesSystem(context.Background(), orgID, entityID, authorCentricGitHubEventTypes); err != nil {
			log.Printf("[router] author-centric owner: prior-task lookup for entity %s: %v", entityID, err)
		} else if t != "" {
			return t, []string{t}
		}
	}

	// Tier 4 — the PR author's identity → TF user(s) → teams.
	teams := r.authorTeams(orgID, evt)
	switch len(teams) {
	case 0:
		return "", nil
	case 1:
		return teams[0], teams
	default:
		return "", teams
	}
}

// authorTeams resolves a github event's PR author login to the union of teams
// over every TF user the login maps to, on the org's effective github host.
// Mirrors reviewRequestVisibilityTeams' user-identity branch (the set-valued
// reverse lookup is the regression guard for a login bound to two users).
// Returns an empty slice when the author isn't a TF user (dependabot /
// external) or the identity stores are unwired.
func (r *Router) authorTeams(orgID string, evt domain.Event) []string {
	if r.users == nil || r.teams == nil || r.orgs == nil {
		return nil
	}
	var m struct {
		Author string `json:"author"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Author == "" {
		return nil
	}
	orgSet, err := r.orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		log.Printf("[router] author-centric owner: read org settings for host: %v", err)
		return nil
	}
	// Effective host (empty → github.com) so the reverse lookup matches
	// identities captured under the canonical host — the raw empty setting
	// would look up host="" and resolve nobody.
	host := dbpkg.EffectiveGitHubHost(orgSet.GitHubBaseURL)
	userIDs, err := r.users.UserIDsForGitHubLoginSystem(context.Background(), host, m.Author)
	if err != nil {
		log.Printf("[router] author-centric owner: reverse login lookup for %s: %v", m.Author, err)
		return nil
	}
	set := map[string]struct{}{}
	for _, uid := range userIDs {
		tids, terr := r.teams.TeamIDsForUserInOrgSystem(context.Background(), orgID, uid)
		if terr != nil {
			log.Printf("[router] author-centric owner: teams for user %s: %v", uid, terr)
			continue
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// assigneeCentricJiraOwner runs the owning-team ladder for an assignee-centric
// jira event (first hit wins) — the Jira twin of authorCentricOwner. It returns
// the single owning team plus the set of teams the ladder produced, with the
// same NULL-owner semantics:
//
//	owner == team, ownerSet == {team}   — a structural owner (tier 1
//	                                       owning_team_id override), a prior
//	                                       owned assignee-centric task (tier 3),
//	                                       or a single-team assignee (tier 4);
//	owner == "",   ownerSet == {A,B,…}  — the assignee maps to multiple teams
//	                                       (tier 4 ambiguous): no single owner,
//	                                       visible to all of them, NULL owner;
//	owner == "",   ownerSet == nil      — nothing resolved (the assignee isn't a
//	                                       TF user, or the issue is unassigned):
//	                                       no task unless an explicit watch rule
//	                                       pulls a team in.
//
// Tiers 1 and 3 are provider-agnostic and shared with authorCentricOwner; only
// the identity tier (4) differs — the Jira assignee account id instead of the
// PR author login. Tier 2 (a team-visibility project's team) is DELIBERATELY
// ABSENT: Jira projects are multi-team-tracked via jira_project_status_rules,
// so a project confers no single owner — the assignee identity is the owner
// signal. OwningTeamForEntitySystem (tier 1) would also return a team-visibility
// project's team, but Jira entities are never attached to a `projects` row (that
// table pins GitHub repos; the classifier assigns it for GitHub entities only),
// so its project sub-tier is inert here and only the owning_team_id override can
// fire.
//
// Claims-free ...System lookups throughout: the router runs on the eventbus
// goroutine with no JWT context.
func (r *Router) assigneeCentricJiraOwner(orgID string, evt domain.Event, entityID string) (owner string, ownerSet []string) {
	// Tier 1 — structural owner (owning_team_id override). One store query.
	if r.entities != nil {
		if t, err := r.entities.OwningTeamForEntitySystem(context.Background(), orgID, entityID); err != nil {
			log.Printf("[router] assignee-centric owner: structural lookup for entity %s: %v", entityID, err)
		} else if t != "" {
			return t, []string{t}
		}
	}

	// Tier 3 — the most recent ACTIVE owned assignee-centric task on the
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
	if r.tasks != nil {
		if t := r.latestActiveOwnedTaskTeam(orgID, entityID, assigneeCentricJiraEventSet); t != "" {
			return t, []string{t}
		}
	}

	// Tier 4 — the issue assignee's identity → TF user(s) → teams.
	teams := r.assigneeTeams(orgID, evt)
	switch len(teams) {
	case 0:
		return "", nil
	case 1:
		return teams[0], teams
	default:
		return "", teams
	}
}

// assigneeTeams resolves a jira event's assignee account id to the union of
// teams over every TF user the account maps to, on the org's Jira host. The
// Jira twin of authorTeams (the set-valued reverse lookup is the regression
// guard for one account bound to two users). Returns an empty slice when the
// issue is unassigned, the assignee isn't a TF user (external collaborator), or
// the identity stores are unwired.
func (r *Router) assigneeTeams(orgID string, evt domain.Event) []string {
	if r.users == nil || r.teams == nil || r.orgs == nil {
		return nil
	}
	// Every assignee-centric jira metadata struct carries assignee_account_id
	// (the Atlassian stable identifier) — a minimal unmarshal is enough.
	var m struct {
		AssigneeAccountID string `json:"assignee_account_id"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.AssigneeAccountID == "" {
		return nil
	}
	orgSet, err := r.orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		log.Printf("[router] assignee-centric owner: read org settings for host: %v", err)
		return nil
	}
	// The reverse lookup normalizes the host (db.NormalizeJiraHost) the same
	// way the capture paths stored it, so the raw org_settings value resolves
	// the identities bound under the canonical host. An empty host (Jira not
	// configured) resolves nobody.
	userIDs, err := r.users.UserIDsForJiraAccountSystem(context.Background(), orgSet.JiraBaseURL, m.AssigneeAccountID)
	if err != nil {
		log.Printf("[router] assignee-centric owner: reverse account lookup for %s: %v", m.AssigneeAccountID, err)
		return nil
	}
	set := map[string]struct{}{}
	for _, uid := range userIDs {
		tids, terr := r.teams.TeamIDsForUserInOrgSystem(context.Background(), orgID, uid)
		if terr != nil {
			log.Printf("[router] assignee-centric owner: teams for user %s: %v", uid, terr)
			continue
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// ownerLadderRouting computes the visibility set, owner, firing order, and seed
// priority for an owning-team-ladder event (author-centric GitHub or
// assignee-centric Jira) from the ladder's (owner, ownerSet) result and the
// matched rules. Both provider branches share it so the NULL-owner and
// visibility-union semantics stay identical:
//
//   - visibility = {owner set} ∪ {teams whose explicit user-authored rule
//     matched}. System/default rules gate creation + the owner's automation but
//     never grant visibility on their own; a team widens visibility beyond
//     ownership only by opting in with its own rule (the deliberate watch);
//   - ownerTeam is "" when the identity maps to multiple teams (or to none with
//     a watch rule): the owner is unresolved, stored as NULL — the auto-fire
//     gate for free, resolving on the first human claim;
//   - only the owner's automation fires. A resolved owner fires its own matched
//     triggers (acting = owner); a NULL owner fires nothing (the empty-team
//     gate), so the bot never claims an unowned task.
//
// ok=false means the visibility set is empty (external identity, no watching
// rule) → no task; the caller returns without creating one.
func ownerLadderRouting(owner string, ownerSet []string, matchedRules []domain.EventHandler) (visibleTeams []string, ownerTeam string, orderedTeams []string, taskPriority float64, ok bool) {
	vis := map[string]struct{}{}
	for _, t := range ownerSet {
		vis[t] = struct{}{}
	}
	for t := range explicitUserRuleTeams(matchedRules) {
		vis[t] = struct{}{}
	}
	if len(vis) == 0 {
		return nil, "", nil, 0, false
	}
	if owner != "" {
		orderedTeams = []string{owner}
	}
	return sortedKeys(vis), owner, orderedTeams, maxRuleDefaultPriority(matchedRules), true
}

// latestActiveOwnedTaskTeam returns the owning team of the most recently
// created ACTIVE task on the entity whose event_type is in types and whose
// owner (team_id) is non-NULL, or "" when none qualifies. It is the
// active-only tier-3 anchor for the assignee-centric Jira ladder — the
// reassignable-entity counterpart to the all-statuses
// OwnerTeamForLatestTaskInTypesSystem the GitHub ladder uses. Claims-free
// (...System); the router runs on the eventbus goroutine with no JWT context.
func (r *Router) latestActiveOwnedTaskTeam(orgID, entityID string, types map[string]bool) string {
	active, err := r.tasks.FindActiveByEntitySystem(context.Background(), orgID, entityID)
	if err != nil {
		log.Printf("[router] assignee-centric owner: active-task lookup for entity %s: %v", entityID, err)
		return ""
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
	return best
}

// explicitUserRuleTeams returns the teams whose matched handler is a
// user-authored rule — the only handlers that widen an author-centric task's
// visibility beyond the entity owner. System/default rules gate creation and
// the owner's automation but never grant visibility on their own, so the
// scoping stays the owner unless a team deliberately opts in.
func explicitUserRuleTeams(matchedRules []domain.EventHandler) map[string]struct{} {
	out := map[string]struct{}{}
	for _, h := range matchedRules {
		if h.Source == domain.EventHandlerSourceUser {
			out[handlerTeamID(h)] = struct{}{}
		}
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
