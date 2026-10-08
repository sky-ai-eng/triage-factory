package dbtest

import (
	"context"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// LinearTeamRulesFixture is what a backend hands RunLinearTeamRulesConformance:
// the store, plus an org holding two teams so the suite can prove a write to
// one team leaves the other's rows alone. Both teams must start with no rows.
type LinearTeamRulesFixture struct {
	Store       db.LinearTeamRulesStore
	OrgID       string
	TeamID      string
	OtherTeamID string
}

// LinearTeamRulesFactory builds a fresh fixture per subtest.
type LinearTeamRulesFactory func(t *testing.T) LinearTeamRulesFixture

// linearWorkspace is the Linear workspace every subtest saves its rows under,
// except the one about workspaces.
const linearWorkspace = "ws-a"

// linearState builds a state ref whose id is derived from its name, so a test
// names a state once and gets the same ref wherever it appears.
func linearState(name, typ string) domain.LinearStateRef {
	return domain.LinearStateRef{ID: "state-" + name, Name: name, Type: typ}
}

// armedLinearRules is a fully mapped row for linearTeamID.
func armedLinearRules(linearTeamID, key string) domain.LinearTeamRules {
	started := linearState(key+"-started", "started")
	done := linearState(key+"-done", "completed")
	return domain.LinearTeamRules{
		LinearTeamID:        linearTeamID,
		LinearTeamKey:       key,
		LinearTeamName:      key + " Team",
		PickupMembers:       []domain.LinearStateRef{linearState(key+"-todo", "unstarted"), linearState(key+"-backlog", "backlog")},
		InProgressMembers:   []domain.LinearStateRef{started},
		InProgressCanonical: started,
		DoneMembers:         []domain.LinearStateRef{done, linearState(key+"-canceled", "canceled")},
		DoneCanonical:       done,
	}
}

// unarmedLinearRules is a watched row with nothing mapped. The stores hand
// empty members back as empty slices, never nil, so that is what this carries.
func unarmedLinearRules(linearTeamID, key string) domain.LinearTeamRules {
	return domain.LinearTeamRules{
		LinearTeamID:      linearTeamID,
		LinearTeamKey:     key,
		LinearTeamName:    key + " Team",
		PickupMembers:     []domain.LinearStateRef{},
		InProgressMembers: []domain.LinearStateRef{},
		DoneMembers:       []domain.LinearStateRef{},
	}
}

func withTeam(rules []domain.LinearTeamRules, teamID string) []domain.LinearTeamRules {
	out := make([]domain.LinearTeamRules, len(rules))
	for i, r := range rules {
		r.TeamID = teamID
		out[i] = r
	}
	return out
}

func linearTeamIDs(rules []domain.LinearTeamRules) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.LinearTeamID)
	}
	return out
}

// RunLinearTeamRulesConformance is the shared suite for
// db.LinearTeamRulesStore: replace-set semantics (an absent id's row is
// deleted, a present one upserted, another team's rows untouched), ordering,
// the stored set ReplaceForTeam returns, the org union, TracksTeamSystem, the
// armed-or-unarmed CHECK, and every read and write confined to the workspace
// it is given. RLS is the Postgres backend's own test.
func RunLinearTeamRulesConformance(t *testing.T, factory LinearTeamRulesFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("EmptyTeam_ReturnsEmptySlice", func(t *testing.T) {
		f := factory(t)
		for name, list := range map[string]func(context.Context, string, string) ([]domain.LinearTeamRules, error){
			"ListForTeam":       f.Store.ListForTeam,
			"ListForTeamSystem": f.Store.ListForTeamSystem,
		} {
			got, err := list(ctx, f.TeamID, linearWorkspace)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got == nil || len(got) != 0 {
				t.Errorf("%s on an empty team = %#v, want an empty non-nil slice", name, got)
			}
		}
	})

	t.Run("ReplaceForTeam_ReturnsStoredSetInIDOrder", func(t *testing.T) {
		f := factory(t)
		// Input deliberately out of id order: the stored set comes back
		// ordered by linear_team_id, whatever order it was written in.
		input := []domain.LinearTeamRules{
			armedLinearRules("lt-c", "CCC"),
			unarmedLinearRules("lt-a", "AAA"),
			armedLinearRules("lt-b", "BBB"),
		}
		got, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, input)
		if err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		want := withTeam([]domain.LinearTeamRules{input[1], input[2], input[0]}, f.TeamID)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ReplaceForTeam returned\n%#v\nwant\n%#v", got, want)
		}
		for name, list := range map[string]func(context.Context, string, string) ([]domain.LinearTeamRules, error){
			"ListForTeam":       f.Store.ListForTeam,
			"ListForTeamSystem": f.Store.ListForTeamSystem,
		} {
			read, err := list(ctx, f.TeamID, linearWorkspace)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !reflect.DeepEqual(read, got) {
				t.Errorf("%s after the write =\n%#v\nwant what ReplaceForTeam returned\n%#v", name, read, got)
			}
		}
	})

	t.Run("ReplaceForTeam_IsAReplaceSet", func(t *testing.T) {
		f := factory(t)
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{
			armedLinearRules("lt-a", "AAA"),
			armedLinearRules("lt-b", "BBB"),
			unarmedLinearRules("lt-c", "CCC"),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		other := []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA"), armedLinearRules("lt-z", "ZZZ")}
		if _, err := f.Store.ReplaceForTeam(ctx, f.OtherTeamID, linearWorkspace, other); err != nil {
			t.Fatalf("seed other team: %v", err)
		}

		// lt-a and lt-c are absent, so their rows go. lt-b is present with a
		// renamed key and its rules cleared, so it is upserted to that. lt-d is
		// new.
		renamed := unarmedLinearRules("lt-b", "BB2")
		renamed.LinearTeamName = "Renamed"
		got, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{renamed, armedLinearRules("lt-d", "DDD")})
		if err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		want := withTeam([]domain.LinearTeamRules{renamed, armedLinearRules("lt-d", "DDD")}, f.TeamID)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after replace =\n%#v\nwant\n%#v", got, want)
		}

		otherGot, err := f.Store.ListForTeamSystem(ctx, f.OtherTeamID, linearWorkspace)
		if err != nil {
			t.Fatalf("ListForTeamSystem(other): %v", err)
		}
		if !reflect.DeepEqual(otherGot, withTeam(other, f.OtherTeamID)) {
			t.Errorf("the other team's rows changed:\n%#v\nwant\n%#v", otherGot, withTeam(other, f.OtherTeamID))
		}
	})

	t.Run("ReplaceForTeam_EmptyClearsOnlyThatTeam", func(t *testing.T) {
		f := factory(t)
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA")}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.Store.ReplaceForTeam(ctx, f.OtherTeamID, linearWorkspace, []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA")}); err != nil {
			t.Fatalf("seed other team: %v", err)
		}
		got, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, nil)
		if err != nil {
			t.Fatalf("ReplaceForTeam(nil): %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("ReplaceForTeam(nil) returned %#v, want an empty non-nil slice", got)
		}
		otherGot, err := f.Store.ListForTeamSystem(ctx, f.OtherTeamID, linearWorkspace)
		if err != nil {
			t.Fatalf("ListForTeamSystem(other): %v", err)
		}
		if len(otherGot) != 1 {
			t.Errorf("clearing one team removed the other's rows: %#v", otherGot)
		}
	})

	t.Run("ReplaceForTeam_RefusesAnEntryWithNoID", func(t *testing.T) {
		f := factory(t)
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA")}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{armedLinearRules("", "AAA")}); err == nil {
			t.Fatal("ReplaceForTeam accepted an entry with no LinearTeamID")
		}
		got, err := f.Store.ListForTeamSystem(ctx, f.TeamID, linearWorkspace)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("a refused write changed the stored set: %#v", got)
		}
	})

	t.Run("ReplaceForTeam_RefusesARepeatedID", func(t *testing.T) {
		f := factory(t)
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA")}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		twice := []domain.LinearTeamRules{armedLinearRules("lt-b", "BBB"), armedLinearRules("lt-b", "BB2")}
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, twice); err == nil {
			t.Fatal("ReplaceForTeam accepted one Linear team twice")
		}
		got, err := f.Store.ListForTeamSystem(ctx, f.TeamID, linearWorkspace)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].LinearTeamID != "lt-a" {
			t.Errorf("a refused write changed the stored set: %#v", got)
		}
	})

	t.Run("ReplaceForTeam_RefusesHalfAMapping", func(t *testing.T) {
		// The armed-or-unarmed CHECK, in both dialects: pickup with nothing
		// to move an issue into, or the reverse, never lands.
		f := factory(t)
		noDone := armedLinearRules("lt-a", "AAA")
		noDone.DoneMembers, noDone.DoneCanonical = nil, domain.LinearStateRef{}
		onlyPickup := unarmedLinearRules("lt-b", "BBB")
		onlyPickup.PickupMembers = []domain.LinearStateRef{linearState("todo", "unstarted")}
		noCanonical := armedLinearRules("lt-c", "CCC")
		noCanonical.InProgressCanonical = domain.LinearStateRef{}
		for name, r := range map[string]domain.LinearTeamRules{
			"no done rule":           noDone,
			"pickup only":            onlyPickup,
			"in_progress, no target": noCanonical,
		} {
			if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{r}); err == nil {
				t.Errorf("%s: ReplaceForTeam stored half a mapping", name)
			}
		}
		got, err := f.Store.ListForTeamSystem(ctx, f.TeamID, linearWorkspace)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("a refused write left rows behind: %#v", got)
		}
	})

	t.Run("ListForOrgSystem_UnionOrderedByLinearTeamThenTeam", func(t *testing.T) {
		f := factory(t)
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{
			armedLinearRules("lt-b", "BBB"), armedLinearRules("lt-a", "AAA"),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.Store.ReplaceForTeam(ctx, f.OtherTeamID, linearWorkspace, []domain.LinearTeamRules{
			armedLinearRules("lt-a", "AAA"), unarmedLinearRules("lt-c", "CCC"),
		}); err != nil {
			t.Fatalf("seed other team: %v", err)
		}
		got, err := f.Store.ListForOrgSystem(ctx, f.OrgID, linearWorkspace)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		lowTeam, highTeam := f.TeamID, f.OtherTeamID
		if highTeam < lowTeam {
			lowTeam, highTeam = highTeam, lowTeam
		}
		type key struct{ linearTeamID, teamID string }
		var gotKeys []key
		for _, r := range got {
			gotKeys = append(gotKeys, key{r.LinearTeamID, r.TeamID})
		}
		wantKeys := []key{{"lt-a", lowTeam}, {"lt-a", highTeam}, {"lt-b", f.TeamID}, {"lt-c", f.OtherTeamID}}
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Errorf("ListForOrgSystem order = %v, want %v", gotKeys, wantKeys)
		}
	})

	t.Run("TracksTeamSystem", func(t *testing.T) {
		f := factory(t)
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, []domain.LinearTeamRules{
			armedLinearRules("lt-a", "AAA"), unarmedLinearRules("lt-b", "BBB"),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.Store.ReplaceForTeam(ctx, f.OtherTeamID, linearWorkspace, []domain.LinearTeamRules{armedLinearRules("lt-z", "ZZZ")}); err != nil {
			t.Fatalf("seed other team: %v", err)
		}
		for _, c := range []struct {
			teamID, linearTeamID string
			want                 bool
		}{
			{f.TeamID, "lt-a", true},
			// Watched and unarmed is still tracked, exactly as a Jira project
			// with a rules row is.
			{f.TeamID, "lt-b", true},
			{f.TeamID, "lt-z", false},
			{f.TeamID, "lt-missing", false},
			{f.OtherTeamID, "lt-z", true},
			{f.OtherTeamID, "lt-a", false},
		} {
			got, err := f.Store.TracksTeamSystem(ctx, c.teamID, linearWorkspace, c.linearTeamID)
			if err != nil {
				t.Fatalf("TracksTeamSystem(%s, %s): %v", c.teamID, c.linearTeamID, err)
			}
			if got != c.want {
				t.Errorf("TracksTeamSystem(%s, %s) = %v, want %v", c.teamID, c.linearTeamID, got, c.want)
			}
		}
	})

	t.Run("Workspace_ConfinesEveryReadAndTheReplace", func(t *testing.T) {
		// The org's credential moved from ws-a to ws-b: the team's ws-a rows
		// stop applying, a save under ws-b leaves them stored, and reading
		// ws-a again — the old workspace bound once more — brings them back.
		f := factory(t)
		old := []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA"), unarmedLinearRules("lt-b", "BBB")}
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, "ws-a", old); err != nil {
			t.Fatalf("seed ws-a: %v", err)
		}
		next := []domain.LinearTeamRules{armedLinearRules("lt-x", "XXX")}
		got, err := f.Store.ReplaceForTeam(ctx, f.TeamID, "ws-b", next)
		if err != nil {
			t.Fatalf("ReplaceForTeam(ws-b): %v", err)
		}
		if !reflect.DeepEqual(got, withTeam(next, f.TeamID)) {
			t.Errorf("ReplaceForTeam(ws-b) returned %#v, want only ws-b's rows", got)
		}

		for _, c := range []struct {
			workspace string
			want      []string
		}{{"ws-a", []string{"lt-a", "lt-b"}}, {"ws-b", []string{"lt-x"}}, {"ws-c", []string{}}, {"", []string{}}} {
			for name, list := range map[string]func(context.Context, string, string) ([]domain.LinearTeamRules, error){
				"ListForTeam":       f.Store.ListForTeam,
				"ListForTeamSystem": f.Store.ListForTeamSystem,
			} {
				rows, err := list(ctx, f.TeamID, c.workspace)
				if err != nil {
					t.Fatalf("%s(%q): %v", name, c.workspace, err)
				}
				if ids := linearTeamIDs(rows); !reflect.DeepEqual(ids, c.want) {
					t.Errorf("%s(%q) = %v, want %v", name, c.workspace, ids, c.want)
				}
			}
			union, err := f.Store.ListForOrgSystem(ctx, f.OrgID, c.workspace)
			if err != nil {
				t.Fatalf("ListForOrgSystem(%q): %v", c.workspace, err)
			}
			if ids := linearTeamIDs(union); !reflect.DeepEqual(ids, c.want) {
				t.Errorf("ListForOrgSystem(%q) = %v, want %v", c.workspace, ids, c.want)
			}
		}
		for _, c := range []struct {
			workspace, linearTeamID string
			want                    bool
		}{{"ws-a", "lt-a", true}, {"ws-b", "lt-a", false}, {"ws-b", "lt-x", true}, {"", "lt-a", false}} {
			got, err := f.Store.TracksTeamSystem(ctx, f.TeamID, c.workspace, c.linearTeamID)
			if err != nil {
				t.Fatalf("TracksTeamSystem(%q, %s): %v", c.workspace, c.linearTeamID, err)
			}
			if got != c.want {
				t.Errorf("TracksTeamSystem(%q, %s) = %v, want %v", c.workspace, c.linearTeamID, got, c.want)
			}
		}

		// Clearing the team under ws-b leaves its ws-a rows stored.
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, "ws-b", nil); err != nil {
			t.Fatalf("ReplaceForTeam(ws-b, nil): %v", err)
		}
		kept, err := f.Store.ListForTeamSystem(ctx, f.TeamID, "ws-a")
		if err != nil {
			t.Fatalf("ListForTeamSystem(ws-a): %v", err)
		}
		if !reflect.DeepEqual(kept, withTeam(old, f.TeamID)) {
			t.Errorf("a ws-b save changed ws-a's rows:\n%#v\nwant\n%#v", kept, withTeam(old, f.TeamID))
		}
		if _, err := f.Store.ReplaceForTeam(ctx, f.TeamID, "", next); err == nil {
			t.Error("ReplaceForTeam accepted an empty workspace id")
		}
	})

	t.Run("ReplaceForTeam_ResendIsIdempotent", func(t *testing.T) {
		f := factory(t)
		input := []domain.LinearTeamRules{armedLinearRules("lt-a", "AAA"), unarmedLinearRules("lt-b", "BBB")}
		first, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, input)
		if err != nil {
			t.Fatalf("first ReplaceForTeam: %v", err)
		}
		second, err := f.Store.ReplaceForTeam(ctx, f.TeamID, linearWorkspace, input)
		if err != nil {
			t.Fatalf("second ReplaceForTeam: %v", err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("resending the same set changed it:\n%#v\nthen\n%#v", first, second)
		}
		if ids := linearTeamIDs(second); !reflect.DeepEqual(ids, []string{"lt-a", "lt-b"}) {
			t.Errorf("ids = %v", ids)
		}
	})
}
