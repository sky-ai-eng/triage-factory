package projectclassify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// isolateHome redirects ~ to a fresh tempdir for the test. Required
// for any test that runs Classify, because readProjectKB resolves
// project IDs under ~/.triagefactory/projects/<id>/ — without
// isolation, a developer machine with real project dirs could leak
// real KB files into the test run, producing flaky truncation flags
// and unintended Stage 2 escalations.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func mkdirAll(t *testing.T, path string) error {
	t.Helper()
	return os.MkdirAll(path, 0o755)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stubStage1 swaps runStage1Haiku for the duration of a test, restoring
// the real implementation when t.Cleanup fires. Scoring is keyed off
// the project name embedded in the prompt so different projects can
// return different scores.
func stubStage1(t *testing.T, scoresByProjectName map[string]int) *callRecorder {
	t.Helper()
	rec := &callRecorder{}
	orig := runStage1Haiku
	runStage1Haiku = func(_ context.Context, orgID, prompt string, _ agentproc.SecretsReader) (int, string, error) {
		rec.record(prompt)
		rec.mu.Lock()
		rec.orgIDs = append(rec.orgIDs, orgID)
		rec.mu.Unlock()
		for name, score := range scoresByProjectName {
			if strings.Contains(prompt, "<project_name>\n"+name+"\n</project_name>") {
				return score, "stage1 stub for " + name, nil
			}
		}
		return 0, "no stage1 stub match", nil
	}
	t.Cleanup(func() { runStage1Haiku = orig })
	return rec
}

// stubStage2 swaps runStage2Haiku for the duration of a test. Stage 2
// gets cwd as a second arg; the stub records both prompt and cwd so
// tests can verify the agent ran in the expected project dir.
func stubStage2(t *testing.T, scoresByProjectName map[string]int) *callRecorder {
	t.Helper()
	rec := &callRecorder{}
	orig := runStage2Haiku
	runStage2Haiku = func(_ context.Context, orgID, prompt, cwd string, _ agentproc.SecretsReader) (int, string, error) {
		rec.record(prompt)
		rec.mu.Lock()
		rec.cwds = append(rec.cwds, cwd)
		rec.orgIDs = append(rec.orgIDs, orgID)
		rec.mu.Unlock()
		for name, score := range scoresByProjectName {
			if strings.Contains(prompt, "<project_name>\n"+name+"\n</project_name>") {
				return score, "stage2 stub for " + name, nil
			}
		}
		return 0, "no stage2 stub match", nil
	}
	t.Cleanup(func() { runStage2Haiku = orig })
	return rec
}

type callRecorder struct {
	mu     sync.Mutex
	calls  int
	prompt []string
	cwds   []string
	orgIDs []string
}

func (c *callRecorder) record(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.prompt = append(c.prompt, p)
}

func (c *callRecorder) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *callRecorder) orgIDList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.orgIDs))
	copy(out, c.orgIDs)
	return out
}

// TestClassify_OrgIDFlowsToHaiku pins the SKY-323 contract: the orgID
// passed to Classify is threaded down through the per-project vote
// fan-out into the Haiku invocation, so agentproc.Run resolves the
// right tenant's credentials. A regression that dropped the orgID
// somewhere in the chain (Classify → runVotes → voteStage1 →
// runStage1Haiku) would silently bill the wrong org in multi-mode.
func TestClassify_OrgIDFlowsToHaiku(t *testing.T) {
	isolateHome(t)
	stage1 := stubStage1(t, map[string]int{"A": 80, "B": 20})
	stubStage2(t, nil)

	projects := []domain.Project{
		{ID: "p-a", Name: "A"},
		{ID: "p-b", Name: "B"},
	}
	const orgID = "11111111-2222-3333-4444-555555555555"
	Classify(context.Background(), orgID, nil, projects, domain.Entity{Title: "X"})

	got := stage1.orgIDList()
	if len(got) != 2 {
		t.Fatalf("stage1 received %d calls; want 2 (one per project)", len(got))
	}
	for i, g := range got {
		if g != orgID {
			t.Errorf("stage1 call[%d] orgID = %q; want %q", i, g, orgID)
		}
	}
}

func TestClassify_WinnerAboveThreshold(t *testing.T) {
	isolateHome(t)
	stubStage1(t, map[string]int{
		"Auth Migration": 85,
		"Misc Work":      20,
	})
	stage2 := stubStage2(t, nil)

	projects := []domain.Project{
		{ID: "p-auth", Name: "Auth Migration", Description: "Replace session storage with JWT"},
		{ID: "p-misc", Name: "Misc Work", Description: ""},
	}
	entity := domain.Entity{
		ID:     "e1",
		Source: "github", SourceID: "owner/repo#42",
		Title: "Migrate session token validation",
	}

	winner, votes := Classify(context.Background(), "test-org", nil, projects,entity)
	if winner == nil {
		t.Fatalf("expected winner, got nil; votes: %+v", votes)
	}
	if *winner != "p-auth" {
		t.Errorf("winner = %s, want p-auth", *winner)
	}
	if stage2.callCount() != 0 {
		t.Errorf("stage2 should not fire when stage1 has a winner; got %d calls", stage2.callCount())
	}
}

func TestClassify_AllBelowThreshold_ReturnsNil(t *testing.T) {
	isolateHome(t)
	stubStage1(t, map[string]int{
		"Misc Work":     20,
		"Other Project": 45,
	})
	stage2 := stubStage2(t, nil)

	projects := []domain.Project{
		{ID: "p1", Name: "Misc Work"},
		{ID: "p2", Name: "Other Project"},
	}
	entity := domain.Entity{ID: "e1", Title: "Random PR"}

	winner, votes := Classify(context.Background(), "test-org", nil, projects,entity)
	if winner != nil {
		t.Errorf("expected nil winner, got %s", *winner)
	}
	if len(votes) != 2 {
		t.Errorf("expected 2 votes, got %d", len(votes))
	}
	// No truncated KBs in this fixture, so no stage 2 either.
	if stage2.callCount() != 0 {
		t.Errorf("stage2 should not fire without truncated KBs; got %d calls", stage2.callCount())
	}
}

func TestClassify_HighestAboveThresholdWins(t *testing.T) {
	isolateHome(t)
	stubStage1(t, map[string]int{
		"P1": 65,
		"P2": 90,
		"P3": 70,
	})
	stubStage2(t, nil)

	projects := []domain.Project{
		{ID: "p1", Name: "P1"},
		{ID: "p2", Name: "P2"},
		{ID: "p3", Name: "P3"},
	}
	winner, _ := Classify(context.Background(), "test-org", nil, projects,domain.Entity{Title: "X"})
	if winner == nil || *winner != "p2" {
		got := "<nil>"
		if winner != nil {
			got = *winner
		}
		t.Errorf("winner = %s, want p2", got)
	}
}

func TestClassify_TiesGoToFirstReturned(t *testing.T) {
	isolateHome(t)
	stubStage1(t, map[string]int{
		"Alpha": 75,
		"Beta":  75,
	})
	stubStage2(t, nil)

	projects := []domain.Project{
		{ID: "p-alpha", Name: "Alpha"},
		{ID: "p-beta", Name: "Beta"},
	}
	winner, _ := Classify(context.Background(), "test-org", nil, projects,domain.Entity{Title: "X"})
	if winner == nil {
		t.Fatal("expected winner")
	}
	if *winner != "p-alpha" {
		t.Errorf("expected p-alpha (first-returned tie), got %s", *winner)
	}
}

func TestClassify_NoProjects_ReturnsNilNoVotes(t *testing.T) {
	winner, votes := Classify(context.Background(), "test-org", nil, nil, domain.Entity{Title: "X"})
	if winner != nil {
		t.Errorf("expected nil winner")
	}
	if len(votes) != 0 {
		t.Errorf("expected zero votes, got %d", len(votes))
	}
}

func TestClassify_HaikuErrorTreatedAsNoVote(t *testing.T) {
	isolateHome(t)
	origS1 := runStage1Haiku
	t.Cleanup(func() { runStage1Haiku = origS1 })
	runStage1Haiku = func(_ context.Context, _ string, prompt string, _ agentproc.SecretsReader) (int, string, error) {
		if strings.Contains(prompt, "<project_name>\nFlaky\n</project_name>") {
			return 0, "", errors.New("simulated CLI failure")
		}
		return 80, "ok", nil
	}
	stubStage2(t, nil)

	projects := []domain.Project{
		{ID: "p-flaky", Name: "Flaky"},
		{ID: "p-good", Name: "Healthy"},
	}
	winner, votes := Classify(context.Background(), "test-org", nil, projects,domain.Entity{Title: "X"})
	if winner == nil || *winner != "p-good" {
		got := "<nil>"
		if winner != nil {
			got = *winner
		}
		t.Errorf("winner = %s, want p-good", got)
	}
	for _, v := range votes {
		if v.ProjectID == "p-flaky" && v.Err == nil {
			t.Errorf("flaky vote should carry Err")
		}
	}
}

// TestClassify_Stage2EscalatesOnBorderlineTruncated verifies that a
// borderline (40-59) Stage 1 vote with a truncated KB triggers Stage 2,
// and that the Stage 2 result supersedes the Stage 1 one in the
// returned vote slice. Uses a real on-disk KB larger than
// kbInlineMaxBytes so the truncation flag is exercised through the
// production code path.
func TestClassify_Stage2EscalatesOnBorderlineTruncated(t *testing.T) {
	isolateHome(t)
	projectID := "p-border"
	kbDir := fmt.Sprintf("%s/.triagefactory/projects/%s/knowledge-base", os.Getenv("HOME"), projectID)
	if err := mkdirAll(t, kbDir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a single .md file larger than the inline cap so readProjectKB
	// returns truncated=true.
	bigContent := strings.Repeat("x", kbInlineMaxBytes+1024)
	writeFile(t, kbDir+"/big.md", bigContent)

	stubStage1(t, map[string]int{
		"Borderline": 50, // Borderline score → stage 2 candidate when truncated.
	})
	stage2Calls := stubStage2(t, map[string]int{
		"Borderline": 80, // Stage 2 promotes the borderline project past threshold.
	})

	projects := []domain.Project{
		{ID: projectID, Name: "Borderline", Description: "Big-KB project"},
	}
	winner, votes := Classify(context.Background(), "test-org", nil, projects,domain.Entity{Title: "X"})
	if winner == nil || *winner != projectID {
		got := "<nil>"
		if winner != nil {
			got = *winner
		}
		t.Errorf("winner = %s, want %s", got, projectID)
	}
	if stage2Calls.callCount() != 1 {
		t.Errorf("expected exactly 1 stage 2 call, got %d", stage2Calls.callCount())
	}
	if len(votes) != 1 {
		t.Fatalf("expected 1 vote, got %d", len(votes))
	}
	if votes[0].Stage != 2 {
		t.Errorf("merged vote stage = %d, want 2", votes[0].Stage)
	}
	if votes[0].Score != 80 {
		t.Errorf("merged score = %d, want 80", votes[0].Score)
	}
}

// TestClassify_Stage2DoesNotFireWithoutTruncation verifies that a
// borderline score with a NON-truncated KB does NOT escalate to
// Stage 2. The premise of escalation is "the model might have scored
// higher with more context"; if it already had the full KB, Stage 2
// can't help.
func TestClassify_Stage2DoesNotFireWithoutTruncation(t *testing.T) {
	isolateHome(t)
	stubStage1(t, map[string]int{
		"NotTruncated": 50, // Borderline score…
	})
	stage2 := stubStage2(t, map[string]int{
		"NotTruncated": 80,
	})

	// readProjectKB returns truncated=false when the KB doesn't exist
	// on disk, which is the default in unit tests with a temp HOME —
	// but we don't even need to set HOME here because no KB dir means
	// no truncation either way.
	projects := []domain.Project{
		{ID: "p-nt", Name: "NotTruncated"},
	}
	winner, _ := Classify(context.Background(), "test-org", nil, projects,domain.Entity{Title: "X"})
	if winner != nil {
		t.Errorf("expected nil winner without escalation, got %s", *winner)
	}
	if stage2.callCount() != 0 {
		t.Errorf("stage2 should not fire without truncated KB; got %d calls", stage2.callCount())
	}
}

// TestClassifyPrompt_IncludesCalibrationLanguage is a regression guard
// against accidentally weakening the prompt's "score LOW when uncertain"
// posture. The exact phrase is what makes "always vote, even on
// thin-context projects" safe — if it goes missing, name-only projects
// could over-claim entities. SKY-220.
func TestClassifyPrompt_IncludesCalibrationLanguage(t *testing.T) {
	must := []string{
		"Lack of information is a reason to score LOW",
		"score below 30",
		"Do NOT round up",
	}
	// Both stage prompts must include the calibration block.
	stage1 := fmt.Sprintf(stage1Prompt, "", "", "", "", "", "", "", "")
	stage2 := fmt.Sprintf(stage2Prompt, "", "", "", "", "", "", "")
	for _, snippet := range must {
		if !strings.Contains(stage1, snippet) {
			t.Errorf("stage 1 prompt missing calibration phrase %q", snippet)
		}
		if !strings.Contains(stage2, snippet) {
			t.Errorf("stage 2 prompt missing calibration phrase %q", snippet)
		}
	}
}
