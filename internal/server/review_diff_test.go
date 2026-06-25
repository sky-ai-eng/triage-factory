package server

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// strPtr returns a pointer to a string literal so test fixtures can
// supply non-nil snapshots inline. nil vs non-nil-pointer-to-"" is a
// load-bearing distinction in HumanFeedbackInput (legacy vs. real
// empty snapshot), so legibility at the call site matters — using
// raw `&"agent draft"` works but is awkward.
func strPtr(s string) *string { return &s }

// Golden tests for FormatHumanFeedback. The formatter is pure data
// in / string out, so the tests live in this file as straight
// equality checks against expected markdown. The goal is to pin the
// shape future agents will read — if you're tempted to reorder bold
// labels or rename headings, update the briefing in
// docs/for-agents/auto-delegation-briefing.md in lockstep so you
// don't break the parser side of the contract.

func TestFormatHumanFeedback_NoEditsVerdictUnchanged(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("Looks good to me."),
		FinalBody:     "Looks good to me.",
		OriginalEvent: strPtr("APPROVE"),
		FinalEvent:    "APPROVE",
		Comments: []ReviewCommentDiffEntry{
			{Path: "foo.go", Line: 10, Status: CommentDiffUnchanged, Original: "nit", Final: "nit"},
		},
	})
	// Stored shape carries no leading "## Human feedback (post-run)"
	// heading; the materialization layer (db.materializeMemory)
	// prepends it via the separator when reading alongside
	// agent_content. Baking it in here would double-head the
	// agent-readable file. See FormatHumanFeedback's note.
	want := "**Outcome:** Human submitted the review as drafted, no edits.\n" +
		"**Verdict:** APPROVE (unchanged from agent's draft).\n"
	if got != want {
		t.Errorf("formatter output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormatHumanFeedback_VerdictChangedOnly is the highest-signal
// case the spec calls out: the agent drafted one verdict, the human
// submitted another. The "Verdict changed:" prefix (vs. "Verdict:")
// is what teaches the next agent to recalibrate severity reading
// from this entity's history.
func TestFormatHumanFeedback_VerdictChangedOnly(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("LGTM"),
		FinalBody:     "LGTM",
		OriginalEvent: strPtr("APPROVE"),
		FinalEvent:    "REQUEST_CHANGES",
	})
	if !strings.Contains(got, "**Verdict changed:** agent drafted APPROVE, human submitted REQUEST_CHANGES.") {
		t.Errorf("expected verdict-changed line; got:\n%s", got)
	}
	if !strings.Contains(got, "**Outcome:** Human submitted the review with edits.") {
		t.Errorf("verdict change must flip outcome to 'with edits'; got:\n%s", got)
	}
}

// TestFormatHumanFeedback_BodyEditedOnly pins the body diff section
// shape: blockquoted final, separator line, blockquoted original
// under the "Originally drafted as:" header. Block-quoting (rather
// than inline quotes) was chosen so multi-line review bodies parse
// unambiguously regardless of what punctuation they contain.
func TestFormatHumanFeedback_BodyEditedOnly(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("this PR adds X."),
		FinalBody:     "This PR adds X. Note the migration ordering issue.",
		OriginalEvent: strPtr("COMMENT"),
		FinalEvent:    "COMMENT",
	})
	wantPieces := []string{
		"**Body:** Edited.",
		"> This PR adds X. Note the migration ordering issue.",
		">\n> **Originally drafted as:**",
		"> this PR adds X.",
		"**Verdict:** COMMENT (unchanged from agent's draft).",
		"**Outcome:** Human submitted the review with edits.",
	}
	for _, want := range wantPieces {
		if !strings.Contains(got, want) {
			t.Errorf("missing piece %q in output:\n%s", want, got)
		}
	}
	// Body section must NOT appear when there are no edits — but
	// here we have edits, so it must appear:
	if !strings.Contains(got, "\n**Body:** Edited.\n") {
		t.Errorf("body section header missing: %s", got)
	}
}

// TestFormatHumanFeedback_CommentEdited pins the per-comment shape:
// path:line in code-quote, hyphen separator, status, then the
// nested Was/Now lines. Newlines in comment bodies fold to spaces so
// the list stays scannable.
func TestFormatHumanFeedback_CommentEdited(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("ok"),
		FinalBody:     "ok",
		OriginalEvent: strPtr("APPROVE"),
		FinalEvent:    "APPROVE",
		Comments: []ReviewCommentDiffEntry{
			{
				Path:     "internal/db/db.go",
				Line:     447,
				Status:   CommentDiffEdited,
				Original: "This index might not be necessary, double-check.",
				Final:    "Drop this index — it duplicates idx_runs_status.",
			},
		},
	})
	if !strings.Contains(got, "\n**Comment edits:**\n\n") {
		t.Errorf("missing Comment edits header: %s", got)
	}
	if !strings.Contains(got, "- `internal/db/db.go:447` — edited\n") {
		t.Errorf("missing edited comment header: %s", got)
	}
	if !strings.Contains(got, "  - Was: This index might not be necessary, double-check.\n") {
		t.Errorf("missing Was line: %s", got)
	}
	if !strings.Contains(got, "  - Now: Drop this index — it duplicates idx_runs_status.\n") {
		t.Errorf("missing Now line: %s", got)
	}
}

// TestFormatHumanFeedback_CommentRemovedAndAdded covers the two
// statuses the v1 handler doesn't yet emit (soft-delete + UI add
// flow are deferred). The formatter still supports them so the
// future wiring is a one-line change. Pinning their output shape
// here means that wiring can land without re-litigating format.
func TestFormatHumanFeedback_CommentRemovedAndAdded(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("x"),
		FinalBody:     "x",
		OriginalEvent: strPtr("COMMENT"),
		FinalEvent:    "COMMENT",
		Comments: []ReviewCommentDiffEntry{
			{Path: "a.go", Line: 1, Status: CommentDiffRemoved, Original: "agent's drafted comment"},
			{Path: "b.go", Line: 2, Status: CommentDiffAdded, Final: "human's added insight"},
		},
	})
	for _, want := range []string{
		"- `a.go:1` — removed by human\n",
		"  - Was: agent's drafted comment\n",
		"- `b.go:2` — added by human (was not in agent's draft)\n",
		"  - Body: human's added insight\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestFormatHumanFeedback_LegacyNullOriginalsDegrade is the regression
// for the SKY-204 / SKY-205 cutover: pending_reviews rows whose
// original_review_body / original_review_event are NULL (mid-flight
// when the columns were added) must produce a bare verdict line
// without an unchanged/changed claim, and skip the body diff
// entirely. We don't fabricate an "as drafted" assertion we can't
// substantiate.
func TestFormatHumanFeedback_LegacyNullOriginalsDegrade(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  nil, // legacy: no snapshot was captured
		FinalBody:     "Final body the human submitted.",
		OriginalEvent: nil, // legacy: no snapshot was captured
		FinalEvent:    "APPROVE",
	})
	if !strings.Contains(got, "**Verdict:** APPROVE.\n") {
		t.Errorf("legacy verdict line missing or annotated: %s", got)
	}
	if strings.Contains(got, "unchanged from agent's draft") {
		t.Errorf("must NOT claim unchanged when original event is NULL: %s", got)
	}
	if strings.Contains(got, "Verdict changed") {
		t.Errorf("must NOT claim changed when original event is NULL: %s", got)
	}
	if strings.Contains(got, "**Body:**") {
		t.Errorf("must NOT emit body diff when original body is NULL: %s", got)
	}
}

// TestFormatHumanFeedback_CombinedAllChanged exercises the most
// common "the human did real work" case: verdict flipped, body
// edited, and a comment edited. All three sections must render in
// the documented order — outcome, verdict, body, comments —
// because the briefing teaches agents to scan top-to-bottom.
func TestFormatHumanFeedback_CombinedAllChanged(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("small nits."),
		FinalBody:     "Larger concerns: see comments.",
		OriginalEvent: strPtr("APPROVE"),
		FinalEvent:    "REQUEST_CHANGES",
		Comments: []ReviewCommentDiffEntry{
			{Path: "x.go", Line: 5, Status: CommentDiffEdited, Original: "minor", Final: "blocking issue here"},
			{Path: "y.go", Line: 9, Status: CommentDiffUnchanged, Original: "ok", Final: "ok"},
		},
	})
	outcomeIdx := strings.Index(got, "**Outcome:**")
	verdictIdx := strings.Index(got, "**Verdict")
	bodyIdx := strings.Index(got, "**Body:**")
	commentsIdx := strings.Index(got, "**Comment edits:**")
	if outcomeIdx < 0 || verdictIdx <= outcomeIdx || bodyIdx <= verdictIdx || commentsIdx <= bodyIdx {
		t.Errorf("section order wrong; got indices outcome=%d verdict=%d body=%d comments=%d in:\n%s",
			outcomeIdx, verdictIdx, bodyIdx, commentsIdx, got)
	}
	// Unchanged comment must not appear in the list.
	if strings.Contains(got, "y.go:9") {
		t.Errorf("unchanged comment leaked into edits list:\n%s", got)
	}
}

// TestBuildReviewHumanFeedbackInput_ClassifiesComments pins the bridge between a
// review artifact's proposed snapshot + the live final comments and the
// formatter's pure input. Comments are joined by GitHub node id; severity badges
// are stripped from both sides so the diff renders clean prose. With node ids the
// classifier resolves all four statuses (the old local-table path could only emit
// unchanged/edited).
func TestBuildReviewHumanFeedbackInput_ClassifiesComments(t *testing.T) {
	line := func(n int) *int { return &n }
	details := domain.ReviewArtifactDetails{
		ReviewBody:  "final body",
		ReviewEvent: "REQUEST_CHANGES",
		Proposed: domain.ReviewArtifactProposed{
			Body:  "draft body",
			Event: "APPROVE",
			Comments: []domain.ReviewArtifactComment{
				// Badge baked into the agent's draft — must be stripped for the diff.
				{ID: "c1", Path: "x.go", Line: line(1), Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "agent draft"},
				{ID: "c2", Path: "y.go", Line: line(2), Body: "still the same"},
				{ID: "c3", Path: "z.go", Line: line(3), Body: "removed by human"},
			},
		},
	}
	finalComments := []ghclient.PendingReviewComment{
		// Edited: same id, body differs from the proposed snapshot.
		{ID: "c1", Path: "x.go", Line: line(1), Body: domain.SeverityBadgeMarkdown(domain.SeverityMajor) + "user edit"},
		// Unchanged.
		{ID: "c2", Path: "y.go", Line: line(2), Body: "still the same"},
		// c3 is absent → removed by the human.
		// Added: present in final, not in the proposed draft.
		{ID: "c4", Path: "w.go", Line: line(4), Body: "human added"},
	}

	got := buildReviewHumanFeedbackInput(details, finalComments)

	if got.OriginalBody == nil || *got.OriginalBody != "draft body" || got.FinalBody != "final body" {
		t.Errorf("body fields wrong: orig=%v final=%q", got.OriginalBody, got.FinalBody)
	}
	if got.OriginalEvent == nil || *got.OriginalEvent != "APPROVE" || got.FinalEvent != "REQUEST_CHANGES" {
		t.Errorf("event fields wrong: orig=%v final=%q", got.OriginalEvent, got.FinalEvent)
	}
	wantStatus := map[string]string{
		"x.go": CommentDiffEdited,
		"y.go": CommentDiffUnchanged,
		"z.go": CommentDiffRemoved,
		"w.go": CommentDiffAdded,
	}
	if len(got.Comments) != len(wantStatus) {
		t.Fatalf("len(Comments) = %d, want %d", len(got.Comments), len(wantStatus))
	}
	for _, c := range got.Comments {
		if want := wantStatus[c.Path]; c.Status != want {
			t.Errorf("comment[%s]: status = %q, want %q", c.Path, c.Status, want)
		}
		// Severity badge must be stripped — the diff shows clean prose.
		if strings.Contains(c.Original, "img.shields.io") || strings.Contains(c.Final, "img.shields.io") {
			t.Errorf("comment[%s] still carries a severity badge: orig=%q final=%q", c.Path, c.Original, c.Final)
		}
	}
}

// TestFormatHumanFeedback_EmptyOriginalBodyIsRealSnapshot is the
// regression for the COALESCE-collapsed-sentinel bug: a
// pending_reviews row whose original_review_body was a real empty
// string (the agent drafted only inline comments, no top-level body)
// must drive a body diff if the human added body text — not get
// silently treated as legacy and skipped. Likewise for the verdict
// line: a non-nil pointer to "" is a real snapshot, so an unchanged
// comparison still applies.
func TestFormatHumanFeedback_EmptyOriginalBodyIsRealSnapshot(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr(""), // agent drafted no top-level body
		FinalBody:     "I added context the agent missed.",
		OriginalEvent: strPtr("COMMENT"),
		FinalEvent:    "COMMENT",
	})
	if !strings.Contains(got, "**Body:** Edited.") {
		t.Errorf("empty real snapshot must drive a body-diff section when human adds text:\n%s", got)
	}
	if !strings.Contains(got, "> I added context the agent missed.") {
		t.Errorf("final body missing from blockquote:\n%s", got)
	}
	if !strings.Contains(got, "> **Originally drafted as:**") {
		t.Errorf("originally-drafted-as section missing:\n%s", got)
	}
	if strings.Contains(got, "**Verdict:** COMMENT.\n") {
		t.Errorf("legacy-style bare verdict line leaked through; OriginalEvent was a real snapshot:\n%s", got)
	}
	if !strings.Contains(got, "**Verdict:** COMMENT (unchanged from agent's draft).") {
		t.Errorf("expected unchanged annotation against real empty snapshot:\n%s", got)
	}
}

// TestFormatHumanFeedback_BodyCRLFNormalized is the regression for
// the blockquote CRLF bug: GitHub's web textarea and Windows clients
// deliver review bodies with "\r\n" line endings, and a naive split
// on "\n" leaves a stray "\r" at the end of every quoted line. The
// stored human_content must be byte-stable across platforms so
// downstream parsers (and humans diffing run_memory rows) see clean
// LF-terminated blockquote lines regardless of where the human typed
// the review.
func TestFormatHumanFeedback_BodyCRLFNormalized(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalBody:  strPtr("first line\r\nsecond line"),
		FinalBody:     "rewritten first\r\nrewritten second\r\n",
		OriginalEvent: strPtr("COMMENT"),
		FinalEvent:    "COMMENT",
	})
	if strings.Contains(got, "\r") {
		t.Errorf("output must not contain CR characters; got:\n%q", got)
	}
	for _, want := range []string{
		"> rewritten first\n",
		"> rewritten second\n",
		"> first line\n",
		"> second line\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in CRLF-normalized output:\n%s", want, got)
		}
	}
}

// TestFormatHumanFeedback_MultilineCommentBodyFoldsToSpaces pins the
// inlineBody contract: review comments occasionally span multiple
// lines, but the bullet-list rendering needs to stay single-line per
// entry to be scannable. Newlines collapse to spaces; paragraph
// structure is sacrificed for legibility (acceptable trade-off
// because review comments are rarely multi-paragraph and the LLM
// consumer reads the result as a flat sentence either way).
func TestFormatHumanFeedback_MultilineCommentBodyFoldsToSpaces(t *testing.T) {
	got := FormatHumanFeedback(HumanFeedbackInput{
		OriginalEvent: strPtr("COMMENT"),
		FinalEvent:    "COMMENT",
		Comments: []ReviewCommentDiffEntry{
			{
				Path:     "a.go",
				Line:     1,
				Status:   CommentDiffEdited,
				Original: "first line\nsecond line",
				Final:    "rewritten line",
			},
		},
	})
	if !strings.Contains(got, "  - Was: first line second line\n") {
		t.Errorf("multi-line comment body not folded: %s", got)
	}
}
