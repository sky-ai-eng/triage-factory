package dbtest

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityReadFactory is what a per-backend test file hands to
// RunEntityReadConformance: the two stores whose lists the entity reads
// serve, the org to read, and a seeder whose Entity makes each entity visible
// to those reads (on Postgres, by tracking its repository for the org's
// team).
type EntityReadFactory func(t *testing.T) (entities db.EntityStore, events db.EventStore, orgID string, seed FlowSeeder)

// RunEntityReadConformance covers the read contract of EntityStore.List and
// EventStore.List that every dialect must hold: each filter narrows, the
// latest event is the newest recorded one, the order is total and pages
// partition it, and the event list resumes a keyset walk exactly while
// events arrive. The tracked-set scoping is Postgres-only and lives in the
// backend test file.
func RunEntityReadConformance(t *testing.T, mk EntityReadFactory) {
	t.Helper()
	ctx := context.Background()

	listIDs := func(t *testing.T, store db.EntityStore, orgID string, f db.EntityListFilter) map[string]domain.ListedEntity {
		t.Helper()
		items, total, err := store.List(ctx, orgID, f, db.Unwindowed)
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		if total != len(items) {
			t.Errorf("List(%+v): total = %d, want the %d rows it served unwindowed", f, total, len(items))
		}
		out := make(map[string]domain.ListedEntity, len(items))
		for _, it := range items {
			out[it.Entity.ID] = it
		}
		return out
	}

	t.Run("EntityList_CarriesTheLatestEvent", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		now := time.Now().UTC().Truncate(time.Millisecond)
		withEvents, repo := seed.GroupedEntity(t, "github")
		quiet := seed.Entity(t, "quiet")
		seed.Event(t, withEvents, domain.EventGitHubPROpened, "", now.Add(-3*time.Minute), time.Time{})
		// A source time later than the detection time does not make an event
		// the latest: the latest is the newest recorded.
		seed.Event(t, withEvents, domain.EventGitHubPRCICheckPassed, "", now.Add(-time.Minute), now.Add(time.Hour))
		seed.Event(t, withEvents, domain.EventGitHubPRCommented, "", now.Add(-2*time.Minute), time.Time{})

		got := listIDs(t, entities, orgID, db.EntityListFilter{})
		item, ok := got[withEvents]
		if !ok {
			t.Fatalf("entity %s missing from the list", withEvents)
		}
		if item.LatestEventType != domain.EventGitHubPRCICheckPassed {
			t.Errorf("latest event type = %q, want %q", item.LatestEventType, domain.EventGitHubPRCICheckPassed)
		}
		if item.LatestEventAt == nil || !item.LatestEventAt.Equal(now.Add(-time.Minute)) {
			t.Errorf("latest event at = %v, want %v", item.LatestEventAt, now.Add(-time.Minute))
		}
		if item.Entity.RepositoryID != repo {
			t.Errorf("repository id = %q, want %q", item.Entity.RepositoryID, repo)
		}
		if q, ok := got[quiet]; !ok {
			t.Errorf("entity %s with no event missing from the list", quiet)
		} else if q.LatestEventType != "" || q.LatestEventAt != nil {
			t.Errorf("entity with no event: latest = (%q, %v), want none", q.LatestEventType, q.LatestEventAt)
		}
	})

	// Events detected in the same instant, as one poll's burst can be, keep
	// the order they were recorded in.
	t.Run("EntityList_LatestEventBreaksATieByInsertion", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		ent := seed.Entity(t, "tie")
		seed.Event(t, ent, domain.EventGitHubPRNewCommits, "", at, time.Time{})
		seed.Event(t, ent, domain.EventGitHubPRCICheckFailed, "", at, time.Time{})
		got := listIDs(t, entities, orgID, db.EntityListFilter{IDs: []string{ent}})
		if got[ent].LatestEventType != domain.EventGitHubPRCICheckFailed {
			t.Errorf("latest event type = %q, want the one recorded last (%q)", got[ent].LatestEventType, domain.EventGitHubPRCICheckFailed)
		}
	})

	t.Run("EntityList_ClosedSinceKeepsTheOpenAndTheRecentlyClosed", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		now := time.Now().UTC()
		active := seed.Entity(t, "active")
		fresh := seed.Entity(t, "fresh")
		stale := seed.Entity(t, "stale")
		seed.CloseEntity(t, fresh, now.Add(-10*time.Second))
		seed.CloseEntity(t, stale, now.Add(-2*time.Hour))

		since := now.Add(-time.Minute)
		for _, c := range []struct {
			name string
			f    db.EntityListFilter
			want []string
		}{
			{"closed_since", db.EntityListFilter{ClosedSince: &since}, []string{active, fresh}},
			{"active", db.EntityListFilter{States: []string{"active"}}, []string{active}},
			{"closed", db.EntityListFilter{States: []string{"closed"}}, []string{fresh, stale}},
			{"closed and closed_since", db.EntityListFilter{States: []string{"closed"}, ClosedSince: &since}, []string{fresh}},
		} {
			got := listIDs(t, entities, orgID, c.f)
			for _, id := range []string{active, fresh, stale} {
				want := false
				for _, w := range c.want {
					want = want || w == id
				}
				if _, ok := got[id]; ok != want {
					t.Errorf("%s: entity %s listed = %v, want %v", c.name, id, ok, want)
				}
			}
		}
	})

	t.Run("EntityList_FiltersBySourceKindAndID", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		pr, _ := seed.GroupedEntity(t, "github")
		jira, _ := seed.GroupedEntity(t, "jira")
		stub := seed.Entity(t, "stub")
		seed.SetEntityKind(t, stub, domain.EntityKindIssue)

		bySource := listIDs(t, entities, orgID, db.EntityListFilter{Sources: []string{"github"}})
		if _, ok := bySource[pr]; !ok {
			t.Error("sources [github] dropped the pull request")
		}
		if _, ok := bySource[stub]; !ok {
			t.Error("sources [github] dropped the GitHub issue")
		}
		if _, ok := bySource[jira]; ok {
			t.Error("sources [github] kept the Jira issue")
		}
		byKind := listIDs(t, entities, orgID, db.EntityListFilter{Sources: []string{"github"}, Kinds: []string{domain.EntityKindPullRequest}})
		if _, ok := byKind[stub]; ok || len(byKind) != 1 {
			t.Errorf("kinds [pr] listed %d rows (stub listed %v), want the pull request alone", len(byKind), ok)
		}
		byID := listIDs(t, entities, orgID, db.EntityListFilter{IDs: []string{jira}})
		if _, ok := byID[jira]; !ok || len(byID) != 1 {
			t.Errorf("ids [%s] listed %d rows, want that entity alone", jira, len(byID))
		}
	})

	t.Run("EntityList_PagesPartitionANewestFirstOrder", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		for i := 0; i < 5; i++ {
			seed.Entity(t, "page-"+strconv.Itoa(i))
		}
		all, total, err := entities.List(ctx, orgID, db.EntityListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 5 || len(all) != 5 {
			t.Fatalf("unwindowed list = %d rows of total %d, want 5 of 5", len(all), total)
		}
		if !sort.SliceIsSorted(all, func(i, j int) bool {
			a, b := all[i].Entity, all[j].Entity
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
			return a.ID > b.ID
		}) {
			t.Error("the list is not ordered created_at DESC, id DESC")
		}
		var walked []string
		for offset := 0; offset < 5; offset += 2 {
			page, pageTotal, err := entities.List(ctx, orgID, db.EntityListFilter{}, db.ListOpts{Limit: 2, Offset: offset})
			if err != nil {
				t.Fatalf("List page at %d: %v", offset, err)
			}
			if pageTotal != 5 {
				t.Errorf("page at %d: total = %d, want 5", offset, pageTotal)
			}
			for _, it := range page {
				walked = append(walked, it.Entity.ID)
			}
		}
		var want []string
		for _, it := range all {
			want = append(want, it.Entity.ID)
		}
		if !reflect.DeepEqual(walked, want) {
			t.Errorf("pages walked %v, want the unwindowed order %v", walked, want)
		}
		none, countTotal, err := entities.List(ctx, orgID, db.EntityListFilter{}, db.ListOpts{CountOnly: true})
		if err != nil || len(none) != 0 || countTotal != 5 {
			t.Errorf("count-only = %d rows, total %d, err %v; want 0 rows, total 5", len(none), countTotal, err)
		}
	})

	t.Run("EventList_FiltersByEntityAndSince", func(t *testing.T) {
		_, events, orgID, seed := mk(t)
		now := time.Now().UTC().Truncate(time.Millisecond)
		a := seed.Entity(t, "ev-a")
		b := seed.Entity(t, "ev-b")
		old := seed.Event(t, a, domain.EventGitHubPROpened, "", now.Add(-3*time.Hour), time.Time{})
		mid := seed.Event(t, a, domain.EventGitHubPRNewCommits, "", now.Add(-30*time.Minute), now.Add(-31*time.Minute))
		newest := seed.Event(t, a, domain.EventGitHubPRCICheckPassed, "", now.Add(-10*time.Minute), time.Time{})
		other := seed.Event(t, b, domain.EventGitHubPROpened, "", now.Add(-5*time.Minute), time.Time{})
		seed.EventNullEntity(t, domain.EventGitHubPROpened, now.Add(-time.Minute))

		read := func(f db.EventListFilter) []db.ListedEvent {
			t.Helper()
			rows, total, err := events.List(ctx, orgID, f, db.Unwindowed)
			if err != nil {
				t.Fatalf("List(%+v): %v", f, err)
			}
			if total != len(rows) {
				t.Errorf("List(%+v): total = %d, want the %d rows it served unwindowed", f, total, len(rows))
			}
			return rows
		}
		ids := func(rows []db.ListedEvent) []string {
			out := make([]string, len(rows))
			for i, r := range rows {
				out[i] = r.Event.ID
			}
			return out
		}

		if got, want := ids(read(db.EventListFilter{})), []string{other, newest, mid, old}; !reflect.DeepEqual(got, want) {
			t.Errorf("every entity event = %v, want newest recorded first and no system event: %v", got, want)
		}
		since := now.Add(-time.Hour)
		rows := read(db.EventListFilter{EntityIDs: []string{a}, Since: &since})
		if got, want := ids(rows), []string{newest, mid}; !reflect.DeepEqual(got, want) {
			t.Errorf("entity a since an hour ago = %v, want %v", got, want)
		}
		for _, r := range rows {
			if r.Event.EntityID == nil || *r.Event.EntityID != a {
				t.Errorf("event %s entity = %v, want %s", r.Event.ID, r.Event.EntityID, a)
			}
			switch r.Event.ID {
			case mid:
				if !r.Event.OccurredAt.Equal(now.Add(-31 * time.Minute)) {
					t.Errorf("occurred_at = %v, want the source time %v", r.Event.OccurredAt, now.Add(-31*time.Minute))
				}
			case newest:
				if !r.Event.OccurredAt.IsZero() {
					t.Errorf("an event with no source time carries occurred_at %v", r.Event.OccurredAt)
				}
			}
		}
	})

	t.Run("EventList_KeysetResumesWhileEventsArrive", func(t *testing.T) {
		_, events, orgID, seed := mk(t)
		now := time.Now().UTC()
		ent := seed.Entity(t, "keyset")
		first := seed.Event(t, ent, domain.EventGitHubPROpened, "", now.Add(-3*time.Minute), time.Time{})
		second := seed.Event(t, ent, domain.EventGitHubPRNewCommits, "", now.Add(-2*time.Minute), time.Time{})
		third := seed.Event(t, ent, domain.EventGitHubPRCICheckPassed, "", now.Add(-time.Minute), time.Time{})

		f := db.EventListFilter{EntityIDs: []string{ent}}
		page1, total, err := events.List(ctx, orgID, f, db.ListOpts{Limit: 2})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if total != 3 || len(page1) != 2 || page1[0].Event.ID != third || page1[1].Event.ID != second {
			t.Fatalf("page 1 = %d rows of %d, want [%s %s] of 3", len(page1), total, third, second)
		}
		// A new event lands between the two pages; it sorts ahead of the cut.
		seed.Event(t, ent, domain.EventGitHubPRReviewApproved, "", now, time.Time{})
		after := []string{strconv.FormatInt(page1[1].Position, 10)}
		page2, total, err := events.List(ctx, orgID, f, db.ListOpts{Limit: 2, After: after})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(page2) != 1 || page2[0].Event.ID != first {
			t.Errorf("page 2 = %d rows, want [%s] alone, neither repeated nor skipped", len(page2), first)
		}
		if total != 4 {
			t.Errorf("page 2 total = %d, want 4", total)
		}
	})
}
