package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

const (
	jiraBatchSize = 100 // max issues per JQL key IN (...) query

	// descriptionStoreMaxRunes caps what we persist on entities.description.
	// Jira descriptions are unbounded (teams regularly paste multi-KB specs,
	// stack traces, etc.); storing them raw would bloat the column for no
	// current benefit — the scorer already truncates at 1500 runes for the
	// LLM prompt, so 2000 gives a small buffer while keeping rows compact.
	// If a future UI wants to render the full body it should re-fetch from
	// Jira directly rather than relying on this mirror.
	descriptionStoreMaxRunes = 2000
)

// Tracker manages the discover → refresh → diff → emit cycle for both
// GitHub and Jira. In the entity-first model, the tracker:
//   - creates/updates entities (not tasks — that's routing's job)
//   - diffs entity snapshots to produce per-action events
//   - publishes events to the bus (recording is routing's job)
//   - does NOT create or update tasks
type Tracker struct {
	database *sql.DB
	bus      *eventbus.Bus
	tasks    db.TaskStore   // SKY-283: tracker creates review_requested tasks during discovery + reconciles stale ones
	entities db.EntityStore // SKY-284: entity lifecycle (find/create, snapshot, title/description, close/reactivate)
	repos    db.RepoStore   // per-repo conditional-request (ETag) state for GitHub open-PR discovery
	// orgID is the tenant this tracker emits events and reads/writes
	// entities for. Set at construction and stable for the Tracker's
	// lifetime; the poller's per-org loop constructs a fresh Tracker
	// per tenant per cycle. Local mode passes runmode.LocalDefaultOrgID;
	// multi mode passes the iterated active org.
	//
	// TODO: a future GitHub-App-credentials change (see SKY-263) will
	// also bundle the per-org GitHub client + bot username on this
	// struct — today those are method parameters because credentials
	// are process-global.
	orgID string
}

// New creates a Tracker bound to one tenant. The poller's per-org
// loop calls this once per active org per cycle; the resulting
// Tracker handles all event-emission for that org and stamps every
// published event with the tenant via publish() below.
func New(database *sql.DB, bus *eventbus.Bus, tasks db.TaskStore, entities db.EntityStore, repos db.RepoStore, orgID string) *Tracker {
	return &Tracker{database: database, bus: bus, tasks: tasks, entities: entities, repos: repos, orgID: orgID}
}

// publish stamps evt.OrgID with the tracker's configured tenant before
// forwarding to the bus so org-scoped subscribers see a tagged event.
// A pre-set evt.OrgID is left intact so future callers stamping their
// own org (carry-over, backfill in another tenant) override the
// tracker's default.
func (t *Tracker) publish(evt domain.Event) {
	if evt.OrgID == "" {
		evt.OrgID = t.orgID
	}
	t.bus.Publish(evt)
}

// --- GitHub ---

// RefreshGitHub runs the full tracking cycle for GitHub PRs. userTeams
// is the "org/slug" list of teams the session user belongs to — used to
// match team-based review requests (where the PR's reviewRequests list
// contains the team, not the user's individual login).
//
// All entity/task reads and writes are scoped to the Tracker's
// orgID (set at construction). In multi mode the poller's per-org
// loop constructs one Tracker per active org per cycle; in local
// mode there's one Tracker for the single synthetic tenant.
func (t *Tracker) RefreshGitHub(client *ghclient.Client, username string, userTeams, repos []string) (int, error) {
	orgID := t.orgID
	startedAt := time.Now()
	// Phase 1: Discovery — find new PRs and register as entities.
	// quietRepos is the set of "owner/repo" whose open-PR listing returned
	// 304 (unchanged) this cycle; their tracked entities can keep their
	// stored snapshot through the Phase-2 gate without a refresh.
	discovered, quietRepos, err := t.discoverGitHub(client, username, repos)
	if err != nil {
		log.Printf("[tracker] GitHub discovery error: %v", err)
	}

	// Build a SourceID-keyed lookup of discovery snapshots so Phase 2 can
	// gate refresh on (updatedAt, headSHA) without a second round-trip.
	// Discovery already returns both fields via prDiscoveryFragment; the
	// only cost here is the map allocation.
	discoveredBySourceID := make(map[string]domain.PRSnapshot, len(discovered))
	for _, d := range discovered {
		discoveredBySourceID[ghSourceID(d.Snapshot.Repo, d.Snapshot.Number)] = d.Snapshot
	}

	for _, d := range discovered {
		// Ensure the NodeID is stored in the snapshot so entity-based refresh
		// can extract it without a separate column.
		snap := d.Snapshot
		snap.NodeID = d.NodeID

		sid := ghSourceID(snap.Repo, snap.Number)
		entity, created, err := t.entities.FindOrCreateSystem(context.Background(), orgID, "github", sid, "pr", snap.Title, snap.URL)
		if err != nil {
			log.Printf("[tracker] error creating entity for %s: %v", sid, err)
			continue
		}

		if created {
			// Seed the discovery snapshot.
			snapJSON, _ := json.Marshal(snap)
			if err := t.entities.UpdateSnapshotSystem(context.Background(), orgID, entity.ID, string(snapJSON)); err != nil {
				log.Printf("[tracker] failed to seed snapshot for %s: %v", sid, err)
			}
			// If the PR is already terminal, mark the entity closed immediately
			// so it doesn't sit in the active refresh set forever (Phase 3
			// won't emit a merged/closed event because prev==curr).
			if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
				if err := t.entities.MarkClosedSystem(context.Background(), orgID, entity.ID); err != nil {
					log.Printf("[tracker] failed to mark entity %s closed on discovery: %v", sid, err)
				}
			} else if snap.Author != username && isReviewerMatch(snap.ReviewRequests, username, userTeams) {
				// Backfill: user is a pending reviewer (directly or via one
				// of their teams) on a just-discovered open PR.
				// DiffPRSnapshots' "no events on initial load" rule means
				// pr:review_requested would never fire for requests that
				// existed before we started watching — the user would only see
				// them if someone re-requested. Synthesize the event + queued
				// task directly so existing review-requests land in the queue
				// on first connect. Mirrors the Jira carry-over queue path in
				// handleJiraStockPost.
				//
				// Self-authored PRs are skipped: GitHub forbids self-requests,
				// so the only way the match fires here is via a team the user
				// is on (CODEOWNERS auto-assigning them to their own PR). That
				// isn't an ask — surfacing it as a queued task pollutes the
				// queue. Matches the same guard in DiffPRSnapshots.
				if err := t.backfillReviewRequested(entity.ID, snap); err != nil {
					log.Printf("[tracker] failed to backfill review_requested for %s: %v", sid, err)
				}
			}
		} else {
			// Update title if changed.
			if entity.Title != snap.Title {
				_ = t.entities.UpdateTitleSystem(context.Background(), orgID, entity.ID, snap.Title)
			}
			// Reactivate if a previously-closed entity reappears as open
			// (e.g., reopened PR).
			if !snap.Merged && snap.State != "CLOSED" && snap.State != "MERGED" && entity.State == "closed" {
				if reactivated, err := t.entities.ReactivateSystem(context.Background(), orgID, entity.ID); err != nil {
					log.Printf("[tracker] error reactivating %s: %v", sid, err)
				} else if reactivated {
					log.Printf("[tracker] reactivated entity %s (reopened)", sid)
				}
			}
		}
	}

	// Phase 2: Refresh active entities.
	entities, err := t.entities.ListActiveSystem(context.Background(), orgID, "github")
	if err != nil {
		return 0, fmt.Errorf("list active github entities: %w", err)
	}

	// Classify by snapshot state (open vs terminal) for query cost tiering.
	// Open entities also pass through the updatedAt-gate using the discovery
	// snapshot we already have in hand — quiet PRs (unchanged updatedAt and
	// SHA, no in-flight CI) skip the refresh entirely. See gate.go for the
	// safety reasoning. Terminal items always refresh because the set is
	// small and the cheap fragment is used; gate doesn't apply.
	type entityWithSnap struct {
		entity domain.Entity
		snap   domain.PRSnapshot
		nodeID string
	}
	var openItems, terminalItems []entityWithSnap
	skippedOpen := 0

	for _, e := range entities {
		var snap domain.PRSnapshot
		if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
			_ = json.Unmarshal([]byte(e.SnapshotJSON), &snap)
		}
		if snap.NodeID == "" {
			continue // can't refresh without a node ID
		}

		item := entityWithSnap{entity: e, snap: snap, nodeID: snap.NodeID}
		if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
			terminalItems = append(terminalItems, item)
			continue
		}
		// Open path: gate against discovery's fresh snapshot if we have one.
		// Entities not in this cycle's discovery (rare — e.g. a PR you've
		// stopped being a reviewer on) fall through to refresh, which is the
		// safe default. age is "time since last full refresh" — nil pointer
		// treated as very stale so first-time skip decisions force a fetch.
		var age time.Duration
		if e.LastPolledAt != nil {
			age = time.Since(*e.LastPolledAt)
		} else {
			age = 24 * time.Hour
		}
		// Gate against discovery's fresh snapshot when we have one. A repo
		// that 304'd this cycle has no fresh snapshot, but a 304 means its
		// open-PR listing (including each PR's updated_at + head sha) is
		// byte-identical to last cycle — so the stored snapshot IS the fresh
		// state for gate purposes. Feed it back as `fresh` so quiet repos
		// keep the skip optimization the REST conditional request earns.
		fresh, ok := discoveredBySourceID[e.SourceID]
		if !ok && quietRepos[snap.Repo] {
			fresh, ok = snap, true
		}
		if ok && shouldSkipRefresh(snap, fresh, age) {
			// Skipped entities won't be diffed, so reconcile stale
			// review_requested tasks here. Entities proceeding to
			// DiffPRSnapshots emit their own review_request_removed events.
			if userTeams != nil && !isReviewerMatch(snap.ReviewRequests, username, userTeams) {
				if stale, err := t.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, e.ID, domain.EventGitHubPRReviewRequested); err == nil && len(stale) > 0 {
					meta, _ := json.Marshal(events.GitHubPRReviewRequestRemovedMetadata{
						Author:   snap.Author,
						Repo:     snap.Repo,
						PRNumber: snap.Number,
						IsDraft:  snap.IsDraft,
						HeadSHA:  snap.HeadSHA,
						Labels:   snap.Labels,
						Title:    snap.Title,
					})
					eid := e.ID
					t.publish(domain.Event{
						EventType:    domain.EventGitHubPRReviewRequestRemoved,
						EntityID:     &eid,
						MetadataJSON: string(meta),
						OccurredAt:   time.Now(),
					})
					log.Printf("[tracker] reconciled: emitting review_request_removed for skipped entity %s", e.ID)
				}
			}
			skippedOpen++
			continue
		}
		openItems = append(openItems, item)
	}

	if len(openItems) == 0 && len(terminalItems) == 0 {
		log.Printf("[tracker] GitHub refresh: %d discovered, %d entities, %d skipped (quiet), 0 refreshed, 0 events",
			len(discovered), len(entities), skippedOpen)
		if len(entities) > 0 {
			t.EmitPollComplete("github", startedAt, len(entities), 0)
		}
		return 0, nil
	}

	// Fetch fresh state — open PRs get the full fragment (includes CheckRuns).
	refreshed := make(map[string]domain.PRSnapshot)
	if len(openItems) > 0 {
		nodeIDs := make([]string, len(openItems))
		for i, item := range openItems {
			nodeIDs[i] = item.nodeID
		}
		open, err := client.RefreshPRs(nodeIDs, true)
		if err != nil {
			return 0, fmt.Errorf("refresh open PRs: %w", err)
		}
		for k, v := range open {
			refreshed[k] = v
		}
	}
	if len(terminalItems) > 0 {
		nodeIDs := make([]string, len(terminalItems))
		for i, item := range terminalItems {
			nodeIDs[i] = item.nodeID
		}
		terminal, err := client.RefreshPRs(nodeIDs, false)
		if err != nil {
			return 0, fmt.Errorf("refresh terminal PRs: %w", err)
		}
		for k, v := range terminal {
			refreshed[k] = v
		}
	}

	// Phase 3: Diff + emit events.
	allItems := append(openItems, terminalItems...)
	eventsEmitted := 0

	for _, item := range allItems {
		newSnap, ok := refreshed[item.nodeID]
		if !ok {
			continue
		}
		// Preserve NodeID through the refresh (RefreshPRs returns map[nodeID]→snap
		// but doesn't set snap.NodeID).
		newSnap.NodeID = item.nodeID

		// Diff against previous snapshot.
		events := DiffPRSnapshots(item.snap, newSnap, item.entity.ID, username, userTeams)

		// Update entity snapshot + title.
		snapJSON, _ := json.Marshal(newSnap)
		if err := t.entities.UpdateSnapshotSystem(context.Background(), orgID, item.entity.ID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating snapshot for %s: %v", item.entity.SourceID, err)
		}
		if item.entity.Title != newSnap.Title {
			_ = t.entities.UpdateTitleSystem(context.Background(), orgID, item.entity.ID, newSnap.Title)
		}

		// Publish events to bus. Recording + routing happens downstream.
		for _, evt := range events {
			t.publish(evt)
			eventsEmitted++
		}
	}

	log.Printf("[tracker] GitHub refresh: %d discovered, %d entities, %d skipped (quiet), %d refreshed, %d events",
		len(discovered), len(entities), skippedOpen, len(refreshed), eventsEmitted)

	if len(entities) > 0 {
		t.EmitPollComplete("github", startedAt, len(entities), eventsEmitted)
	}

	return eventsEmitted, nil
}

// maxSearchQueryLen is GitHub's limit for the q= search parameter.
const maxSearchQueryLen = 256

// discoverGitHub finds open PRs in the configured repo set by enumeration:
// for each repo it lists open PRs via REST (GET /pulls?state=open) with a
// conditional request keyed on the stored ETag. This gives PAT and App
// installation tokens parity of mechanism — they differ only in which repos
// the token can reach — and moves discovery onto the roomier core REST budget
// (conditional 304s are free on the primary rate limit).
//
// Returns the discovered PRs and the set of "owner/repo" that returned 304
// (unchanged open set this cycle). A 304 repo's tracked entities can keep
// their stored snapshot through the Phase-2 gate.
//
// When username is non-empty (the local/PAT perspective — App tokens have no
// "me"), it additionally runs the merged/closed 30-day dashboard backfill via
// GraphQL search to seed recent-history entities the dashboard reads. That
// backfill is inherently user-perspective and stays local/PAT-only;
// multi-mode dashboard history is out of scope.
func (t *Tracker) discoverGitHub(client *ghclient.Client, username string, repos []string) ([]ghclient.DiscoveredPR, map[string]bool, error) {
	seen := map[string]bool{}
	var all []ghclient.DiscoveredPR
	quiet := map[string]bool{}

	ctx := context.Background()

	// Phase 1a: per-repo conditional open-PR enumeration. Sequential — the
	// per-repo sweep is paced to respect GitHub's secondary (concurrency /
	// burst) limits, which a parallel fan-out would trip even though the
	// conditional 304s are free on the primary limit.
	for _, repoFull := range repos {
		owner, name := splitOwnerRepo(repoFull)
		if owner == "" || name == "" {
			continue
		}

		etag := ""
		if t.repos != nil {
			if stored, _, err := t.repos.GetPullsPollStateSystem(ctx, t.orgID, repoFull); err != nil {
				log.Printf("[tracker] read pulls poll state for %s: %v", repoFull, err)
			} else {
				etag = stored
			}
		}

		prs, newEtag, notModified, err := client.ListOpenPRs(owner, name, etag)
		if err != nil {
			// 403/404 means the token can't reach this configured repo (a
			// PAT user without access, or an App not installed on it) — skip
			// and log rather than aborting the whole sweep.
			var he *ghclient.HTTPError
			if errors.As(err, &he) && (he.StatusCode == 403 || he.StatusCode == 404) {
				log.Printf("[tracker] discovery: %s unreachable (HTTP %d) — skipping", repoFull, he.StatusCode)
				continue
			}
			log.Printf("[tracker] discovery: list open PRs for %s failed: %v", repoFull, err)
			continue
		}

		if notModified {
			quiet[repoFull] = true
			t.recordPullsPoll(ctx, repoFull, etag) // advance polled_at, keep etag
			continue
		}

		t.recordPullsPoll(ctx, repoFull, newEtag)
		for _, pr := range prs {
			sid := ghSourceID(pr.Snapshot.Repo, pr.Snapshot.Number)
			if !seen[sid] {
				seen[sid] = true
				all = append(all, pr)
			}
		}
	}

	// Phase 1b: merged/closed dashboard backfill (local/PAT-only). Seeds
	// recent-history entities via user-perspective GraphQL search. App tokens
	// have no "me", so this is skipped when username is empty.
	if username != "" {
		since := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
		bases := []string{
			fmt.Sprintf("is:pr is:merged author:%s merged:>=%s", username, since),
			fmt.Sprintf("is:pr is:merged reviewed-by:%s merged:>=%s", username, since),
			fmt.Sprintf("is:pr is:closed is:unmerged author:%s closed:>=%s", username, since),
			fmt.Sprintf("is:pr is:closed is:unmerged reviewed-by:%s closed:>=%s", username, since),
		}
		var queries []string
		for _, base := range bases {
			queries = append(queries, scopedQueries(base, repos)...)
		}
		for _, q := range queries {
			prs, err := client.DiscoverPRs(q, 50)
			if err != nil {
				log.Printf("[tracker] dashboard backfill query failed: %v (query: %s)", err, q)
				continue
			}
			for _, pr := range prs {
				sid := ghSourceID(pr.Snapshot.Repo, pr.Snapshot.Number)
				if !seen[sid] {
					seen[sid] = true
					all = append(all, pr)
				}
			}
		}
	}

	return all, quiet, nil
}

// recordPullsPoll persists the conditional-request state for a repo after a
// successful list (200 or 304). Best-effort — a write failure just means the
// next cycle re-lists unconditionally, costing one primary-limit request.
func (t *Tracker) recordPullsPoll(ctx context.Context, repoFull, etag string) {
	if t.repos == nil {
		return
	}
	if err := t.repos.SetPullsPollStateSystem(ctx, t.orgID, repoFull, etag, time.Now()); err != nil {
		log.Printf("[tracker] write pulls poll state for %s: %v", repoFull, err)
	}
}

// splitOwnerRepo splits an "owner/repo" slug at the first slash. Returns
// empty halves for a malformed entry (no slash), which the caller skips.
func splitOwnerRepo(s string) (owner, repo string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// backfillReviewRequested publishes a synthesized pr:review_requested
// event for a PR being discovered for the first time with the session
// user already in its requested-reviewer list. The router subscribes
// to the bus, evaluates rules, and fans out to per-team tasks (SKY-295).
// The task's primary_event_id FK is satisfied when the router records
// the event in its HandleEvent step 1.
//
// Pre-SKY-295 the tracker bypassed the bus and called
// tasks.FindOrCreateAt directly, which sidestepped rule evaluation —
// every backfilled task ended up assigned to "the oldest team in the
// org" regardless of which team's rule actually matched. Routing
// through the bus gives backfill the same team-aware fanout every
// other tracker-detected event already gets.
//
// The OccurredAt stamp uses the PR's CreatedAt as a lower bound:
// GitHub doesn't expose per-review-request timestamps, so PR creation
// time is the closest we have — better than "just now" on the card
// for a PR that's been pending your review for weeks. Falls back to
// time.Now() if the GraphQL timestamp is missing or unparseable.
//
// The "is this PR's review requested from me" decision happens
// upstream at the caller's matchesAny check, not here; this function
// just records the author login on the metadata so the predicate
// matcher can do its work.
func (t *Tracker) backfillReviewRequested(entityID string, snap domain.PRSnapshot) error {
	meta := events.GitHubPRReviewRequestedMetadata{
		Author:   snap.Author,
		Repo:     snap.Repo,
		PRNumber: snap.Number,
		IsDraft:  snap.IsDraft,
		HeadSHA:  snap.HeadSHA,
		Labels:   snap.Labels,
		Title:    snap.Title,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	occurredAt := time.Time{}
	if snap.CreatedAt != "" {
		if parsed, perr := time.Parse(time.RFC3339, snap.CreatedAt); perr == nil {
			occurredAt = parsed
		}
	}
	eid := entityID
	t.publish(domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRReviewRequested,
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurredAt,
	})
	return nil
}

// isReviewerMatch reports whether the session user appears in a PR's
// reviewer list, either directly (as username) or via any of their teams
// (as "org/slug"). Both the discovery backfill and the per-poll diff use
// this check — getting it wrong in either place means team-based review
// requests never surface as tasks, which is the case historically (only
// direct-to-user requests matched the old containsString(rr, username)).
func isReviewerMatch(reviewers []string, username string, userTeams []string) bool {
	if len(reviewers) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(reviewers))
	for _, r := range reviewers {
		set[r] = struct{}{}
	}
	if username != "" {
		if _, ok := set[username]; ok {
			return true
		}
	}
	for _, t := range userTeams {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}

// --- Jira ---

// JiraProjectRules is the tracker-local per-project view of the user's
// Jira status configuration. Mirrors the slice from config.JiraConfig
// but kept independent so the tracker doesn't depend on internal/config
// — call sites in the poller manager convert at the boundary.
type JiraProjectRules struct {
	Key           string
	PickupMembers []string
	DoneMembers   []string
}

// JiraRules is a slice of per-project rules with lookup helpers.
type JiraRules []JiraProjectRules

// ForKey returns the rules for the given project key, or nil when no
// matching project is configured. Callers should degrade gracefully on
// a nil return — typically by treating the event as "no rules
// configured" (no terminal check, log a warning).
func (r JiraRules) ForKey(key string) *JiraProjectRules {
	for i := range r {
		if r[i].Key == key {
			return &r[i]
		}
	}
	return nil
}

// AllDoneMembers returns the deduplicated union of every project's
// DoneMembers. Useful for subtask classification when the parent and
// subtasks may live in different projects.
func (r JiraRules) AllDoneMembers() []string {
	return r.unionMembers(func(p JiraProjectRules) []string { return p.DoneMembers })
}

func (r JiraRules) unionMembers(pick func(JiraProjectRules) []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, p := range r {
		for _, m := range pick(p) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// doneMembersForKey resolves the Done.Members for an issue key by
// looking up the project. Returns nil when the project isn't in the
// configured rule set — typically because the user removed the project
// from Settings while its entities are still active in the DB (entities
// aren't auto-deleted on settings change). Nil matches the closure in
// RefreshJira's terminal() (which returns false on unknown project)
// so discovery's "should I mark this entity closed" and the diff
// layer's "did this transition complete it" stay consistent.
//
// An earlier version fell back to the union of every configured
// project's done members, but that misclassifies entities from removed
// projects whose status happens to coincide with another project's
// "done" word (e.g. OLD-1 transitioning to "Resolved" when NEW project
// also uses "Resolved" as Done) — emits a spurious jira:issue:completed
// for an entity whose actual workflow has nothing to do with NEW's.
func (r JiraRules) doneMembersForKey(issueKey string) []string {
	if rule := r.ForKey(extractProject(issueKey)); rule != nil {
		return rule.DoneMembers
	}
	return nil
}

// RefreshJira runs the full tracking cycle for Jira issues. projects is
// the team's full per-project rule set; the tracker dispatches discovery
// JQL per project and looks up terminal-status sets by the issue's
// project_key. Tickets whose project_key has no row degrade silently
// — no terminal check, no pickup discovery.
//
// SKY-270 dropped the username parameter: actor identity now flows through
// the snapshot (assignee_account_id) and predicate matching happens
// downstream against the assignee_in / reporter_in / commenter_in
// allowlists.
//
// All entity reads/writes are scoped to the Tracker's orgID (set at
// construction). In multi mode the poller's per-org loop constructs
// one Tracker per active org per cycle; in local mode there's one
// Tracker for the single synthetic tenant.
func (t *Tracker) RefreshJira(client *jiraclient.Client, baseURL string, projects JiraRules) (int, error) {
	orgID := t.orgID
	startedAt := time.Now()
	terminal := func(snap domain.JiraSnapshot) bool {
		rule := projects.ForKey(extractProject(snap.Key))
		if rule == nil {
			return false
		}
		for _, d := range rule.DoneMembers {
			if d == snap.Status {
				return true
			}
		}
		return false
	}
	// Phase 1: Discovery
	discovered, err := t.discoverJira(client, baseURL, projects)
	if err != nil {
		log.Printf("[tracker] Jira discovery error: %v", err)
	}

	for _, state := range discovered {
		snap := state.Snap
		entity, created, err := t.entities.FindOrCreateSystem(context.Background(), orgID, "jira", snap.Key, "issue", snap.Summary, snap.URL)
		if err != nil {
			log.Printf("[tracker] error creating entity for %s: %v", snap.Key, err)
			continue
		}
		if created {
			snapJSON, _ := json.Marshal(snap)
			if err := t.entities.UpdateSnapshotSystem(context.Background(), orgID, entity.ID, string(snapJSON)); err != nil {
				log.Printf("[tracker] failed to seed snapshot for %s: %v", snap.Key, err)
			}
			if state.Description != "" {
				if err := t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, state.Description); err != nil {
					log.Printf("[tracker] failed to seed description for %s: %v", snap.Key, err)
				}
			}
			if terminal(snap) {
				if err := t.entities.MarkClosedSystem(context.Background(), orgID, entity.ID); err != nil {
					log.Printf("[tracker] failed to mark entity %s closed on discovery: %v", snap.Key, err)
				}
			}
		} else {
			if entity.Title != snap.Summary {
				_ = t.entities.UpdateTitleSystem(context.Background(), orgID, entity.ID, snap.Summary)
			}
			if entity.Description != state.Description {
				_ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, state.Description)
			}
			// Reactivate if a previously-closed issue reappears as open.
			if !terminal(snap) && entity.State == "closed" {
				if reactivated, err := t.entities.ReactivateSystem(context.Background(), orgID, entity.ID); err != nil {
					log.Printf("[tracker] error reactivating %s: %v", snap.Key, err)
				} else if reactivated {
					log.Printf("[tracker] reactivated entity %s (reopened)", snap.Key)
				}
			}
		}
	}

	// Phase 2: Refresh
	entities, err := t.entities.ListActiveSystem(context.Background(), orgID, "jira")
	if err != nil {
		return 0, fmt.Errorf("list active jira entities: %w", err)
	}
	if len(entities) == 0 {
		// No entities to refresh, but still emit poll-complete so carry-over
		// readiness flips true on fresh-setup / empty-project cases.
		t.EmitPollComplete("jira", startedAt, 0, 0)
		return 0, nil
	}

	keys := make([]string, len(entities))
	for i, e := range entities {
		keys[i] = e.SourceID
	}

	refreshed, err := t.batchFetchJira(client, baseURL, keys, projects)
	if err != nil {
		return 0, fmt.Errorf("batch fetch jira: %w", err)
	}

	// Phase 3: Diff + emit events.
	eventsEmitted := 0
	for _, e := range entities {
		newState, ok := refreshed[e.SourceID]
		if !ok {
			continue
		}
		newSnap := newState.Snap

		var prevSnap domain.JiraSnapshot
		if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
			if err := json.Unmarshal([]byte(e.SnapshotJSON), &prevSnap); err != nil {
				log.Printf("[tracker] corrupt jira snapshot for %s, reseeding: %v", e.SourceID, err)
				snapJSON, _ := json.Marshal(newSnap)
				_ = t.entities.UpdateSnapshotSystem(context.Background(), orgID, e.ID, string(snapJSON))
				continue
			}
		}

		// Per-project Done.Members for this entity's project_key. Falls
		// back to the union across all projects when the entity is in
		// a project that's no longer configured (defensive — terminal
		// detection still works for previously-known done statuses).
		events := DiffJiraSnapshots(prevSnap, newSnap, e.ID, projects.doneMembersForKey(newSnap.Key))

		snapJSON, _ := json.Marshal(newSnap)
		if err := t.entities.UpdateSnapshotSystem(context.Background(), orgID, e.ID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating jira snapshot for %s: %v", e.SourceID, err)
		}
		if e.Title != newSnap.Summary {
			_ = t.entities.UpdateTitleSystem(context.Background(), orgID, e.ID, newSnap.Summary)
		}
		// Description intentionally not updated here — batchFetchJira
		// excludes the description field to save bandwidth, so newState's
		// description would be the empty-string parse result of an absent
		// field and writing it back would wipe the stored value. Description
		// is seeded and refreshed by phase 1 (discoverJira), which is the
		// only place that actually carries the field in the response.

		for _, evt := range events {
			t.publish(evt)
			eventsEmitted++
		}
	}

	log.Printf("[tracker] Jira refresh: %d discovered, %d entities, %d refreshed, %d events",
		len(discovered), len(entities), len(refreshed), eventsEmitted)

	// Always fire the sentinel — it means "a poll cycle completed," not "a
	// poll produced work." Carry-over readiness depends on this firing even
	// on an empty first poll (e.g. projects configured but nothing assigned
	// yet), otherwise the setup step shimmers forever.
	t.EmitPollComplete("jira", startedAt, len(entities), eventsEmitted)

	return eventsEmitted, nil
}

// discoverJira runs JQL queries to find new issues. Each project gets
// its own JQL pair — one Pickup query against the project's
// PickupMembers and one assigned-to-me query that excludes the
// project's DoneMembers. Per-project iteration is required because
// status names rarely overlap across heterogeneous workflows
// ("Backlog/Selected" vs "New/Triage"); a unified `status IN
// (union)` query would surface tickets the user never wants to pick
// up.
//
// Subtask classification uses the union of every project's
// DoneMembers — subtasks can live in projects other than the parent's,
// and the union matches today's "treat any known done status as
// terminal" behavior across heterogeneous projects.
func (t *Tracker) discoverJira(client *jiraclient.Client, baseURL string, projects JiraRules) ([]jiraIssueState, error) {
	if len(projects) == 0 {
		return nil, nil
	}

	type queryWithDone struct {
		jql         string
		doneMembers []string // for subtask classification on issues returned by this query
	}
	var queries []queryWithDone

	allDone := projects.AllDoneMembers()

	for _, p := range projects {
		if p.Key == "" {
			continue
		}

		if len(p.PickupMembers) > 0 {
			quoted := make([]string, len(p.PickupMembers))
			for i, s := range p.PickupMembers {
				quoted[i] = fmt.Sprintf("%q", s)
			}
			queries = append(queries, queryWithDone{
				jql: fmt.Sprintf(
					`project = %q AND status IN (%s) AND assignee IS EMPTY`,
					p.Key, strings.Join(quoted, ", ")),
				doneMembers: allDone,
			})
		}

		// Assigned-to-me query, with terminal statuses excluded via the
		// project's Done.Members set. If empty (defensive — Ready()
		// gates the poller on non-empty Done.Members, so we shouldn't
		// hit this in practice), the NOT IN clause is dropped entirely
		// rather than falling back to a hardcoded list that would
		// contradict the user's workflow.
		assignedJQL := fmt.Sprintf(`project = %q AND assignee = currentUser()`, p.Key)
		if len(p.DoneMembers) > 0 {
			quoted := make([]string, len(p.DoneMembers))
			for i, s := range p.DoneMembers {
				quoted[i] = fmt.Sprintf("%q", s)
			}
			assignedJQL += fmt.Sprintf(` AND status NOT IN (%s)`, strings.Join(quoted, ", "))
		}
		queries = append(queries, queryWithDone{jql: assignedJQL, doneMembers: allDone})
	}

	seen := map[string]bool{}
	var all []jiraIssueState

	// "updated" is required for the diff layer's source-time fallback —
	// without it, JiraSnapshot.UpdatedAt is empty and emit() degrades all
	// the way to detection time. Added explicitly here because this
	// callsite passes a custom field list rather than relying on
	// DefaultSearchFields.
	fields := []string{"summary", "description", "status", "assignee", "priority", "labels", "issuetype", "parent", "comment", "subtasks", "created", "updated"}

	for _, q := range queries {
		issues, err := client.SearchIssues(q.jql, fields, 100)
		if err != nil {
			log.Printf("[tracker] Jira discovery query failed: %v", err)
			continue
		}
		for _, issue := range issues {
			if !seen[issue.Key] {
				seen[issue.Key] = true
				all = append(all, issueToState(issue, baseURL, q.doneMembers))
			}
		}
	}

	return all, nil
}

// batchFetchJira fetches current state for tracked Jira issues. Description
// is deliberately excluded from the field list — it's seeded on discovery
// and only relevant to the scorer, which reads from the stored column rather
// than the API response. Skipping the multi-KB body on every poll saves
// bandwidth and latency; the tradeoff is that descriptions for entities
// that stop matching discovery's JQL (e.g. reassigned to someone else) stay
// pinned at their last-captured value. Acceptable — description relevance
// drops fast once a ticket is off the user's plate.
func (t *Tracker) batchFetchJira(client *jiraclient.Client, baseURL string, keys []string, projects JiraRules) (map[string]jiraIssueState, error) {
	results := make(map[string]jiraIssueState, len(keys))
	// "updated" is required for the diff layer's source-time fallback.
	// See the comment on the discovery field list for context.
	fields := []string{"summary", "status", "assignee", "priority", "labels", "issuetype", "parent", "comment", "subtasks", "created", "updated"}

	allDone := projects.AllDoneMembers()

	for i := 0; i < len(keys); i += jiraBatchSize {
		end := i + jiraBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]

		jql := fmt.Sprintf("key IN (%s)", strings.Join(batch, ", "))
		issues, err := client.SearchIssues(jql, fields, jiraBatchSize)
		if err != nil {
			return nil, fmt.Errorf("batch fetch keys %d-%d: %w", i, end, err)
		}

		for _, issue := range issues {
			// Subtask classification uses the union of every project's
			// done members — subtasks can live in projects other than
			// the parent's.
			results[issue.Key] = issueToState(issue, baseURL, allDone)
		}
	}

	return results, nil
}

// jiraIssueState bundles the diff-scope snapshot with the bulk description
// body. Description is carried alongside rather than inside the snapshot so
// the persisted snapshot_json stays small — diff reads don't drag multi-KB
// issue bodies through every poll.
type jiraIssueState struct {
	Snap        domain.JiraSnapshot
	Description string
}

// issueToState converts a Jira API Issue into the diff-scope snapshot plus
// a flattened description. The description is stored on entities.description
// separately; the snapshot itself only carries fields that DiffJiraSnapshots
// compares. doneStatuses is the user's configured Done.Members set, used
// to decide which subtasks count as "open" when populating OpenSubtaskCount.
func issueToState(issue jiraclient.Issue, baseURL string, doneStatuses []string) jiraIssueState {
	snap := domain.JiraSnapshot{
		Key:     issue.Key,
		Summary: issue.Fields.Summary,
		URL:     fmt.Sprintf("%s/browse/%s", strings.TrimRight(baseURL, "/"), issue.Key),
	}
	if issue.Fields.Status != nil {
		snap.Status = issue.Fields.Status.Name
	}
	if issue.Fields.Assignee != nil {
		snap.Assignee = issue.Fields.Assignee.DisplayName
		// Prefer AccountID (Jira Cloud). Fall back to Name (Jira
		// Server/DC username key) so predicates and inline-close
		// comparisons work on both deployment types. This mirrors
		// auth.JiraUser.StableID() which prefers accountId over key.
		if issue.Fields.Assignee.AccountID != "" {
			snap.AssigneeAccountID = issue.Fields.Assignee.AccountID
		} else {
			snap.AssigneeAccountID = issue.Fields.Assignee.Name
		}
	}
	if issue.Fields.Priority != nil {
		snap.Priority = issue.Fields.Priority.Name
	}
	if issue.Fields.IssueType != nil {
		snap.IssueType = issue.Fields.IssueType.Name
	}
	if issue.Fields.Parent != nil {
		snap.ParentKey = issue.Fields.Parent.Key
	}
	if issue.Fields.Comment != nil {
		snap.CommentCount = issue.Fields.Comment.Total
	}
	snap.Labels = issue.Fields.Labels
	if issue.Fields.Created != "" {
		snap.CreatedAt = issue.Fields.Created
	}
	if issue.Fields.Updated != "" {
		snap.UpdatedAt = issue.Fields.Updated
	}
	snap.OpenSubtaskCount = countOpenSubtasks(issue, doneStatuses)
	return jiraIssueState{
		Snap:        snap,
		Description: truncateDescription(jiraclient.ExtractDescriptionText(issue.Fields.Description), descriptionStoreMaxRunes),
	}
}

// countOpenSubtasks returns the number of subtasks on this issue whose
// status is NOT in the configured Done.Members set. Missing/unknown status
// is counted as open — conservative default: better to show a parent as
// "has open subtasks" and suppress task creation than to wrongly surface
// it as atomic when we couldn't classify.
func countOpenSubtasks(issue jiraclient.Issue, doneStatuses []string) int {
	if len(issue.Fields.Subtasks) == 0 {
		return 0
	}
	done := make(map[string]struct{}, len(doneStatuses))
	for _, s := range doneStatuses {
		done[s] = struct{}{}
	}
	open := 0
	for _, sub := range issue.Fields.Subtasks {
		name := ""
		if sub.Fields.Status != nil {
			name = sub.Fields.Status.Name
		}
		if _, ok := done[name]; !ok {
			open++
		}
	}
	return open
}

// truncateDescription caps the stored description at maxRunes codepoints
// (rune-based so we never persist a string that ends mid-UTF-8-codepoint).
// Strict cap — when truncation happens the returned string contains exactly
// maxRunes runes, with the last rune replaced by an ellipsis so downstream
// readers can distinguish a cut string from a genuinely short one.
func truncateDescription(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

// --- Helpers ---

// EmitPollComplete publishes the system poll-completed sentinel. startedAt
// is the wall-clock time the poll cycle started, carried in metadata so
// subscribers can ignore sentinels emitted by pre-restart poll generations
// (an old RefreshXxx goroutine that finishes after a config-triggered restart).
func (t *Tracker) EmitPollComplete(source string, startedAt time.Time, entityCount, eventCount int) {
	t.publish(domain.Event{
		EventType: domain.EventSystemPollCompleted,
		MetadataJSON: mustJSON(events.SystemPollCompletedMetadata{
			Source:    source,
			StartedAt: startedAt.UnixNano(),
			Entities:  entityCount,
			Events:    eventCount,
		}),
		CreatedAt: time.Now(),
	})
}
