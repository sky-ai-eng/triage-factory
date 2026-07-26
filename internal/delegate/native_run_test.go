package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// TestPrepareInheritedMemory_TrustsAClaimThatAlreadyRan pins the distinction
// the native runtime has no session id to make for it.
//
// A blueprint's steps share one run tree and all write the same memory
// filename, so a fresh step must distrust whatever is at that path. But a
// PARKED engagement picking up a steer is the same conversation continuing, and
// the file there is its own — distrusting it would discard the run's notes at
// termination and file agent_content NULL over real work.
//
// Both cases are exercised on a handed-off tree, which is the only shape where
// the two outcomes differ: with the tree still writable a fresh claim simply
// deletes the file and fingerprints nothing.
func TestPrepareInheritedMemory_TrustsAClaimThatAlreadyRan(t *testing.T) {
	for _, tc := range []struct {
		name        string
		drive       bool
		wantDigest  bool
		wantContent string
	}{
		{name: "fresh claim distrusts a predecessor's file", drive: false, wantDigest: true},
		{name: "re-claimed engagement keeps its own", drive: true, wantDigest: false, wantContent: "what I found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			cwd := t.TempDir()
			seedRun(t, database, "r-mem", "", cwd)
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

			if err := os.MkdirAll(filepath.Join(cwd, scratchDirName), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(agentMemoryFilePath(cwd), []byte("what I found"), 0644); err != nil {
				t.Fatal(err)
			}
			if tc.drive {
				pending := false
				if _, err := s.agentRuns.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
					ConversationID: "r-mem",
					UserID:         runmode.LocalDefaultUserID,
					Role:           "user",
					Subtype:        "text",
					Content:        "go",
					Delivered:      &pending,
				}); err != nil {
					t.Fatalf("seed transcript: %v", err)
				}
			}

			// handedOff=true: the warm-step shape, where the orchestrator can
			// read the file but not delete it.
			got := s.prepareInheritedMemory(context.Background(), runmode.LocalDefaultOrgID, "r-mem", cwd, nil, true)
			if (got != nil) != tc.wantDigest {
				t.Fatalf("fingerprint present = %v, want %v", got != nil, tc.wantDigest)
			}
			// The fingerprint's whole purpose is what termination then does with
			// it, so assert through that rather than on the digest itself.
			content, _ := readRunMemory(cwd, got)
			if content != tc.wantContent {
				t.Errorf("memory ingested at termination = %q, want %q", content, tc.wantContent)
			}
		})
	}
}
