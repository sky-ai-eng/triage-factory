package projectclassify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// isolateHome redirects ~ to a fresh tempdir for the test. Required
// for any test that runs classify, because readProjectKB resolves
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

// stage1Stub returns a stage1Func that scores by the project name embedded in
// the prompt, plus a recorder capturing each call (prompt + orgID). It mirrors
// the previous stubStage1 but as a plain closure assigned to a Runner's
// stage1Fn field rather than a package-level mutable var.
func stage1Stub(scoresByProjectName map[string]int) (stage1Func, *callRecorder) {
	rec := &callRecorder{}
	fn := func(_ context.Context, orgID, prompt string) (int, string, error) {
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
	return fn, rec
}

// stage2Stub returns a stage2Func that scores by project name. Stage 2 gets a
// cwd; the recorder captures it so a test can verify the agent ran in the
// expected project dir.
func stage2Stub(scoresByProjectName map[string]int) (stage2Func, *callRecorder) {
	rec := &callRecorder{}
	fn := func(_ context.Context, orgID, prompt, cwd string) (int, string, error) {
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
	return fn, rec
}

// stubRunner builds a Runner for orgID with both stage funcs stubbed to score
// by the project name embedded in the prompt. Either map may be nil. The
// entity/project stores are nil — classify() takes projects as a param and
// never reads the stores, so a stub-driven classify needs no DB. Returns the
// runner plus the stage1 and stage2 call recorders.
func stubRunner(orgID string, stage1, stage2 map[string]int) (*Runner, *callRecorder, *callRecorder) {
	s1, rec1 := stage1Stub(stage1)
	s2, rec2 := stage2Stub(stage2)
	r := NewRunner(nil, nil, orgID, nil, nil, nil)
	r.stage1Fn = s1
	r.stage2Fn = s2
	return r, rec1, rec2
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

// TestClassify_OrgIDFlowsToHaiku pins the contract that the Runner's org
// is threaded down through the per-project vote fan-out into the stage call,
// so agentproc.Run resolves the right tenant's credentials. A regression that
// dropped the orgID somewhere in the chain (classify → runVotes → voteStage1 →
// stage1Fn) would silently bill the wrong org in multi-mode.
func TestClassify_OrgIDFlowsToHaiku(t *testing.T) {
	isolateHome(t)
	const orgID = "11111111-2222-3333-4444-555555555555"
	r, stage1, _ := stubRunner(orgID, map[string]int{"A": 80, "B": 20}, nil)

	projects := []domain.Project{
		{ID: "p-a", Name: "A"},
		{ID: "p-b", Name: "B"},
	}
	r.classify(context.Background(), projects, domain.Entity{Title: "X"})

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
	r, _, stage2 := stubRunner("test-org", map[string]int{
		"Auth Migration": 85,
		"Misc Work":      20,
	}, nil)

	projects := []domain.Project{
		{ID: "p-auth", Name: "Auth Migration", Description: "Replace session storage with JWT"},
		{ID: "p-misc", Name: "Misc Work", Description: ""},
	}
	entity := domain.Entity{
		ID:     "e1",
		Source: "github", SourceID: "owner/repo#42",
		Title: "Migrate session token validation",
	}

	winner, votes := r.classify(context.Background(), projects, entity)
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
	r, _, stage2 := stubRunner("test-org", map[string]int{
		"Misc Work":     20,
		"Other Project": 45,
	}, nil)

	projects := []domain.Project{
		{ID: "p1", Name: "Misc Work"},
		{ID: "p2", Name: "Other Project"},
	}
	entity := domain.Entity{ID: "e1", Title: "Random PR"}

	winner, votes := r.classify(context.Background(), projects, entity)
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
	r, _, _ := stubRunner("test-org", map[string]int{
		"P1": 65,
		"P2": 90,
		"P3": 70,
	}, nil)

	projects := []domain.Project{
		{ID: "p1", Name: "P1"},
		{ID: "p2", Name: "P2"},
		{ID: "p3", Name: "P3"},
	}
	winner, _ := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
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
	r, _, _ := stubRunner("test-org", map[string]int{
		"Alpha": 75,
		"Beta":  75,
	}, nil)

	projects := []domain.Project{
		{ID: "p-alpha", Name: "Alpha"},
		{ID: "p-beta", Name: "Beta"},
	}
	winner, _ := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
	if winner == nil {
		t.Fatal("expected winner")
	}
	if *winner != "p-alpha" {
		t.Errorf("expected p-alpha (first-returned tie), got %s", *winner)
	}
}

func TestClassify_NoProjects_ReturnsNilNoVotes(t *testing.T) {
	r, _, _ := stubRunner("test-org", nil, nil)
	winner, votes := r.classify(context.Background(), nil, domain.Entity{Title: "X"})
	if winner != nil {
		t.Errorf("expected nil winner")
	}
	if len(votes) != 0 {
		t.Errorf("expected zero votes, got %d", len(votes))
	}
}

func TestClassify_HaikuErrorTreatedAsNoVote(t *testing.T) {
	isolateHome(t)
	r, _, _ := stubRunner("test-org", nil, nil)
	// Override stage1 with a per-project failure: "Flaky" errors, others score 80.
	r.stage1Fn = func(_ context.Context, _, prompt string) (int, string, error) {
		if strings.Contains(prompt, "<project_name>\nFlaky\n</project_name>") {
			return 0, "", errors.New("simulated CLI failure")
		}
		return 80, "ok", nil
	}

	projects := []domain.Project{
		{ID: "p-flaky", Name: "Flaky"},
		{ID: "p-good", Name: "Healthy"},
	}
	winner, votes := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
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

	r, _, stage2Calls := stubRunner("test-org",
		map[string]int{"Borderline": 50}, // Borderline score → stage 2 candidate when truncated.
		map[string]int{"Borderline": 80}, // Stage 2 promotes the borderline project past threshold.
	)

	projects := []domain.Project{
		{ID: projectID, Name: "Borderline", Description: "Big-KB project"},
	}
	winner, votes := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
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
	r, _, stage2 := stubRunner("test-org",
		map[string]int{"NotTruncated": 50}, // Borderline score…
		map[string]int{"NotTruncated": 80},
	)

	// readProjectKB returns truncated=false when the KB doesn't exist
	// on disk, which is the default in unit tests with a temp HOME.
	projects := []domain.Project{
		{ID: "p-nt", Name: "NotTruncated"},
	}
	winner, _ := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
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
// could over-claim entities.
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
