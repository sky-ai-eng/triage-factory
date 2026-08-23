package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// manifestClaimCredentials is a db.ClaimCredentialsStore whose Get answers
// with a fixed stamped tools manifest — the executor-path shape, where the
// brain wrote include_tools beside the sealed bundle at provision time.
type manifestClaimCredentials struct {
	db.ClaimCredentialsStore
	includeTools []string
}

func (f manifestClaimCredentials) Get(context.Context, string, string) (db.SealedBundle, bool, error) {
	return db.SealedBundle{IncludeTools: f.includeTools}, true, nil
}

// manifestSpawner builds a spawner whose claim-credentials read answers with
// the given stamped manifest.
func manifestSpawner(t *testing.T, includeTools []string) *Spawner {
	t.Helper()
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	stores.ClaimCredentials = manifestClaimCredentials{includeTools: includeTools}
	return NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
}

// TestToolsReferenceFor_StampedManifest_ComposesRegisteredSource pins the
// executor path: a claim whose stamped include_tools names slack documents the
// registered slack verbs even though the run's own source is github. This is
// the answer the executor cannot compute itself — the brain stamped it — and
// it is what puts the slack docs on a GitHub- or Jira-triggered native run.
func TestToolsReferenceFor_StampedManifest_ComposesRegisteredSource(t *testing.T) {
	t.Cleanup(agentprompt.ResetToolsReferences)
	agentprompt.RegisterToolsReference("slack", "SLACK VERB DOCS")

	s := manifestSpawner(t, []string{"github", "slack"})
	got := s.toolsReferenceFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m1", eventsource.KindGitHub)

	if !strings.Contains(got, "exec gh") || !strings.Contains(got, "SLACK VERB DOCS") {
		t.Errorf("toolsRef = %q, want both the github and the manifest's slack verbs", got)
	}
}

// TestToolsReferenceFor_StampedManifest_UnionsTheRunsOwnSource pins the floor
// under the manifest: the run's own source (and github) stay documented even
// when the stamp omits them — a run never loses the verbs for the entity that
// triggered it.
func TestToolsReferenceFor_StampedManifest_UnionsTheRunsOwnSource(t *testing.T) {
	t.Cleanup(agentprompt.ResetToolsReferences)
	agentprompt.RegisterToolsReference("slack", "SLACK VERB DOCS")

	s := manifestSpawner(t, []string{})
	got := s.toolsReferenceFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m2", "slack")

	if !strings.Contains(got, "exec gh") || !strings.Contains(got, "SLACK VERB DOCS") {
		t.Errorf("toolsRef = %q, want the base union (github + the run's own source) over an empty manifest", got)
	}
}

// TestToolsReferenceFor_StampedManifest_ExcludesWhatItOmits pins the other
// direction: a real (non-nil) manifest that omits slack keeps the slack docs
// out of an unrelated run — the stamp is the org's availability answer, and a
// source it excludes (unconnected, paused, unlicensed) is not advertised.
func TestToolsReferenceFor_StampedManifest_ExcludesWhatItOmits(t *testing.T) {
	t.Cleanup(agentprompt.ResetToolsReferences)
	agentprompt.RegisterToolsReference("slack", "SLACK VERB DOCS")

	s := manifestSpawner(t, []string{"github"})
	got := s.toolsReferenceFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m3", eventsource.KindGitHub)

	if strings.Contains(got, "SLACK VERB DOCS") {
		t.Errorf("toolsRef = %q, want slack absent when the manifest omits it", got)
	}
	if !strings.Contains(got, "exec gh") {
		t.Errorf("toolsRef = %q, want the github verbs regardless", got)
	}
}

// TestToolsReferenceFor_NilManifest_FallsThrough pins nil-vs-empty: a row
// whose writer stamped no answer (include_tools NULL — a failed brain-side
// resolve) is not an answer of "nothing available", so the resolve falls
// through to the live claims path and, failing that, the base superset.
func TestToolsReferenceFor_NilManifest_FallsThrough(t *testing.T) {
	t.Cleanup(agentprompt.ResetToolsReferences)
	agentprompt.RegisterToolsReference("slack", "SLACK VERB DOCS")

	s := manifestSpawner(t, nil)
	got := s.toolsReferenceFor(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "conv-m4", eventsource.KindJira)

	if !strings.Contains(got, "exec gh") || !strings.Contains(got, "exec jira") {
		t.Errorf("toolsRef = %q, want the base superset (github + the run's own source)", got)
	}
	if strings.Contains(got, "SLACK VERB DOCS") {
		t.Errorf("toolsRef = %q, want no slack docs without an availability answer naming it", got)
	}
}
