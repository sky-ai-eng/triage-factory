package curator

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestRenderEnvelope_TrackerBlockReadsBothFields(t *testing.T) {
	got := renderEnvelope(envelopeInputs{
		ProjectName:      "P",
		JiraProjectKey:   "SKY",
		LinearProjectKey: "linear-id-1",
		BinaryPath:       "/usr/local/bin/triagefactory",
	})
	if !strings.Contains(got, "SKY") {
		t.Errorf("expected Jira key SKY in envelope: %q", got)
	}
	if !strings.Contains(got, "linear-id-1") {
		t.Errorf("expected Linear key linear-id-1 in envelope: %q", got)
	}
}

func TestRenderEnvelope_NoTrackersFallback(t *testing.T) {
	got := renderEnvelope(envelopeInputs{ProjectName: "P", BinaryPath: "/x"})
	if !strings.Contains(got, "No issue tracker") {
		t.Errorf("expected fallback for unset trackers, got: %q", got)
	}
}

func TestRenderEnvelope_NoPinnedReposFallback(t *testing.T) {
	got := renderEnvelope(envelopeInputs{ProjectName: "P", BinaryPath: "/x"})
	if !strings.Contains(got, "no repositories pinned") {
		t.Errorf("expected fallback for unset pinned repos, got: %q", got)
	}
}

func TestRenderEnvelope_PinnedReposIncludePath(t *testing.T) {
	got := renderEnvelope(envelopeInputs{
		ProjectName: "P",
		PinnedRepos: []string{"sky-ai-eng/triage-factory"},
		BinaryPath:  "/x",
	})
	if !strings.Contains(got, "./repos/sky-ai-eng/triage-factory") {
		t.Errorf("expected materialized path in envelope, got: %q", got)
	}
}

// TestPendingChangesNote_PinnedRepoDiff covers the basic add/remove
// rendering — what the agent actually sees in the [system note] block.
func TestPendingChangesNote_PinnedRepoDiff(t *testing.T) {
	rows := []domain.CuratorContextChange{{
		ChangeType:    domain.ChangeTypePinnedRepos,
		BaselineValue: `["a/b","c/d"]`,
	}}
	got := pendingChangesNote(rows, envelopeInputs{
		PinnedRepos: []string{"a/b", "e/f"},
	})
	if !strings.Contains(got, `added "e/f"`) {
		t.Errorf("expected `added \"e/f\"` in note, got: %q", got)
	}
	if !strings.Contains(got, `removed "c/d"`) {
		t.Errorf("expected `removed \"c/d\"` in note, got: %q", got)
	}
	if !strings.Contains(got, "[system note]") {
		t.Errorf("expected [system note] tag, got: %q", got)
	}
}

// TestPendingChangesNote_RoundTripSuppressed exercises the coalescing
// payoff: an A→B→A round trip ends up with baseline == current at
// consume time, which renders as no diff and gets dropped — so the
// agent isn't told about a "change" that has no observable effect.
func TestPendingChangesNote_RoundTripSuppressed(t *testing.T) {
	rows := []domain.CuratorContextChange{{
		ChangeType:    domain.ChangeTypePinnedRepos,
		BaselineValue: `["a/b"]`,
	}}
	got := pendingChangesNote(rows, envelopeInputs{
		PinnedRepos: []string{"a/b"},
	})
	if got != "" {
		t.Errorf("expected empty note for round-trip no-op, got: %q", got)
	}
}

// TestPendingChangesNote_TrackerLinkUnlink covers the three transition
// shapes for tracker keys: unset → set, set → unset, set A → set B.
func TestPendingChangesNote_TrackerLinkUnlink(t *testing.T) {
	cases := []struct {
		name      string
		baseline  string
		current   string
		mustMatch string
	}{
		{"link", `null`, "SKY", "linked"},
		{"unlink", `"SKY"`, "", "unlinked"},
		{"change", `"OLD"`, "NEW", "changed from"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []domain.CuratorContextChange{{
				ChangeType:    domain.ChangeTypeJiraProjectKey,
				BaselineValue: tc.baseline,
			}}
			got := pendingChangesNote(rows, envelopeInputs{JiraProjectKey: tc.current})
			if !strings.Contains(got, tc.mustMatch) {
				t.Errorf("note missing %q: %q", tc.mustMatch, got)
			}
		})
	}
}

// TestPendingChangesNote_Empty returns "" so the caller can short-
// circuit prepend logic without producing a stray "[system note]\n\n"
// at the head of the user's message.
func TestPendingChangesNote_Empty(t *testing.T) {
	got := pendingChangesNote(nil, envelopeInputs{})
	if got != "" {
		t.Errorf("expected empty note for nil rows, got: %q", got)
	}
}
