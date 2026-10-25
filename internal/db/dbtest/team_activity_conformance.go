package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamActivityStoreFactory builds a fresh TeamActivityStore plus the fixture
// kit for one subtest. It reuses FactorySeeder deliberately: the node and the
// factory belt share their definition of a team's flow (tracked-set entities,
// team-scoped tasks), so they share the seeding contract too — an entity the
// seeder makes belt-visible is exactly one the node must count.
type TeamActivityStoreFactory func(t *testing.T) (store db.TeamActivityStore, orgID, teamID string, seed FactorySeeder)

// RunTeamActivityConformance covers the read contract every
// TeamActivityStore impl must hold: the window clips, the cuts agree on
// totals, the by-source cut carries a row per external catalog source
// regardless of traffic, the tracked-set cut carries a row per tracked unit
// and no other, and system events (NULL entity) never count.
//
// What it deliberately does NOT pin: whether a source's event count is
// defined (github and jira are defined everywhere; slack diverges by design
// — defined at N=1 where the team is the org, undefined under the
// tracked-set rule), nor the scoping of the merged and failed cuts, which
// only multi-mode has a second team to scope against, nor what the
// by-source cut counts for a unit outside the tracked set, which the two
// dialects scope differently. Each backend's test file asserts its own side
// of all three.
func RunTeamActivityConformance(t *testing.T, mk TeamActivityStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("window_clips_and_cuts_agree", func(t *testing.T) {
		s, orgID, teamID, seed := mk(t)
		now := time.Now().UTC()
		since, until := now.Add(-72*time.Hour), now.Add(time.Minute)

		entityID := seed.Entity(t, "act")
		ev := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed, "", now.Add(-2*time.Hour), time.Time{})
		seed.Event(t, entityID, domain.EventGitHubPRNewCommits, "", now.Add(-26*time.Hour), time.Time{})
		// Before the window: must not count.
		seed.Event(t, entityID, domain.EventGitHubPROpened, "", now.Add(-80*time.Hour), time.Time{})
		// A system emission has no entity, so it has no source and no place
		// in the flow.
		seed.EventNullEntity(t, domain.EventSystemPollCompleted, now.Add(-1*time.Hour))
		taskID := seed.Task(t, entityID, domain.EventGitHubPRCICheckFailed, "", ev, "queued", now.Add(-90*time.Minute))
		seed.Conversation(t, taskID, "running")
		// A merged pull request is an ordinary event row, so it counts in
		// events too — merged is a named subset, not a parallel total.
		seed.Event(t, entityID, domain.EventGitHubPRMerged, "", now.Add(-3*time.Hour), time.Time{})
		// A run that died inside the window, and one that died before it.
		failedIn := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, failedIn, domain.StatusFailed, now.Add(-45*time.Minute))
		failedOut := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, failedOut, domain.StatusFailed, now.Add(-80*time.Hour))
		// A run that concluded is not a failure, however recently it ended.
		concluded := seed.Conversation(t, taskID, SeedConcluded)
		seed.FinishConversation(t, concluded, domain.StatusOpen, now.Add(-30*time.Minute))

		got, err := s.TeamActivity(ctx, orgID, teamID, since, until)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}

		// Day-bucket boundaries are timezone-fragile, so the assertions sum
		// across days rather than pinning which day a row landed in.
		dayEvents, dayTasks, dayMerged, dayFailed := 0, 0, 0, 0
		for _, d := range got.ByDay {
			dayEvents += d.Events
			dayTasks += d.Tasks
			dayMerged += d.Merged
			dayFailed += d.Failed
		}
		if dayEvents != 3 || dayTasks != 1 {
			t.Errorf("by_day sums = %d events / %d tasks, want 3 / 1 (window clipped, system row ignored)", dayEvents, dayTasks)
		}
		if dayMerged != 1 {
			t.Errorf("by_day merged = %d, want 1", dayMerged)
		}
		if dayFailed != 1 {
			t.Errorf("by_day failed = %d, want 1 (the run that ended outside the window and the completed one don't count)", dayFailed)
		}
		dsEvents, dsTasks := 0, 0
		for _, d := range got.ByDaySource {
			if d.Source != "github" {
				t.Errorf("by_day_source names source %q, want only github", d.Source)
			}
			dsEvents += d.Events
			dsTasks += d.Tasks
		}
		if dsEvents != dayEvents || dsTasks != dayTasks {
			t.Errorf("by_day_source sums = %d/%d, disagree with by_day %d/%d", dsEvents, dsTasks, dayEvents, dayTasks)
		}
		for i := 1; i < len(got.ByDay); i++ {
			if got.ByDay[i-1].Date >= got.ByDay[i].Date {
				t.Errorf("by_day not ascending: %q then %q", got.ByDay[i-1].Date, got.ByDay[i].Date)
			}
		}

		bySource := map[string]domain.TeamActivitySource{}
		for _, row := range got.BySource {
			bySource[row.Source] = row
		}
		for _, want := range []string{"github", "jira", "slack"} {
			if _, ok := bySource[want]; !ok {
				t.Errorf("by_source is missing the %s row — the shape must not wobble with traffic", want)
			}
		}
		gh := bySource["github"]
		if gh.Events == nil || *gh.Events != 3 || gh.Tasks != 1 || gh.Runs != 4 {
			t.Errorf("github row = %+v, want events 3 / tasks 1 / runs 4", gh)
		}
		jira := bySource["jira"]
		if jira.Events == nil || *jira.Events != 0 || jira.Tasks != 0 || jira.Runs != 0 {
			t.Errorf("jira row = %+v, want a defined zero row", jira)
		}
		// LastPollAt is the handler's stitch, never the store's.
		for _, row := range got.BySource {
			if row.LastPollAt != nil {
				t.Errorf("%s row carries LastPollAt from the store; poll times are the handler's", row.Source)
			}
		}
	})

	t.Run("by_tracked_is_the_tracked_set", func(t *testing.T) {
		s, orgID, teamID, seed := mk(t)
		// Millisecond precision, as in window_is_half_open, so a latest
		// event read back from either dialect compares exactly.
		now := time.Now().UTC().Truncate(time.Millisecond)
		since, until := now.Add(-72*time.Hour), now.Add(time.Minute)

		pr, repo := seed.TrackedEntity(t, "github")
		issue, project := seed.TrackedEntity(t, "jira")
		linear, linearTeam := seed.TrackedEntity(t, "linear")
		// Tracked, and has never sent an event.
		_, quietRepo := seed.TrackedEntity(t, "github")

		prLast := now.Add(-2 * time.Hour)
		prEvent := seed.Event(t, pr, domain.EventGitHubPRCICheckFailed, "", prLast, time.Time{})
		seed.Event(t, pr, domain.EventGitHubPRNewCommits, "", now.Add(-26*time.Hour), time.Time{})
		seed.Event(t, pr, domain.EventGitHubPROpened, "", now.Add(-80*time.Hour), time.Time{})
		issueLast := now.Add(-3 * time.Hour)
		issueEvent := seed.Event(t, issue, domain.EventJiraIssueAssigned, "", issueLast, time.Time{})
		// The Linear team's only event is before the window: a row of zeros
		// that still says when the unit was last heard from.
		linearLast := now.Add(-100 * time.Hour)
		seed.Event(t, linear, domain.EventLinearIssueAssigned, "", linearLast, time.Time{})

		prTask := seed.Task(t, pr, domain.EventGitHubPRCICheckFailed, "", prEvent, "queued", now.Add(-90*time.Minute))
		seed.Conversation(t, prTask, "running")
		seed.Conversation(t, prTask, domain.StatusFailed)
		issueTask := seed.Task(t, issue, domain.EventJiraIssueAssigned, "", issueEvent, "queued", now.Add(-2*time.Hour))
		seed.Conversation(t, issueTask, domain.StatusFailed)

		got, err := s.TeamActivity(ctx, orgID, teamID, since, until)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}

		type unit struct{ source, id string }
		type want struct {
			events, tasks, runs int
			last                *time.Time
		}
		wants := map[unit]want{
			{"github", repo}:       {2, 1, 2, &prLast},
			{"github", quietRepo}:  {0, 0, 0, nil},
			{"jira", project}:      {1, 1, 1, &issueLast},
			{"linear", linearTeam}: {0, 0, 0, &linearLast},
		}
		if len(got.ByTracked) != len(wants) {
			t.Errorf("by_tracked = %+v, want exactly the %d tracked units", got.ByTracked, len(wants))
		}
		type sums struct{ events, tasks, runs int }
		bySource := map[string]sums{}
		for _, row := range got.ByTracked {
			w, ok := wants[unit{row.Source, row.UnitID}]
			if !ok {
				t.Errorf("by_tracked carries %s %q, which is no tracked unit", row.Source, row.UnitID)
				continue
			}
			if row.Events != w.events || row.Tasks != w.tasks || row.Runs != w.runs {
				t.Errorf("%s %q = %d events / %d tasks / %d runs, want %d / %d / %d",
					row.Source, row.UnitID, row.Events, row.Tasks, row.Runs, w.events, w.tasks, w.runs)
			}
			switch {
			case w.last == nil && row.LastEventAt != nil:
				t.Errorf("%s %q last_event_at = %v, want nil — it has never sent an event", row.Source, row.UnitID, *row.LastEventAt)
			case w.last != nil && (row.LastEventAt == nil || !row.LastEventAt.Equal(*w.last)):
				t.Errorf("%s %q last_event_at = %v, want %v", row.Source, row.UnitID, row.LastEventAt, *w.last)
			}
			sum := bySource[row.Source]
			sum.events += row.Events
			sum.tasks += row.Tasks
			sum.runs += row.Runs
			bySource[row.Source] = sum
		}

		// Every unit here is tracked, so each source's rows add up to its
		// by-source figures exactly.
		for _, src := range got.BySource {
			sum := bySource[src.Source]
			if src.Source == "slack" {
				if sum != (sums{}) {
					t.Errorf("by_tracked carries slack rows; slack has no tracked set")
				}
				continue
			}
			if src.Events == nil || *src.Events != sum.events || src.Tasks != sum.tasks || src.Runs != sum.runs {
				t.Errorf("%s by_source = %+v, disagrees with its by_tracked rows' sum %+v", src.Source, src, sum)
			}
		}
	})

	t.Run("a_failed_run_alone_still_makes_a_day", func(t *testing.T) {
		s, orgID, teamID, seed := mk(t)
		now := time.Now().UTC()
		// Failed is the cut that can orphan a day. Merged never can — a
		// merged pull request is an event row, so its day is already in the
		// events cut's answer — but a run that died reaches no event and no
		// entity, so a day whose only fact is a failure exists in that cut
		// alone. Building by_day by left-joining onto the days the
		// events/tasks queries found would drop it.
		entityID := seed.Entity(t, "orphan-day")
		ev := seed.Event(t, entityID, domain.EventGitHubPROpened, "", now.Add(-80*time.Hour), time.Time{})
		taskID := seed.Task(t, entityID, domain.EventGitHubPROpened, "", ev, "queued", now.Add(-80*time.Hour))
		conversationID := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, conversationID, domain.StatusFailed, now.Add(-90*time.Minute))

		// Nothing else the node buckets falls inside this window.
		got, err := s.TeamActivity(ctx, orgID, teamID, now.Add(-3*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		if len(got.ByDay) != 1 {
			t.Fatalf("by_day = %+v, want exactly the failure's day", got.ByDay)
		}
		day := got.ByDay[0]
		if day.Failed != 1 || day.Events != 0 || day.Tasks != 0 || day.Merged != 0 {
			t.Errorf("by_day row = %+v, want failed 1 and every other count 0", day)
		}
	})

	t.Run("window_is_half_open", func(t *testing.T) {
		s, orgID, teamID, seed := mk(t)
		// Millisecond precision: SQLite's date functions parse no finer, so
		// a bound carrying more of it could not be seeded exactly.
		until := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		since := until.Add(-2 * time.Hour)

		entityID := seed.Entity(t, "boundary")
		ev := seed.Event(t, entityID, domain.EventGitHubPRMerged, "", since, time.Time{})
		seed.Event(t, entityID, domain.EventGitHubPRMerged, "", until, time.Time{})
		taskID := seed.Task(t, entityID, domain.EventGitHubPRMerged, "", ev, "queued", since)
		onSince := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, onSince, domain.StatusFailed, since)
		onUntil := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, onUntil, domain.StatusFailed, until)

		got, err := s.TeamActivity(ctx, orgID, teamID, since, until)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		merged, failed := 0, 0
		for _, d := range got.ByDay {
			merged += d.Merged
			failed += d.Failed
		}
		if merged != 1 {
			t.Errorf("merged = %d, want 1 — [since, until) includes since and excludes until", merged)
		}
		if failed != 1 {
			t.Errorf("failed = %d, want 1 — [since, until) includes since and excludes until", failed)
		}
	})

	t.Run("quiet_window_keeps_the_shape", func(t *testing.T) {
		s, orgID, teamID, seed := mk(t)
		now := time.Now().UTC()
		// Traffic exists, but entirely outside the asked-for window.
		entityID := seed.Entity(t, "quiet")
		seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed, "", now.Add(-80*time.Hour), time.Time{})

		got, err := s.TeamActivity(ctx, orgID, teamID, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		if len(got.ByDay) != 0 || len(got.ByDaySource) != 0 {
			t.Errorf("quiet window produced series rows: %+v / %+v", got.ByDay, got.ByDaySource)
		}
		if len(got.BySource) < 3 {
			t.Errorf("by_source = %+v, want the full external-source row set even with no traffic", got.BySource)
		}
		for _, row := range got.ByTracked {
			if row.Events != 0 || row.Tasks != 0 || row.Runs != 0 {
				t.Errorf("by_tracked row %+v in a quiet window, want zeros", row)
			}
		}
	})
}
