package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestAskedAboutArtifactAlready pins when the artifact-contract question
// re-arms. Asking twice about the same silence is badgering; asking again
// after a human has changed the premise is a different question about
// different work.
//
// The state is read from the transcript rather than remembered, so an
// engagement that inherits a conversation behaves exactly like the one that
// started it — a crash cannot buy a second nudge, and cannot lose one.
func TestAskedAboutArtifactAlready(t *testing.T) {
	nudge := domain.Message{Role: "user", Subtype: "text", Content: artifactNudgeNote}
	assistant := domain.Message{Role: "assistant", Content: "nothing to publish"}
	human := domain.Message{Role: "user", Subtype: "text", Content: "actually, open the PR"}
	steered := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "also check the tests"}
	crashNotice := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionExecutorChanged, Content: "your executor changed"}

	tests := []struct {
		name string
		rows []domain.Message
		want bool
	}{
		{
			name: "never asked",
			rows: []domain.Message{human, assistant},
		},
		{
			name: "asked, and nothing has happened since but the model's own answer",
			rows: []domain.Message{human, assistant, nudge, assistant},
			want: true,
		},
		{
			name: "a human spoke after the nudge, so the premise is new",
			rows: []domain.Message{human, assistant, nudge, assistant, human, assistant},
		},
		{
			// The case the old per-engagement flag got wrong: a follow-up
			// that lands mid-work is stamped as a steer, and it is still a
			// person asking for more.
			name: "a mid-work steer counts as a human speaking",
			rows: []domain.Message{human, assistant, nudge, assistant, steered, assistant},
		},
		{
			// The loop's own crash notice speaks for nobody, so it must not
			// buy the run a second nudge.
			name: "an executor-changed notice does not re-arm",
			rows: []domain.Message{human, assistant, nudge, crashNotice, assistant},
			want: true,
		},
		{
			name: "an empty transcript has nothing to have asked",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := askedAboutArtifactAlready(tc.rows); got != tc.want {
				t.Errorf("askedAboutArtifactAlready = %v, want %v", got, tc.want)
			}
		})
	}
}
