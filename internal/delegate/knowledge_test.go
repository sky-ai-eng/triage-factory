package delegate

import (
	"context"
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
	manifest := s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)

	want := []string{
		"org/platform/slo/latency.md",
		"team/private/architecture.md",
		"team/shared/conventions.md",
	}
	if got := stagedTree(t, cwd); !equalStrings(got, want) {
		t.Fatalf("staged tree = %v; want %v", got, want)
	}

	// The manifest names what was staged, with each document's first line.
	for _, want := range []string{
		"_tfac/knowledge/",
		"knowledge/team/",
		"private/architecture.md — How it fits together",
		"shared/conventions.md — Naming and review conventions",
		"knowledge/org/platform/ — published by Platform",
		"slo/latency.md — What we promise",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest is missing %q;\n%s", want, manifest)
		}
	}
	if strings.Contains(manifest, "secret.md") {
		t.Errorf("the manifest names another team's private document;\n%s", manifest)
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
	s.stageTeamKnowledge(context.Background(), kbOrg, "team-a", cwdA, nil)
	if got := stagedTree(t, cwdA); !equalStrings(got, []string{"team/private/a.md"}) {
		t.Fatalf("team-a run staged %v; want only its own private note", got)
	}

	cwdB := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, "team-b", cwdB, nil)
	if got := stagedTree(t, cwdB); !equalStrings(got, []string{"team/private/b.md"}) {
		t.Fatalf("team-b run staged %v; want only its own private note", got)
	}
}

// TestStageTeamKnowledge_EmptyStagesNothingAndSaysNothing: the manifest exists
// iff knowledge does, so a run with nothing behind it never mentions
// knowledge at all.
func TestStageTeamKnowledge_EmptyStagesNothingAndSaysNothing(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	if manifest := s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil); manifest != "" {
		t.Fatalf("manifest for an empty KB = %q; want empty", manifest)
	}
	if got := stagedTree(t, cwd); len(got) != 0 {
		t.Fatalf("staged %v from an empty KB", got)
	}
	// And the composed run context says nothing about knowledge either.
	if ctx := runContext("", "/work", "", "", ""); strings.Contains(ctx, knowledgeDirName) {
		t.Fatalf("run context mentioned knowledge with none staged;\n%s", ctx)
	}
}

// TestStageTeamKnowledge_NoTeamOrNoStoreStagesNothing covers the two wiring
// absences: a task with no team, and a spawner with no KB seam.
func TestStageTeamKnowledge_NoTeamOrNoStoreStagesNothing(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "a.md", "x")

	withKB := stagingSpawner(t, kb, fakeTeams{})
	cwd := t.TempDir()
	if m := withKB.stageTeamKnowledge(context.Background(), kbOrg, "", cwd, nil); m != "" {
		t.Errorf("a task with no team staged %q", m)
	}
	if got := stagedTree(t, cwd); len(got) != 0 {
		t.Errorf("a task with no team staged %v", got)
	}

	noKB := NewSpawner(nil, db.Stores{}, nil, nil, "")
	cwd2 := t.TempDir()
	if m := noKB.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd2, nil); m != "" {
		t.Errorf("a spawner with no KB seam staged %q", m)
	}
}

// TestStageTeamKnowledge_RebuildsRatherThanAccumulates: a blueprint's steps
// share one run tree, so a second launch stages into a directory the first one
// already filled. A document deleted from the knowledge base in between must be
// gone from the tree — a leftover is invisible in the manifest (which is
// rendered from what was copied) and present on disk, which is the worst
// combination, because the framework block tells the agent to walk the tree.
func TestStageTeamKnowledge_RebuildsRatherThanAccumulates(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "keep.md", "still true")
	writeKB(t, kb, kbTeam, kbstore.RootPrivate, "notes/retracted.md", "wrong, deleted later")
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/private/keep.md", "team/private/notes/retracted.md"}) {
		t.Fatalf("first launch staged %v", got)
	}

	// The document is retracted between the two steps.
	if _, err := kb.Delete(context.Background(), kbOrg, kbTeam, kbstore.Ref{Root: kbstore.RootPrivate, Path: "notes/retracted.md"}); err != nil {
		t.Fatalf("delete from the KB: %v", err)
	}

	manifest := s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/private/keep.md"}) {
		t.Fatalf("second launch left %v; a retracted document must not stay readable", got)
	}
	if strings.Contains(manifest, "retracted.md") {
		t.Errorf("the manifest names a retracted document;\n%s", manifest)
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
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/private/runbooks/deploy.md"}) {
		t.Fatalf("first launch staged %v", got)
	}

	// A human publishes it while the workflow run is between steps.
	if _, err := kb.Move(context.Background(), kbOrg, kbTeam,
		kbstore.Ref{Root: kbstore.RootPrivate, Path: "runbooks"},
		kbstore.Ref{Root: kbstore.RootShared, Path: "runbooks"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)
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
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)

	scribble := filepath.Join(cwd, scratchDirName, knowledgeDirName, "team", "shared", "my-edit.md")
	if err := os.WriteFile(scribble, []byte("the agent's own"), 0o644); err != nil {
		t.Fatalf("write the agent's file: %v", err)
	}

	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/shared/conventions.md"}) {
		t.Fatalf("tree = %v; an agent's own file under the knowledge tree is discarded", got)
	}
}

// TestStageTeamKnowledge_RebuildKeepsRepoOwnedPaths: the rebuild is a walk
// rather than one RemoveAll for exactly this case. For a GitHub PR run the run
// tree IS the checkout, so deleting a tracked file here would ride the agent's
// next commit into its pull request — a rebuild that "cleaned up" a
// contributor's file would be worse than the staleness it fixes.
func TestStageTeamKnowledge_RebuildKeepsRepoOwnedPaths(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootShared, "other.md", "fine")
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	tracked := filepath.Join(cwd, scratchDirName, knowledgeDirName, "team", "shared", "committed.md")
	if err := os.MkdirAll(filepath.Dir(tracked), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tracked, []byte("the repo's own"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	owned := repoFiles{
		scratchDirName + "/" + knowledgeDirName + "/team/shared/committed.md": true,
	}

	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, owned)

	body, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatalf("the repo-owned file was removed by the rebuild: %v", err)
	}
	if string(body) != "the repo's own" {
		t.Errorf("the repo-owned file was rewritten: %q", body)
	}
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/shared/committed.md", "team/shared/other.md"}) {
		t.Fatalf("tree = %v", got)
	}
}

// TestStageTeamKnowledge_LeavesRepoOwnedPathsAlone: for a GitHub PR run the run
// tree IS the checkout, and an infrastructure write onto a tracked path would
// ride the agent's next commit into its pull request.
func TestStageTeamKnowledge_LeavesRepoOwnedPathsAlone(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootShared, "conventions.md", "ours")
	writeKB(t, kb, kbTeam, kbstore.RootShared, "other.md", "fine")
	s := stagingSpawner(t, kb, fakeTeams{})

	cwd := t.TempDir()
	owned := repoFiles{
		scratchDirName + "/" + knowledgeDirName + "/team/shared/conventions.md": true,
	}
	manifest := s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, owned)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"team/shared/other.md"}) {
		t.Fatalf("staged %v; the repo-owned path must be left alone", got)
	}
	if strings.Contains(manifest, "conventions.md") {
		t.Errorf("the manifest names a file that was not staged;\n%s", manifest)
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
	s.stageTeamKnowledge(context.Background(), kbOrg, kbTeam, cwd, nil)
	if got := stagedTree(t, cwd); !equalStrings(got, []string{"org/team-odd/note.md"}) {
		t.Fatalf("staged %v; a traversal slug must fall back to the team id", got)
	}
}

// TestRunContext_CarriesTheManifest is the composition half: the manifest is
// per-run data and rides <run_context>, never a framework block.
func TestRunContext_CarriesTheManifest(t *testing.T) {
	got := runContext("", "/work", "tfac/<ticket-id>", "", "knowledge/team/ — your team's knowledge base")
	if !strings.Contains(got, "<run_context>") || !strings.Contains(got, "knowledge/team/") {
		t.Fatalf("run context did not carry the manifest;\n%s", got)
	}
	// It renders last, after the run's own facts.
	if strings.Index(got, "Run root:") > strings.Index(got, "knowledge/team/") {
		t.Errorf("the manifest rendered before the run's own facts;\n%s", got)
	}
}

func TestFirstLineSummary(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"# How it fits together\nmore", "How it fits together"},
		{"\n\n  - a bullet first  \nrest", "a bullet first"},
		{"plain prose", "plain prose"},
		{"", ""},
		{"\n\n\n", ""},
		{"> quoted opener", "quoted opener"},
		{strings.Repeat("x", 200), strings.Repeat("x", knowledgeSummaryMaxLen) + "…"},
	} {
		if got := firstLineSummary(tc.in); got != tc.want {
			t.Errorf("firstLineSummary(%.20q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	// A summary can never introduce a line of its own: the manifest is one
	// line per document, and a document whose first line carried a newline
	// would be able to fake an entry.
	if got := firstLineSummary("a\tb   c\nd"); strings.ContainsAny(got, "\n\r") {
		t.Errorf("summary carried a newline: %q", got)
	}
}

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
