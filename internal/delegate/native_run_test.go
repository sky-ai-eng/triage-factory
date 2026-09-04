package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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
	nudge := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionNudge, Content: artifactNudgeNote}
	assistant := domain.Message{Role: "assistant", Content: "nothing to publish"}
	human := domain.Message{Role: "user", Content: "actually, open the PR"}
	steered := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "also check the tests"}
	crashNotice := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionExecutorChanged, Content: "your executor changed"}
	stopNote := domain.Message{Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: "This run reached its spend cap and has been paused."}
	stagedNote := domain.Message{Role: "user", Subtype: "injection:system-note", Content: "<system-note>PR gained commits</system-note>"}
	wrapUp := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionWrapUp, Content: "<system-note>wrap up</system-note>"}

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
			// A guard park between the nudge and the resumed engagement's
			// would-stop: the park's record speaks for nobody either.
			name: "a stop-note from a guard park does not re-arm",
			rows: []domain.Message{human, assistant, nudge, assistant, stopNote, crashNotice, assistant},
			want: true,
		},
		{
			name: "a staged system note does not re-arm",
			rows: []domain.Message{human, assistant, nudge, stagedNote, assistant},
			want: true,
		},
		{
			name: "the wrap-up ask does not re-arm",
			rows: []domain.Message{human, assistant, nudge, wrapUp, assistant},
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
			seedConversation(t, database, "r-mem", "", cwd)
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

			if err := os.MkdirAll(filepath.Join(cwd, scratchDirName), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(agentMemoryFilePath(cwd), []byte("what I found"), 0644); err != nil {
				t.Fatal(err)
			}
			if tc.drive {
				pending := false
				if _, err := s.conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
					ConversationID: "r-mem",
					UserID:         runmode.LocalDefaultUserID,
					Role:           "user",
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
			content, _ := readConversationMemory(cwd, got)
			if content != tc.wantContent {
				t.Errorf("memory ingested at termination = %q, want %q", content, tc.wantContent)
			}
		})
	}
}

// TestMintOpeningTurn_QueuesThePendingInputShape pins the half of the input
// contract the loop owns. The opening turn is queued as an undelivered plain
// user row — the same shape a follow-up takes, and the shape
// ConversationPendingInputStore's predicate is written against — so the engagement's
// entry really is just its first drain, with no first-call special case.
//
// It also pins the idempotence the gate exists for: a re-claim of a
// conversation that has already spoken adds nothing, so a crash between this
// insert and the first call cannot double the opening.
func TestMintOpeningTurn_QueuesThePendingInputShape(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-open-turn", "", "/tmp/wt-open-turn")
	claimID := markEngaged(t, database, "r-open-turn")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	transcript := newNativeTranscript(s, runmode.LocalDefaultOrgID, "r-open-turn", claimID)

	ctx := context.Background()
	opening := "<task_context>\nPull request owner/repo#7\n</task_context>\n\nfix the failing check"
	if err := s.mintOpeningTurn(ctx, transcript, runmode.LocalDefaultOrgID, "r-open-turn", runmode.LocalDefaultUserID, opening); err != nil {
		t.Fatalf("mintOpeningTurn: %v", err)
	}

	rows := pendingRows(t, s, "r-open-turn")
	if len(rows) != 1 {
		t.Fatalf("undelivered rows = %d, want the opening turn waiting for the first drain", len(rows))
	}
	// The pending-input predicate, spelled out: anything else and the queue's
	// reads stop seeing a row the loop is relying on.
	if rows[0].Role != "user" || rows[0].Subtype != "" || rows[0].Content != opening {
		t.Errorf("opening turn = %+v, want a plain user row carrying the composed mission", rows[0])
	}

	// Re-claiming a conversation that has already spoken adds nothing.
	if err := s.mintOpeningTurn(ctx, transcript, runmode.LocalDefaultOrgID, "r-open-turn", runmode.LocalDefaultUserID, opening); err != nil {
		t.Fatalf("second mintOpeningTurn: %v", err)
	}
	if got := pendingRows(t, s, "r-open-turn"); len(got) != 1 {
		t.Errorf("undelivered rows after a re-claim = %d, want 1 — the opening must not double", len(got))
	}
}

// TestExecutorChangedSince pins the one claim in the resume notice that
// neither the transcript nor the tree on disk can establish: that the
// engagement before this one ran somewhere else.
//
// The read is deliberately skipped on a warm workspace, where nothing is said
// at all — a query whose answer cannot change the outcome is a query the
// common resume should not pay for.
func TestExecutorChangedSince(t *testing.T) {
	const org = runmode.LocalDefaultOrgID
	ctx := context.Background()

	// Stages a conversation whose live claim belongs to self, preceded by a
	// released claim on predecessor (skipped when empty — a first claim).
	setup := func(t *testing.T, conversationID, predecessor, self string) (*Spawner, string) {
		t.Helper()
		database := newDelegateTestDB(t)
		seedConversation(t, database, conversationID, "sess", "/tmp/wt-"+conversationID)
		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
		if predecessor != "" {
			s.SetExecutorID(predecessor, 1)
			s.stampExecutor(org, conversationID, "")
			if _, err := s.conversations.SetExecutorSystem(ctx, org, conversationID, "", 0); err != nil {
				t.Fatalf("release the predecessor's claim: %v", err)
			}
		}
		s.SetExecutorID(self, 2)
		s.stampExecutor(org, conversationID, "")
		var claimID string
		if err := database.QueryRow(
			`SELECT id FROM claims WHERE conversation_id = ? AND released_at IS NULL`, conversationID,
		).Scan(&claimID); err != nil {
			t.Fatalf("read the live claim: %v", err)
		}
		return s, claimID
	}

	tests := []struct {
		name        string
		predecessor string
		prov        domain.WorkspaceProvenance
		want        bool
	}{
		{
			name:        "a rebuild after a predecessor elsewhere",
			predecessor: "exec-other",
			prov:        domain.WorkspaceProvenanceRehydrated,
			want:        true,
		},
		{
			// A wiped run root or a startup sweep: the tree is gone, the
			// executor never moved.
			name:        "a rebuild on the executor that parked it",
			predecessor: "exec-self",
			prov:        domain.WorkspaceProvenanceRehydrated,
		},
		{
			name: "a first claim has no predecessor to differ from",
			prov: domain.WorkspaceProvenanceFresh,
		},
		{
			// Nothing is said about a warm tree, so nothing is asked.
			name:        "a warm resume, predecessor elsewhere or not",
			predecessor: "exec-other",
			prov:        domain.WorkspaceProvenanceWarm,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conversationID := fmt.Sprintf("r-exec-changed-%d", i)
			s, claimID := setup(t, conversationID, tc.predecessor, "exec-self")
			if got := s.executorChangedSince(ctx, org, conversationID, claimID, tc.prov); got != tc.want {
				t.Errorf("executorChangedSince = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExecutorChangedSince_UnwiredIdentityStaysSilent: a spawner with no
// resolved instance id cannot tell its own claims from anyone else's, and a
// notice must never assert a move on the strength of a comparison against
// nothing.
func TestExecutorChangedSince_UnwiredIdentityStaysSilent(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-exec-unwired", "sess", "/tmp/wt-unwired")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if _, err := s.conversations.SetExecutorSystem(context.Background(), runmode.LocalDefaultOrgID, "r-exec-unwired", "exec-other", 1); err != nil {
		t.Fatalf("mint the predecessor's claim: %v", err)
	}
	if s.executorChangedSince(context.Background(), runmode.LocalDefaultOrgID, "r-exec-unwired", "some-claim", domain.WorkspaceProvenanceRehydrated) {
		t.Error("an unwired spawner claimed the executor changed; it has no identity to compare against")
	}
}

// TestNativeBashMemBudgetMB pins the derivation, whose whole point is that the
// budget tracks the ceiling the jail was actually launched under.
func TestNativeBashMemBudgetMB(t *testing.T) {
	tests := []struct {
		name    string
		ceiling int
		want    int
	}{
		{
			name:    "the default ceiling leaves a gigabyte of session headroom",
			ceiling: agentproc.DefaultClaimMemoryLimitMB,
			want:    3072,
		},
		{
			name:    "a generous ceiling is capped rather than handed over whole",
			ceiling: 16384,
			want:    3072,
		},
		{
			name:    "a modest ceiling keeps the headroom instead of the cap",
			ceiling: 3072,
			want:    2048,
		},
		{
			// The floor wins over the subtraction: a budget below it would be
			// too tight for an ordinary build step, and the ceiling is still
			// underneath as the backstop.
			name:    "a tight ceiling floors rather than going negative",
			ceiling: 1024,
			want:    512,
		},
		{
			name:    "a ceiling under the headroom still floors",
			ceiling: 256,
			want:    512,
		},
		{
			// An operator who disabled the ceiling said there is no shared
			// allowance to protect; inventing one here would impose a limit
			// they turned off.
			name:    "a disabled ceiling disables the budget",
			ceiling: 0,
			want:    0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeBashMemBudgetMB(tc.ceiling); got != tc.want {
				t.Errorf("nativeBashMemBudgetMB(%d) = %d, want %d", tc.ceiling, got, tc.want)
			}
		})
	}
}

// TestNativeBashMemBudgetMB_StaysUnderTheCeiling is the property the numbers
// exist to satisfy: one command may never be licensed to consume the jail's
// whole allowance, or the breach lands on the jail instead of on the command.
//
// Swept from the floor upward, because below it the floor deliberately wins —
// a ceiling smaller than 512 MB cannot run an agent at all, and the answer
// there is a bigger ceiling, not a budget rounded down to nothing.
func TestNativeBashMemBudgetMB_StaysUnderTheCeiling(t *testing.T) {
	for ceiling := bashMemBudgetFloorMB + 1; ceiling <= 32768; ceiling += 37 {
		budget := nativeBashMemBudgetMB(ceiling)
		if budget >= ceiling {
			t.Fatalf("ceiling %d MB yields a budget of %d MB, which leaves the jail no headroom", ceiling, budget)
		}
	}
}
