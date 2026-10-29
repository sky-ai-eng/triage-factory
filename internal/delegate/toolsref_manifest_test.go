package delegate

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// manifestClaimCredentials is a db.ClaimCredentialsStore whose Get answers
// with a fixed source manifest — the executor-path shape, where the brain
// wrote the manifest beside the sealed bundle at provision time.
type manifestClaimCredentials struct {
	db.ClaimCredentialsStore
	sources domain.SourceManifest
}

func (f manifestClaimCredentials) Get(context.Context, string, string) (db.SealedBundle, bool, error) {
	return db.SealedBundle{Sources: f.sources}, true, nil
}

// manifestSpawner builds a spawner whose claim-credentials read answers with
// the given source manifest.
func manifestSpawner(t *testing.T, sources domain.SourceManifest) *Spawner {
	t.Helper()
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	stores.ClaimCredentials = manifestClaimCredentials{sources: sources}
	return NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
}

// kindSet is kinds as the composer reads them: order and repeats carry no
// meaning, since ToolsReferenceForSources canonicalizes both.
func kindSet(kinds []string) []string {
	out := slices.Clone(kinds)
	slices.Sort(out)
	return slices.Compact(out)
}

// TestSourceDocsFor_UsableSourcesAreDocumented pins the executor path: a
// GitHub-triggered run whose engagement sealed Jira and Linear is documented
// both, and a registered source the manifest names usable joins them — the
// answer the executor cannot compute itself, so the brain sealed it.
func TestSourceDocsFor_UsableSourcesAreDocumented(t *testing.T) {
	s := manifestSpawner(t, domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"jira":   {State: domain.SourceSealed},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
		"slack":  {State: domain.SourceSealed},
	})
	got := s.sourceDocsFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m1", eventsource.KindGitHub)

	if want := []string{"github", "jira", "linear", "slack"}; !slices.Equal(kindSet(got.kinds), want) {
		t.Errorf("kinds = %q, want %q", kindSet(got.kinds), want)
	}
	if got.notes == "" || strings.Contains(got.notes, "\n") {
		t.Errorf("notes = %q, want one line, for the one unresolved source", got.notes)
	}
}

// TestSourceDocsFor_DisabledSourceIsLeftOut pins decision 5: a source an
// admin turned off is left out of <tools> even when it is the run's own, and
// named in a <run_context> note instead, as is every other source the run
// cannot use. GitHub stays documented whatever the manifest says.
func TestSourceDocsFor_DisabledSourceIsLeftOut(t *testing.T) {
	s := manifestSpawner(t, domain.SourceManifest{
		"github": {State: domain.SourceUnconfigured},
		"jira":   {State: domain.SourceDisabled},
		"linear": {State: domain.SourceUnconfigured},
		"slack":  {State: domain.SourceUnlicensed},
	})
	got := s.sourceDocsFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m2", eventsource.KindJira)

	if want := []string{"github"}; !slices.Equal(kindSet(got.kinds), want) {
		t.Errorf("kinds = %q, want %q", kindSet(got.kinds), want)
	}
	if n := len(strings.Split(got.notes, "\n")); n != 4 {
		t.Errorf("notes = %q, want one line per source that is not sealed", got.notes)
	}
	if block := runContext("", "/work", "", "", "", got.notes, ""); !strings.Contains(block, got.notes) {
		t.Errorf("<run_context> = %q, want the source notes in it", block)
	}
}

// TestSourceDocsFor_AllSealedSaysNothing pins the quiet case: a run whose
// every source is sealed carries no source notes.
func TestSourceDocsFor_AllSealedSaysNothing(t *testing.T) {
	sealed := domain.SourceEntry{State: domain.SourceSealed}
	s := manifestSpawner(t, domain.SourceManifest{"github": sealed, "jira": sealed, "linear": sealed})
	got := s.sourceDocsFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m3", eventsource.KindGitHub)
	if got.notes != "" {
		t.Errorf("notes = %q, want none when every source is sealed", got.notes)
	}
}

// TestSourceDocsFor_NoManifest_FallsThrough pins the degrade: a row carrying
// no manifest is not an answer, so the composition falls through to the live
// resolve and, failing that, the base superset with no notes.
func TestSourceDocsFor_NoManifest_FallsThrough(t *testing.T) {
	s := manifestSpawner(t, nil)
	got := s.sourceDocsFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m4", eventsource.KindJira)

	if !slices.Contains(got.kinds, "github") {
		t.Errorf("kinds = %q, want github", got.kinds)
	}
	if slices.Contains(got.kinds, "slack") {
		t.Errorf("kinds = %q, want no slack without an answer naming it", got.kinds)
	}
}
