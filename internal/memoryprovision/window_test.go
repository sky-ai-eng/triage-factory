package memoryprovision

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBuildWindow_KeepsHeadAndTailAndMarksTheGap is the windowing rule in one
// pass: the opening turn survives because it is what makes the tail legible,
// the newest rows survive because that is where the state a memory needs
// lives, and the middle is replaced by one line that says how much went.
func TestBuildWindow_KeepsHeadAndTailAndMarksTheGap(t *testing.T) {
	// Each filler row is a fifth of the budget, so the walk keeps the opening
	// turn plus a handful of the newest and drops the rest.
	filler := strings.Repeat("x", windowByteBudget/5)
	rows := []domain.Message{
		{Role: roleUser, Content: "the opening turn: fix the failing build"},
	}
	for i := range 12 {
		rows = append(rows, domain.Message{Role: roleAssistant, Content: fmt.Sprintf("step %d %s", i, filler)})
	}
	rows = append(rows, domain.Message{Role: roleAssistant, Content: "pushed aa/fix-build and opened the pull request"})

	win := buildWindow(rows)

	if !strings.Contains(win.text, "the opening turn: fix the failing build") {
		t.Errorf("the head must carry the opening turn;\n%s", firstLines(win.text, 3))
	}
	if !strings.Contains(win.text, "pushed aa/fix-build and opened the pull request") {
		t.Errorf("the tail must carry the newest row;\n%s", firstLines(win.text, 3))
	}
	if !strings.Contains(win.text, "step 0 ") {
		// The head is the opening turn alone; step 0 is the first thing the
		// gap can eat, and it should have.
		t.Log("step 0 elided, as expected")
	}
	if win.rowsTotal != len(rows) {
		t.Errorf("rows_total = %d, want %d", win.rowsTotal, len(rows))
	}
	if win.rowsSent >= win.rowsTotal {
		t.Fatalf("rows_sent = %d of %d: nothing was elided, so this fixture proves nothing", win.rowsSent, win.rowsTotal)
	}
	marker := fmt.Sprintf("[… %d rows elided …]", win.rowsTotal-win.rowsSent)
	if !strings.Contains(win.text, marker) {
		t.Errorf("want the elision marker %q in the window;\n%s", marker, firstLines(win.text, 5))
	}
	if got := len(win.text); got > windowByteBudget {
		t.Errorf("window is %d bytes, over the %d-byte budget", got, windowByteBudget)
	}
}

// TestBuildWindow_WholeTranscriptUnderBudgetIsSentWhole pins the ordinary
// case: nothing is elided and no marker appears, so a short conversation
// reads as itself rather than as a reconstruction with a gap in it.
func TestBuildWindow_WholeTranscriptUnderBudgetIsSentWhole(t *testing.T) {
	rows := []domain.Message{
		{Role: roleUser, Subtype: openingTurnSubtype, Content: "opening"},
		{Role: roleAssistant, Content: "reading the file"},
		{Role: roleTool, Content: "package main"},
		{Role: roleAssistant, Content: "done"},
	}

	win := buildWindow(rows)

	if win.rowsSent != len(rows) || win.rowsTotal != len(rows) {
		t.Errorf("rows_sent/rows_total = %d/%d, want %d/%d", win.rowsSent, win.rowsTotal, len(rows), len(rows))
	}
	if strings.Contains(win.text, "elided") {
		t.Errorf("nothing was dropped, so nothing should say so;\n%s", win.text)
	}
	want := "user/injection:task-context: opening\nassistant: reading the file\ntool: package main\nassistant: done\n"
	if win.text != want {
		t.Errorf("render mismatch\n got: %q\nwant: %q", win.text, want)
	}
}

// TestBuildWindow_EmptyTranscript covers the row-less conversation the empty
// path takes: no text, no marker, no counts to explain.
func TestBuildWindow_EmptyTranscript(t *testing.T) {
	win := buildWindow(nil)
	if win.text != "" || win.rowsTotal != 0 || win.rowsSent != 0 {
		t.Errorf("empty transcript windowed to %+v, want a zero window", win)
	}
}

// TestRenderRow_CapsAToolResult: one 100 KB file read must not spend the tail
// a memory is mostly made of.
func TestRenderRow_CapsAToolResult(t *testing.T) {
	rendered := renderRow(domain.Message{Role: roleTool, Content: strings.Repeat("f", 100<<10)})

	if len(rendered) > toolContentLimit+len("tool: ")+len(truncationMarker) {
		t.Errorf("tool result rendered to %d bytes, want it capped near %d", len(rendered), toolContentLimit)
	}
	if !strings.HasSuffix(rendered, truncationMarker) {
		t.Errorf("a cut render must say it was cut; got the tail %q", rendered[max(0, len(rendered)-40):])
	}
}

// TestRenderRow_ToolCallsCarryNameAndBoundedArgs: the name and the target are
// what a memory needs from a call; the whole argument object is the file it
// wrote.
func TestRenderRow_ToolCallsCarryNameAndBoundedArgs(t *testing.T) {
	rendered := renderRow(domain.Message{
		Role:    roleAssistant,
		Content: "editing",
		ToolCalls: []domain.ToolCall{
			{Name: "Edit", Input: map[string]any{"file_path": "internal/foo.go", "new_string": strings.Repeat("z", 4000)}},
			{Name: "Bash", Input: nil},
		},
	})

	if !strings.Contains(rendered, "tool_call Edit(") || !strings.Contains(rendered, "internal/foo.go") {
		t.Errorf("want the call name and its target;\n%s", rendered)
	}
	if !strings.Contains(rendered, "tool_call Bash()") {
		t.Errorf("an argument-less call renders as its name;\n%s", rendered)
	}
	if len(rendered) > len("assistant: editing")+2*(toolCallArgsLimit+len(truncationMarker)+32) {
		t.Errorf("tool call args were not bounded; render is %d bytes", len(rendered))
	}
}

// TestBuildWindow_NoOrdinaryUserRowIsAllTail: a transcript whose every row is
// an injection has no opening to anchor on, and taking its first N rows
// anyway would hand the model a head that is not one.
func TestBuildWindow_NoOrdinaryUserRowIsAllTail(t *testing.T) {
	// Rows at the per-row cap, enough of them to overrun the budget — the
	// only way to force elision, since no single row can.
	filler := strings.Repeat("y", rowContentLimit)
	rows := []domain.Message{{Role: roleUser, Subtype: domain.MessageSubtypeInjectionSteer, Content: "steer " + filler}}
	for range 1 + windowByteBudget/rowContentLimit {
		rows = append(rows, domain.Message{Role: roleAssistant, Content: "work " + filler})
	}
	rows = append(rows, domain.Message{Role: roleAssistant, Content: "the last thing that happened"})

	win := buildWindow(rows)

	if strings.Contains(win.text, "steer ") {
		t.Errorf("the oldest row is not a head and should have been elided;\n%s", firstLines(win.text, 2))
	}
	if !strings.Contains(win.text, "the last thing that happened") {
		t.Errorf("the newest row must survive;\n%s", firstLines(win.text, 2))
	}
	if !strings.HasPrefix(win.text, "[… ") {
		t.Errorf("an all-tail window opens with its marker;\n%s", firstLines(win.text, 2))
	}
}

// firstLines is a bounded preview for a failure message: a window is
// hundreds of kilobytes, and a test that dumps one is unreadable.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	for i, l := range lines {
		if len(l) > 80 {
			lines[i] = l[:80] + "…"
		}
	}
	return strings.Join(lines, "\n")
}

// TestBuildWindowWithin_NeverExceedsItsBudget walks every budget across a range
// so the exact-fill boundaries are all hit, rather than hoping one hand-written
// fixture lands on the one that matters: head and tail filling the budget
// exactly is precisely the case where the elision marker appended afterwards
// pushes the render over it.
func TestBuildWindowWithin_NeverExceedsItsBudget(t *testing.T) {
	rows := []domain.Message{{Role: roleUser, Content: "the opening turn"}}
	for i := range 40 {
		rows = append(rows, domain.Message{Role: roleAssistant, Content: fmt.Sprintf("step %02d %s", i, strings.Repeat("w", 40))})
	}

	elided, whole := false, false
	for budget := elisionMarkerReserve; budget <= 3000; budget++ {
		win := buildWindowWithin(rows, budget)
		if len(win.text) > budget {
			t.Fatalf("budget %d rendered %d bytes:\n%s", budget, len(win.text), firstLines(win.text, 3))
		}
		switch {
		case win.rowsSent < win.rowsTotal:
			elided = true
		default:
			whole = true
		}
	}
	if !elided || !whole {
		t.Fatalf("the range must cover both a windowed transcript and a whole one; elided=%v whole=%v", elided, whole)
	}
}
