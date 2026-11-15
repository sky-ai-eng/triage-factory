package delegate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
)

const (
	kbOrg  = "org-1"
	kbTeam = "team-alpha"
)

// stagingSpawner is a spawner with nothing but the KB seam wired — the whole
// input to staging is (orgID, teamID) and the store, so nothing else is needed
// to exercise it.
func stagingSpawner(t *testing.T, kb kbstore.KB, teams db.TeamsStore) *Spawner {
	t.Helper()
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetTeamKB(kb)
	s.teams = teams
	return s
}

func writeKB(t *testing.T, kb kbstore.KB, teamID string, root kbstore.Root, path, body string) {
	t.Helper()
	if err := kb.Put(context.Background(), kbOrg, teamID, kbstore.Ref{Root: root, Path: path}, strings.NewReader(body)); err != nil {
		t.Fatalf("seed %s/%s: %v", root, path, err)
	}
}

// stagedTree lists the files staged under _tfac/knowledge/, relative to it.
func stagedTree(t *testing.T, cwd string) []string {
	t.Helper()
	root := filepath.Join(cwd, scratchDirName, knowledgeDirName)
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk staged knowledge: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestStageTeamKnowledge_TeamAndOrgRoots is the layout contract: the task
// team's two roots keep their names under team/, and every other team
// contributes only its published root, under org/<slug>/.
func TestStageTeamKnowledge_TeamAndOrgRoots(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "architecture.md", "# How it fits together\n\nmore")
	writeKB(t, kb, kbTeam, kbstore.RootShared, "conventions.md", "Naming and review conventions")
	writeKB(t, kb, "team-platform", kbstore.RootShared, "slo/latency.md", "What we promise")
	// Another team's PRIVATE notes are never staged into anyone else's run.
	writeKB(t, kb, "team-platform", kbstore.RootPrivate, "secret.md", "not yours")

	s := stagingSpawner(t, kb, fakeTeams{teams: []domain.Team{
		{ID: kbTeam, Slug: "alpha", Name: "Alpha"},
		{ID: "team-platform", Slug: "platform", Name: "Platform"},
	}})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)

	want := []string{
		"org/platform/slo/latency.md",
		"team/private/architecture.md",
		"team/shared/conventions.md",
	}
	if got := stagedTree(t, cwd); !equalStrings(got, want) {
		t.Fatalf("staged tree = %v; want %v", got, want)
	}
}

// TestStageTeamKnowledge_ResolvesFromTheTeamAlone: two runs differing only in
// their task's team get different knowledge, and nothing else is consulted.
func TestStageTeamKnowledge_ResolvesFromTheTeamAlone(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, "team-a", kbstore.RootPrivate, "a.md", "alpha only")
	writeKB(t, kb, "team-b", kbstore.RootPrivate, "b.md", "beta only")
	teams := fakeTeams{teams: []domain.Team{
		{ID: "team-a", Slug: "a", Name: "A"},
		{ID: "team-b", Slug: "b", Name: "B"},
	}}
	s := stagingSpawner(t, kb, teams)

	cwdA := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, "team-a", cwdA)
	if got := stagedTree(t, cwdA); !equalStrings(got, []string{"team/private/a.md"}) {
		t.Fatalf("team-a run staged %v; want only its own private note", got)
	}

	cwdB := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, "team-b", cwdB)
	if got := stagedTree(t, cwdB); !equalStrings(got, []string{"team/private/b.md"}) {
		t.Fatalf("team-b run staged %v; want only its own private note", got)
	}
}

func TestStageTeamKnowledge_EmptyStagesNothing(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); len(got) != 0 {
		t.Fatalf("staged %v from an empty KB", got)
	}
}

// TestStageTeamKnowledge_NoTeamOrNoStoreStagesNothing covers the two wiring
// absences: a task with no team, and a spawner with no KB seam.
func TestStageTeamKnowledge_NoTeamOrNoStoreStagesNothing(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "a.md", "x")

	withKB := stagingSpawner(t, kb, fakeTeams{})
	cwd := t.TempDir()
	withKB.stageTeamKnowledge(context.Background(), kbOrg, "", cwd)
	if got := stagedTree(t, cwd); len(got) != 0 {
		t.Errorf("a task with no team staged %v", got)
	}

	noKB := NewSpawner(nil, db.Stores{}, nil, nil, "")
	cwd2 := t.TempDir()
	noKB.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd2)
	if got := stagedTree(t, cwd2); len(got) != 0 {
		t.Errorf("a spawner with no KB seam staged %v", got)
	}
}

// TestStageTeamKnowledge_RebuildsRatherThanAccumulates: a blueprint's steps
// share one run tree, so a second launch stages into a directory the first one
// already filled. A document deleted from the knowledge base in between must be
// gone from the tree, because the framework block tells the agent to walk it.
func TestStageTeamKnowledge_RebuildsRatherThanAccumulates(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "keep.md", "still true")
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "notes/retracted.md", "wrong, deleted later")
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/private/keep.md", "team/private/notes/retracted.md"}) {
		t.Fatalf("first launch staged %v", got)
	}

	// The document is retracted between the two steps.
	if _, err := kb.Delete(context.Background(), kbOrg, kbTeam, kbstore.Ref{Root: kbstore.RootPrivate, Path: "notes/retracted.md"}); err != nil {
		t.Fatalf("delete from the KB: %v", err)
	}

	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/private/keep.md"}) {
		t.Fatalf("second launch left %v; a retracted document must not stay readable", got)
	}
	// The folder it lived in goes too — an object store has no empty folders,
	// so leaving one here would make the tree depend on the deployment mode.
	if _, err := os.Stat(filepath.Join(cwd, scratchDirName, knowledgeDirName, "team", "private", "notes")); !os.IsNotExist(err) {
		t.Errorf("an emptied folder survived the rebuild (err=%v)", err)
	}
}

// TestStageTeamKnowledge_PublishBetweenLaunchesDoesNotDuplicate is the sharp
// half of the same rule. The root a document sits under is a claim about who
// else can see it, so a tree carrying the same file under both roots teaches
// the agent something false about material it may go on to quote.
func TestStageTeamKnowledge_PublishBetweenLaunchesDoesNotDuplicate(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "runbooks/deploy.md", "steps")
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/private/runbooks/deploy.md"}) {
		t.Fatalf("first launch staged %v", got)
	}

	// A human publishes it while the workflow run is between steps.
	if _, err := kb.Move(context.Background(), kbOrg, kbTeam,
		kbstore.Ref{Root: kbstore.RootPrivate, Path: "runbooks"},
		kbstore.Ref{Root: kbstore.RootShared, Path: "runbooks"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/shared/runbooks/deploy.md"}) {
		t.Fatalf("after a publish the tree holds %v; the document must appear under exactly one root", got)
	}
}

// TestStageTeamKnowledge_DiscardsWhatTheAgentLeft pins the promise the
// framework block makes to the agent: nothing it writes under the knowledge
// tree is kept. Without the rebuild that held only for the first launch into a
// run root.
func TestStageTeamKnowledge_DiscardsWhatTheAgentLeft(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootShared, "conventions.md", "ours")
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)

	scribble := filepath.Join(cwd, scratchDirName, knowledgeDirName, "team", "shared", "my-edit.md")
	if err := os.WriteFile(scribble, []byte("the agent's own"), 0o644); err != nil {
		t.Fatalf("write the agent's file: %v", err)
	}

	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/shared/conventions.md"}) {
		t.Fatalf("tree = %v; an agent's own file under the knowledge tree is discarded", got)
	}
}

// TestStageTeamKnowledge_UnusableSlugFallsBackToTheID: the staged tree can
// never gain a level or escape its root because of what somebody named a team.
func TestStageTeamKnowledge_UnusableSlugFallsBackToTheID(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, "team-odd", kbstore.RootShared, "note.md", "x")
	s := stagingSpawner(t, kb, fakeTeams{teams: []domain.Team{
		{ID: kbTeam, Slug: "alpha", Name: "Alpha"},
		{ID: "team-odd", Slug: "../escape", Name: "Odd"},
	}})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"org/team-odd/note.md"}) {
		t.Fatalf("staged %v; a traversal slug must fall back to the team id", got)
	}
}

// TestCopyKnowledgeFile_RemovesItsFragment: the caller logs and skips an entry
// whose copy failed, so a truncated file left behind would be a document nobody
// staged, sitting in a tree the agent is told to walk.
func TestCopyKnowledgeFile_RemovesItsFragment(t *testing.T) {
	s := stagingSpawner(t, kbstore.NewLocalAt(t.TempDir()), fakeTeams{})
	dst := filepath.Join(t.TempDir(), "staged", "runbook.md")

	_, err := s.copyKnowledgeFile(context.Background(), failingKB{after: 6}, kbOrg, kbTeam,
		kbstore.Ref{Root: kbstore.RootPrivate, Path: "runbook.md"}, dst)
	if err == nil {
		t.Fatalf("expected the copy to fail")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("a truncated document survived a failed copy (err=%v)", statErr)
	}
}

// failingKB streams a few bytes and then dies, standing in for the object
// backend losing its connection mid-body — the way this fails in multi mode.
type failingKB struct {
	kbstore.KB
	after int
}

func (f failingKB) Open(context.Context, string, string, kbstore.Ref) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(
		strings.NewReader(strings.Repeat("x", f.after)),
		errReader{},
	)), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset mid-body") }

// fakeTeams answers ListActiveForOrgSystem and nothing else — staging is the
// only caller, and it reads exactly that one method.
type fakeTeams struct {
	db.TeamsStore
	teams []domain.Team
	err   error
}

func (f fakeTeams) ListActiveForOrgSystem(_ context.Context, _ string) ([]domain.Team, error) {
	return f.teams, f.err
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
