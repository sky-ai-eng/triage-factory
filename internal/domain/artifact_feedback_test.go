package domain

import (
	"strings"
	"testing"
)

func TestArtifactResolutionNote_FourDistinctShapes(t *testing.T) {
	prApproved := Artifact{
		Kind:   ArtifactKindPullRequest,
		Target: "octo/repo#42",
		State:  ArtifactStatePROpen,
	}
	prDismissed := Artifact{
		Kind:   ArtifactKindPullRequest,
		Target: "octo/repo#42",
		State:  ArtifactStatePRClosed,
	}
	reviewSubmitted := Artifact{
		ID:     "rev-abc",
		Kind:   ArtifactKindReview,
		Target: "octo/repo#42",
		State:  ArtifactStateReviewSubmitted,
	}
	reviewDismissed := Artifact{
		ID:     "rev-abc",
		Kind:   ArtifactKindReview,
		Target: "octo/repo#42",
		State:  ArtifactStateReviewDismissed,
	}

	notes := map[string]string{
		"pr_approved":      ArtifactResolutionNote(prApproved),
		"pr_dismissed":     ArtifactResolutionNote(prDismissed),
		"review_submitted": ArtifactResolutionNote(reviewSubmitted),
		"review_dismissed": ArtifactResolutionNote(reviewDismissed),
	}

	// Every shape produces a non-empty note.
	for name, note := range notes {
		if strings.TrimSpace(note) == "" {
			t.Errorf("%s: expected a non-empty note", name)
		}
	}

	// All four are distinct — the agent must be able to tell them apart.
	seen := map[string]string{}
	for name, note := range notes {
		if prev, ok := seen[note]; ok {
			t.Errorf("%s and %s produced the same note %q — the four shapes must be differentiable", name, prev, note)
		}
		seen[note] = name
	}

	// Each references the id the agent already knows.
	if !strings.Contains(notes["pr_approved"], "octo/repo#42") {
		t.Errorf("pr_approved note missing PR ref: %q", notes["pr_approved"])
	}
	if !strings.Contains(notes["pr_dismissed"], "octo/repo#42") {
		t.Errorf("pr_dismissed note missing PR ref: %q", notes["pr_dismissed"])
	}
	if !strings.Contains(notes["review_submitted"], "rev-abc") || !strings.Contains(notes["review_submitted"], "octo/repo#42") {
		t.Errorf("review_submitted note missing review handle or PR ref: %q", notes["review_submitted"])
	}
	if !strings.Contains(notes["review_dismissed"], "rev-abc") || !strings.Contains(notes["review_dismissed"], "octo/repo#42") {
		t.Errorf("review_dismissed note missing review handle or PR ref: %q", notes["review_dismissed"])
	}

	// Approve vs dismiss copy must be clearly opposite in wording.
	if !strings.Contains(notes["pr_approved"], "approved") || !strings.Contains(notes["pr_dismissed"], "dismissed") {
		t.Errorf("PR approve/dismiss copy not clearly differentiated: %q / %q", notes["pr_approved"], notes["pr_dismissed"])
	}
	if !strings.Contains(notes["review_submitted"], "submitted") || !strings.Contains(notes["review_dismissed"], "dismissed") {
		t.Errorf("review submit/dismiss copy not clearly differentiated: %q / %q", notes["review_submitted"], notes["review_dismissed"])
	}
}

func TestArtifactResolutionNote_NonResolutionStatesEmpty(t *testing.T) {
	cases := []Artifact{
		{Kind: ArtifactKindPullRequest, State: ArtifactStatePRDraft, Target: "o/r#1"},  // unresolved draft
		{Kind: ArtifactKindPullRequest, State: ArtifactStatePRMerged, Target: "o/r#1"}, // merged (out of scope)
		{Kind: ArtifactKindReview, State: ArtifactStateReviewPending, Target: "o/r#1"}, // still pending
		{Kind: ArtifactKindBranch, State: ArtifactStateBranchPushed, Target: "o/r"},    // branch never reported
		{Kind: ArtifactKindIssue, State: ArtifactStateIssueCreated, Target: "SKY-1"},   // issue never reported
	}
	for _, a := range cases {
		if note := ArtifactResolutionNote(a); note != "" {
			t.Errorf("kind=%s state=%s: expected empty note, got %q", a.Kind, a.State, note)
		}
		if IsResolutionNoteState(a) {
			t.Errorf("kind=%s state=%s: IsResolutionNoteState should be false", a.Kind, a.State)
		}
	}
}

func TestWrapSystemNote(t *testing.T) {
	got := WrapSystemNote("hello")
	if !strings.HasPrefix(got, "<system-note>") || !strings.HasSuffix(got, "</system-note>") {
		t.Errorf("WrapSystemNote missing tags: %q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("WrapSystemNote dropped body: %q", got)
	}
}

func TestArtifactLedgerBlock(t *testing.T) {
	// Empty / no-resolution input prepends nothing.
	if got := ArtifactLedgerBlock(nil); got != "" {
		t.Errorf("nil arts: want empty block, got %q", got)
	}
	if got := ArtifactLedgerBlock([]Artifact{
		{Kind: ArtifactKindPullRequest, State: ArtifactStatePRDraft, Target: "o/r#1"},
	}); got != "" {
		t.Errorf("only-unresolved arts: want empty block, got %q", got)
	}

	arts := []Artifact{
		{Kind: ArtifactKindPullRequest, State: ArtifactStatePROpen, Target: "o/r#1"},
		{Kind: ArtifactKindPullRequest, State: ArtifactStatePRDraft, Target: "o/r#2"}, // unresolved — skipped
		{ID: "rev-1", Kind: ArtifactKindReview, State: ArtifactStateReviewDismissed, Target: "o/r#3"},
	}
	block := ArtifactLedgerBlock(arts)
	if !strings.HasPrefix(block, "<system-note>") || !strings.HasSuffix(block, "</system-note>") {
		t.Errorf("ledger block not wrapped: %q", block)
	}
	// Exactly two bullets (the resolved PR + the dismissed review; the draft skipped).
	if n := strings.Count(block, "\n- "); n != 2 {
		t.Errorf("expected 2 ledger bullets, got %d: %q", n, block)
	}
	if !strings.Contains(block, "o/r#1") || !strings.Contains(block, "o/r#3") {
		t.Errorf("ledger block missing expected refs: %q", block)
	}
	if strings.Contains(block, "o/r#2") {
		t.Errorf("ledger block should not mention the unresolved draft o/r#2: %q", block)
	}
}

// TestArtifactResolutionNote_ReadsResolution pins that a closed or open PR's
// note is decided by who resolved it, not by state alone: rejected names the
// deleted branch, github says the change happened outside Triage Factory, and
// the TF verbs (or a stored row with no resolution) keep the human-action
// copy. The ledger derivation reads only the row, so it gets the same lines.
func TestArtifactResolutionNote_ReadsResolution(t *testing.T) {
	pr := func(state, resolution string) Artifact {
		return Artifact{
			Kind:        ArtifactKindPullRequest,
			Target:      "octo/repo#42",
			State:       state,
			DetailsJSON: MarshalPRArtifactDetails(PRArtifactDetails{HeadBranch: "feature/x", Base: "main", Resolution: resolution}),
		}
	}
	cases := []struct {
		name          string
		a             Artifact
		want, notWant []string
	}{
		{"rejected", pr(ArtifactStatePRClosed, PRResolutionRejected), []string{"rejected", "octo/repo#42", "feature/x", "was deleted"}, []string{"kept", "GitHub"}},
		{"dismissed", pr(ArtifactStatePRClosed, PRResolutionDismissed), []string{"dismissed", "kept"}, []string{"rejected", "GitHub"}},
		{"closed on github", pr(ArtifactStatePRClosed, PRResolutionGitHub), []string{"closed without merging on GitHub"}, []string{"human", "kept", "rejected"}},
		{"closed, no resolution stored", pr(ArtifactStatePRClosed, ""), []string{"dismissed", "kept"}, []string{"rejected", "GitHub"}},
		{"approved", pr(ArtifactStatePROpen, PRResolutionApproved), []string{"approved", "ready for review"}, []string{"GitHub"}},
		{"marked ready on github", pr(ArtifactStatePROpen, PRResolutionGitHub), []string{"ready for review on GitHub"}, []string{"human"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ArtifactResolutionNote(tc.a)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("note %q lacks %q", got, w)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("note %q must not contain %q", got, nw)
				}
			}
			if !IsResolutionNoteState(tc.a) {
				t.Error("must be a resolution-note state so the resume ledger picks it up")
			}
		})
	}
	if block := ArtifactLedgerBlock([]Artifact{pr(ArtifactStatePRClosed, PRResolutionRejected)}); !strings.Contains(block, "feature/x") {
		t.Errorf("ledger block %q should carry the rejection line", block)
	}
}
