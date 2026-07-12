package projectclassify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// isolateHome redirects ~ to a fresh tempdir for the test. Required
// for any test that runs classify, because readProjectKB resolves
// project IDs under ~/.triagefactory/projects/<id>/ — without
// isolation, a developer machine with real project dirs could leak
// real KB files into the test run, producing flaky truncation flags.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
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

// stubRunner builds a Runner for orgID with the stage1 func stubbed to score
// by the project name embedded in the prompt. The map may be nil. The
// entity/project stores are nil — classify() takes projects as a param and
// never reads the stores, so a stub-driven classify needs no DB. Returns the
// runner plus the stage1 call recorder.
func stubRunner(orgID string, stage1 map[string]int) (*Runner, *callRecorder) {
	s1, rec1 := stage1Stub(stage1)
	r := NewRunner(nil, nil, orgID, nil, nil, nil, nil)
	r.stage1Fn = s1
	return r, rec1
}

type callRecorder struct {
	mu     sync.Mutex
	calls  int
	prompt []string
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
	r, stage1 := stubRunner(orgID, map[string]int{"A": 80, "B": 20})

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
	r, _ := stubRunner("test-org", map[string]int{
		"Auth Migration": 85,
		"Misc Work":      20,
	})

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
}

func TestClassify_AllBelowThreshold_ReturnsNil(t *testing.T) {
	isolateHome(t)
	r, _ := stubRunner("test-org", map[string]int{
		"Misc Work":     20,
		"Other Project": 45,
	})

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
}

func TestClassify_HighestAboveThresholdWins(t *testing.T) {
	isolateHome(t)
	r, _ := stubRunner("test-org", map[string]int{
		"P1": 65,
		"P2": 90,
		"P3": 70,
	})

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

// TestClassify_ExactTieUnassigned pins the "can't tell" rule: when two or
// more projects share the exact top above-threshold score, the entity
// resolves to unassigned rather than a coin-flip pick — project_id drives
// team visibility on the entity, so classifier uncertainty must not widen
// who can see it.
func TestClassify_ExactTieUnassigned(t *testing.T) {
	isolateHome(t)
	r, _ := stubRunner("test-org", map[string]int{
		"Alpha": 75,
		"Beta":  75,
	})

	projects := []domain.Project{
		{ID: "p-alpha", Name: "Alpha"},
		{ID: "p-beta", Name: "Beta"},
	}
	winner, votes := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
	if winner != nil {
		t.Errorf("expected nil winner on exact tie, got %s", *winner)
	}
	if len(votes) != 2 {
		t.Errorf("expected 2 votes, got %d", len(votes))
	}
}

// TestClassify_ThreeWayExactTieUnassigned extends the two-way tie case to
// three tied projects — the rule is "≥2 tied", not "exactly 2".
func TestClassify_ThreeWayExactTieUnassigned(t *testing.T) {
	isolateHome(t)
	r, _ := stubRunner("test-org", map[string]int{
		"Alpha": 80,
		"Beta":  80,
		"Gamma": 80,
	})

	projects := []domain.Project{
		{ID: "p-alpha", Name: "Alpha"},
		{ID: "p-beta", Name: "Beta"},
		{ID: "p-gamma", Name: "Gamma"},
	}
	winner, _ := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
	if winner != nil {
		t.Errorf("expected nil winner on three-way exact tie, got %s", *winner)
	}
}

// TestClassify_UniqueWinnerWithTiedRunnerUp verifies that a tie BELOW the
// top score doesn't affect the outcome — only a tie for the top score
// forces unassigned.
func TestClassify_UniqueWinnerWithTiedRunnerUp(t *testing.T) {
	isolateHome(t)
	r, _ := stubRunner("test-org", map[string]int{
		"Winner": 90,
		"Second": 70,
		"Third":  70,
	})

	projects := []domain.Project{
		{ID: "p-winner", Name: "Winner"},
		{ID: "p-second", Name: "Second"},
		{ID: "p-third", Name: "Third"},
	}
	winner, _ := r.classify(context.Background(), projects, domain.Entity{Title: "X"})
	if winner == nil || *winner != "p-winner" {
		got := "<nil>"
		if winner != nil {
			got = *winner
		}
		t.Errorf("winner = %s, want p-winner", got)
	}
}

func TestClassify_NoProjects_ReturnsNilNoVotes(t *testing.T) {
	r, _ := stubRunner("test-org", nil)
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
	r, _ := stubRunner("test-org", nil)
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
	stage1 := fmt.Sprintf(stage1Prompt, "", "", "", "", "", "", "", "")
	for _, snippet := range must {
		if !strings.Contains(stage1, snippet) {
			t.Errorf("stage 1 prompt missing calibration phrase %q", snippet)
		}
	}
}
