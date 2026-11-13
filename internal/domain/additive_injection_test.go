package domain

import (
	"strings"
	"testing"
)

// TestAdditiveEventInjection_NamesTypeAndMetadata: the copy names the event
// type and includes the metadata JSON, plus tells the agent it won't spawn a
// separate run.
func TestAdditiveEventInjection_NamesTypeAndMetadata(t *testing.T) {
	injection := AdditiveEventInjection("slack:message", `{"channel":"C123","ts":"1.2"}`)
	for _, want := range []string{"slack:message", `"channel":"C123"`, "will not spawn a separate run"} {
		if !strings.Contains(injection, want) {
			t.Errorf("injection missing %q: %q", want, injection)
		}
	}
}

// TestAdditiveEventInjection_EmptyMetadata_StillNamesType: a best-effort
// metadata lookup failure degrades to a body naming the event type alone,
// never an empty/blank injection.
func TestAdditiveEventInjection_EmptyMetadata_StillNamesType(t *testing.T) {
	injection := AdditiveEventInjection("github:pr:ci_check_failed", "")
	if injection == "" {
		t.Fatal("injection is empty")
	}
	if !strings.Contains(injection, "github:pr:ci_check_failed") {
		t.Errorf("injection missing event type: %q", injection)
	}
	if strings.Contains(injection, "Event metadata:") {
		t.Errorf("empty metadata should omit the metadata section: %q", injection)
	}
}

// TestTaskNotes_EachSaysAPersonsMessageStillStands: every note about a task's
// close or held close tells the agent to end the workflow unless its
// instructions need otherwise, so every one of them ends with the sentence
// that keeps a person's later message standing. The check is that the notes
// share it, not what it says.
func TestTaskNotes_EachSaysAPersonsMessageStillStands(t *testing.T) {
	notes := map[string]string{
		"event":             TaskClosedNote(TaskClose{Kind: TaskClosedByEvent, EventType: EventGitHubPRMerged, MetadataJSON: `{"merged_by":"octocat"}`}),
		"event, no event":   TaskClosedNote(TaskClose{Kind: TaskClosedByEvent}),
		"completed":         TaskClosedNote(TaskClose{Kind: TaskClosedCompleted}),
		"dismissed":         TaskClosedNote(TaskClose{Kind: TaskClosedDismissed}),
		"duplicate":         TaskClosedNote(TaskClose{Kind: TaskClosedDuplicate, DuplicateOf: "task 7"}),
		"reassigned":        TaskClosedNote(TaskClose{Kind: TaskClosedReassigned, EventType: EventLinearIssueAssigned}),
		"held":              TaskCloseHeldNote(EventGitHubPRCICheckPassed, `{"check_name":"build"}`),
		"held, no metadata": TaskCloseHeldNote(EventGitHubPRCICheckPassed, ""),
	}
	for name, note := range notes {
		if !strings.HasSuffix(note, taskNotePersonMessage) {
			t.Errorf("%s note does not end by keeping a person's message standing:\n%s", name, note)
		}
	}
	// The line a launch reads in <run_context> is a statement of the task's
	// state and gives no instruction a person's message could conflict with.
	if line := TaskClosedRunContext(TaskClose{Kind: TaskClosedDismissed}); strings.Contains(line, strings.TrimSpace(taskNotePersonMessage)) {
		t.Errorf("<run_context> line carries the note's guidance: %q", line)
	}
}
