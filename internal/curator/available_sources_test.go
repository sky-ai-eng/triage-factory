package curator

import (
	"context"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAvailableSources_ReflectsWhatTheOrgCanReach pins the wiring the commit
// message claims but nothing exercised: a curator turn asks the same
// availability question the delegated path does, rather than assuming both
// core families are always there.
func TestAvailableSources_ReflectsWhatTheOrgCanReach(t *testing.T) {
	keyring.MockInit()
	database := newTestDB(t)
	stores := sqlitestore.New(database)
	if err := integrations.Save(context.Background(), stores.Secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: "https://github.example.com", GitHubPAT: "ghp-x",
	}); err != nil {
		t.Fatalf("save github credential: %v", err)
	}

	sess := &projectSession{
		curator: &Curator{stores: stores},
		ctx:     context.Background(),
	}
	item := queueItem{orgID: runmode.LocalDefaultOrgID, creatorUserID: runmode.LocalDefaultUserID}

	got := sess.availableSources(item)
	if len(got) != 1 || got[0] != "github" {
		t.Errorf("availableSources = %v, want exactly [github] — jira has no credential configured", got)
	}
}

// TestAvailableSources_NothingConfiguredIsEmpty is the fallback's precondition:
// an org with no credentials at all resolves to no available sources, which is
// what makes renderToolsReference's len(sources)==0 fallback the right test
// for "nothing was resolved" rather than "the org has nothing".
func TestAvailableSources_NothingConfiguredIsEmpty(t *testing.T) {
	keyring.MockInit()
	database := newTestDB(t)
	stores := sqlitestore.New(database)

	sess := &projectSession{
		curator: &Curator{stores: stores},
		ctx:     context.Background(),
	}
	item := queueItem{orgID: runmode.LocalDefaultOrgID, creatorUserID: runmode.LocalDefaultUserID}

	if got := sess.availableSources(item); len(got) != 0 {
		t.Errorf("availableSources = %v, want empty — no credential is configured for anything", got)
	}
}

// TestAvailableSources_ResolveFailureDegradesToNil pins fail-open at the
// curator's own call site: a resolve failure must not panic or block the
// turn, and must hand renderToolsReference nil so it falls back to
// documenting both core families rather than silently omitting one.
func TestAvailableSources_ResolveFailureDegradesToNil(t *testing.T) {
	keyring.MockInit()
	database := newTestDB(t)
	stores := sqlitestore.New(database)

	sess := &projectSession{
		curator: &Curator{stores: stores},
		ctx:     context.Background(),
	}
	// An orgID that isn't the local sentinel fails SyntheticClaimsWithTx's own
	// assertion in local mode — a cheap, real way to force the resolve to
	// error without a fake store.
	item := queueItem{orgID: "not-the-local-org", creatorUserID: runmode.LocalDefaultUserID}

	if got := sess.availableSources(item); got != nil {
		t.Errorf("availableSources = %v, want nil on a failed resolve", got)
	}
}
