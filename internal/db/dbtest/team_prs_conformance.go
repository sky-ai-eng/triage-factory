package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamPRStoreFactory is what a per-backend test file hands to
// RunTeamPRStoreConformance. Returns:
//   - the wired TeamPRStore impl,
//   - the orgID and the SUBJECT team id every call names,
//   - the org's configured GitHub base URL, passed through to the read the way
//     the handler passes it (raw from org_settings — the store resolves it),
//   - a TeamPRSeeder for the fixtures.
type TeamPRStoreFactory func(t *testing.T) (store db.TeamPRStore, orgID, teamID, githubBaseURL string, seed TeamPRSeeder)

// TeamPRFixture is one seeded pull request. Its snapshot is what the row is
// projected from; the two columns beside it are what the population and the
// states filter actually read.
type TeamPRFixture struct {
	Snapshot domain.PRSnapshot
	// OwningTeam stamps entities.owning_team_id — the "TF opened this on the
	// team's behalf" leg. Empty leaves it NULL.
	OwningTeam string
	// EntityState overrides entities.state, which is what the open filter
	// reads. Empty means 'active'.
	EntityState string
}

// TeamPRSeeder is a bag of callbacks the conformance suite stages fixtures
// through. Each backend implements them against its own SQL.
type TeamPRSeeder struct {
	// Member adds a user to the SUBJECT team and binds login on the org's
	// GitHub host, returning the user id. A member seeded this way is one
	// whose authored pull requests must appear.
	Member func(t *testing.T, login string) string
	// MemberWithoutIdentity adds a user to the subject team and binds
	// nothing, returning the user id — the accepted caveat that an unbound
	// member contributes no rows.
	MemberWithoutIdentity func(t *testing.T, name string) string
	// OtherTeam creates a second team in the same org and returns its id, for
	// the owning-team leg's negative case.
	OtherTeam func(t *testing.T, name string) string
	// PR inserts a github pull-request entity carrying the fixture, in a repo
	// the SUBJECT team tracks, and returns its entity id. Tracking is the
	// backend's business: multi registers the repo in the team's tracked set,
	// local is N=1 and every entity is the team's by construction. A repo the
	// team does NOT track is the Postgres suite's own case, since local has no
	// second team to be outside of.
	PR func(t *testing.T, fx TeamPRFixture) string
}

// RunTeamPRStoreConformance covers the read contract every TeamPRStore impl
// must hold:
//
//   - the population is the two legs unioned — a member authored it, or the
//     team owns it structurally — and nothing else in the tracked set;
//   - a member who has bound no GitHub identity contributes nothing;
//   - another team's owned pull request is not this team's;
//   - the states filter maps as specified, refuses an unknown value, and the
//     count-only read equals the paged total under the same filters;
//   - paging applies to the team's set rather than to the entity table.
//
// What it deliberately does NOT pin is the tracked-set outer filter: local
// mode is N=1 and has no untracked repo to exclude, so that assertion lives in
// the Postgres backend's own test file — the same split the team activity
// node's suite draws.
func RunTeamPRStoreConformance(t *testing.T, mk TeamPRStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("population_is_members_union_owned", func(t *testing.T) {
		s, orgID, teamID, host, seed := mk(t)
		seed.Member(t, "member-a")
		seed.Member(t, "member-b")
		seed.MemberWithoutIdentity(t, "Unbound Member")
		otherTeam := seed.OtherTeam(t, "Another Team")

		seed.PR(t, TeamPRFixture{Snapshot: prSnap(1, "member-a")})
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(2, "member-b")})
		// TF opened this one: the author is a bot that maps to no TF user, so
		// only the owning-team stamp can claim it.
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(3, "tf-bot"), OwningTeam: teamID})
		// A stranger in a repo the team tracks. Tracking a repo is not
		// adopting everyone who pushes to it.
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(4, "stranger")})
		// A bot pull request another team commissioned, in the same tracked
		// repo. Ownership is the discriminator, not the repo.
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(5, "tf-bot"), OwningTeam: otherTeam})
		// The unbound member's own work: invisible until they bind.
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(6, "unbound-login")})

		prs, total, err := s.TeamPRs(ctx, orgID, teamID, host, db.PRListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("TeamPRs: %v", err)
		}
		got := numbersOf(prs)
		if len(prs) != 3 || total != 3 {
			t.Fatalf("returned %v (total %d), want exactly #1, #2 and #3", got, total)
		}
		for _, want := range []int{1, 2, 3} {
			if !got[want] {
				t.Errorf("missing #%d from the team's pull requests", want)
			}
		}
		if got[4] {
			t.Error("returned #4 — a stranger's pull request in a tracked repo is not the team's")
		}
		if got[5] {
			t.Error("returned #5 — another team's owned pull request is not this team's")
		}
		if got[6] {
			t.Error("returned #6 — a member with no bound identity contributes nothing")
		}
	})

	t.Run("states_filter_and_count_only", func(t *testing.T) {
		s, orgID, teamID, host, seed := mk(t)
		seed.Member(t, "member-a")

		// open reads the ENTITY's own state, so the terminal rows carry
		// state='closed' exactly as the tracker leaves them.
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(10, "member-a")})
		merged := prSnap(11, "member-a")
		merged.State, merged.Merged = "MERGED", true
		seed.PR(t, TeamPRFixture{Snapshot: merged, EntityState: "closed"})
		closed := prSnap(12, "member-a")
		closed.State = "CLOSED"
		seed.PR(t, TeamPRFixture{Snapshot: closed, EntityState: "closed"})

		for _, tc := range []struct {
			states []string
			want   []int
		}{
			{nil, []int{10, 11, 12}},
			{[]string{domain.PRStateOpen}, []int{10}},
			{[]string{domain.PRStateMerged}, []int{11}},
			// closed must not swallow merged: GitHub reports one wire shape of
			// a merged pull request as CLOSED with merged=true.
			{[]string{domain.PRStateClosed}, []int{12}},
			{[]string{domain.PRStateOpen, domain.PRStateMerged}, []int{10, 11}},
		} {
			f := db.PRListFilter{States: tc.states}
			prs, total, err := s.TeamPRs(ctx, orgID, teamID, host, f, db.Unwindowed)
			if err != nil {
				t.Fatalf("TeamPRs(states=%v): %v", tc.states, err)
			}
			got := numbersOf(prs)
			if len(prs) != len(tc.want) || total != len(tc.want) {
				t.Errorf("states=%v returned %v (total %d), want %v", tc.states, got, total, tc.want)
			}
			for _, n := range tc.want {
				if !got[n] {
					t.Errorf("states=%v is missing #%d", tc.states, n)
				}
			}
			// The Overview's figure is this read with page_size 0; it must be
			// the number the list itself would report, or the two disagree in
			// front of the user.
			empty, countTotal, err := s.TeamPRs(ctx, orgID, teamID, host, f, db.ListOpts{CountOnly: true})
			if err != nil {
				t.Fatalf("TeamPRs(states=%v, count-only): %v", tc.states, err)
			}
			if len(empty) != 0 {
				t.Errorf("states=%v count-only returned %d rows, want none", tc.states, len(empty))
			}
			if countTotal != total {
				t.Errorf("states=%v count-only total=%d, paged total=%d — the figure and the list disagree",
					tc.states, countTotal, total)
			}
		}
	})

	t.Run("unknown_state_is_refused", func(t *testing.T) {
		// Never dropped and never widened to "all": a filter the store cannot
		// honor has to fail, or a caller renders a figure for a query they did
		// not ask.
		s, orgID, teamID, host, seed := mk(t)
		seed.Member(t, "member-a")
		seed.PR(t, TeamPRFixture{Snapshot: prSnap(20, "member-a")})

		_, _, err := s.TeamPRs(ctx, orgID, teamID, host,
			db.PRListFilter{States: []string{"nonsense"}}, db.Unwindowed)
		if !errors.Is(err, db.ErrUnknownPRState) {
			t.Fatalf("TeamPRs(states=[nonsense]) err = %v, want db.ErrUnknownPRState", err)
		}
	})

	t.Run("pages_within_the_team_set", func(t *testing.T) {
		// The window has to apply to the team's pull requests, not to the
		// entity table: a filter applied after the LIMIT windows the wrong
		// set, and page 2 comes back empty while page 3 has rows.
		s, orgID, teamID, host, seed := mk(t)
		seed.Member(t, "member-a")
		for i := range 3 {
			seed.PR(t, TeamPRFixture{Snapshot: prSnap(30+i, "member-a")})
			seed.PR(t, TeamPRFixture{Snapshot: prSnap(40+i, "stranger")})
		}

		seen := map[int]bool{}
		for offset := 0; offset < 4; offset += 2 {
			page, total, err := s.TeamPRs(ctx, orgID, teamID, host, db.PRListFilter{},
				db.ListOpts{Limit: 2, Offset: offset})
			if err != nil {
				t.Fatalf("TeamPRs(offset=%d): %v", offset, err)
			}
			if total != 3 {
				t.Fatalf("TeamPRs(offset=%d) total=%d, want 3 (the team's, not the table's)", offset, total)
			}
			want := 2
			if offset == 2 {
				want = 1
			}
			if len(page) != want {
				t.Fatalf("TeamPRs(offset=%d) len=%d, want %d", offset, len(page), want)
			}
			for _, p := range page {
				if p.Author != "member-a" {
					t.Errorf("page carried a stranger's pull request: %+v", p)
				}
				if seen[p.Number] {
					t.Errorf("PR #%d served on two pages — the ORDER BY has no total order", p.Number)
				}
				seen[p.Number] = true
			}
		}
		if len(seen) != 3 {
			t.Fatalf("paging walked %d distinct pull requests, want 3", len(seen))
		}
	})
}

// prSnap is an open pull request by author, numbered so assertions can name
// it. Repo is left to the seeder, which owns the tracked-repo key.
func prSnap(number int, author string) domain.PRSnapshot {
	return domain.PRSnapshot{Number: number, Author: author, State: "OPEN"}
}

// numbersOf indexes a page by PR number, which is what the assertions above
// name rows by.
func numbersOf(prs []domain.PRSummaryRow) map[int]bool {
	out := make(map[int]bool, len(prs))
	for _, p := range prs {
		out[p.Number] = true
	}
	return out
}
