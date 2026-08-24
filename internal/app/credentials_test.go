package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// enableSetStores is a fresh local install with the org and team enable-sets a
// case wants. A nil set is the absent value on that scope, which is the state a
// deployment nobody has configured is in.
func enableSetStores(t *testing.T, orgSet, teamSet []string, teamDefault string) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	org, err := stores.Orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	org.EnabledModels = orgSet
	if _, err := stores.Orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, org); err != nil {
		t.Fatalf("write org settings: %v", err)
	}

	team, err := stores.Teams.GetSettingsSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("read team settings: %v", err)
	}
	team.EnabledModels = teamSet
	team.DefaultModel = teamDefault
	if _, err := stores.Teams.UpdateSettings(ctx, runmode.LocalDefaultTeamID, team); err != nil {
		t.Fatalf("write team settings: %v", err)
	}
	return stores, conn
}

// A default both sets admit resolves, and hands back the set every model the
// run touches is held to.
func TestResolveAIModelForTeam_DefaultInTheSet(t *testing.T) {
	stores, _ := enableSetStores(t, nil, nil, domain.ModelSonnet)

	got, err := resolveAIModelForTeam(context.Background(), stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("resolveAIModelForTeam: %v", err)
	}
	model, err := got.RequireDefault()
	if err != nil {
		t.Fatalf("RequireDefault: %v", err)
	}
	if model != domain.ModelSonnet {
		t.Errorf("default = %q, want %q", model, domain.ModelSonnet)
	}
	// Both sets absent → the catalog default, so the whole catalog is pinnable.
	for _, key := range modelcatalog.DefaultEnabled() {
		if !got.Enabled().Has(key) {
			t.Errorf("%s: not enabled with both sets absent", key)
		}
	}
}

// The claim gate: a team whose default its own enable-set no longer includes
// refuses, naming the model and the set that excludes it. It never substitutes
// — the alternative is a transcript that lies about what produced it and a
// ledger that lies about what was bought.
//
// The resolve itself succeeds: a pinned step is held to the set by its own pin
// and has no business failing over a default it never reads, so the refusal
// lands at RequireDefault rather than upstream of it.
func TestResolveAIModelForTeam_DefaultOutsideTheSetRefuses(t *testing.T) {
	// The org narrowed to Haiku after this team picked Opus. Nothing rewrote
	// the team's row, which is exactly the case this gate exists for.
	stores, _ := enableSetStores(t, []string{domain.ModelHaiku}, nil, domain.ModelOpus)

	got, err := resolveAIModelForTeam(context.Background(), stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("resolveAIModelForTeam: %v", err)
	}
	model, err := got.RequireDefault()
	if !errors.Is(err, domain.ErrModelNotEnabled) {
		t.Fatalf("err = %v, want ErrModelNotEnabled", err)
	}
	if model != "" {
		t.Errorf("a refusal handed back %q; want no model at all", model)
	}
	if !strings.Contains(err.Error(), domain.ModelOpus) {
		t.Errorf("error %q does not name the model", err)
	}
	if !strings.Contains(err.Error(), domain.ModelHaiku) {
		t.Errorf("error %q does not name the set that excludes it", err)
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say where to fix it", err)
	}
	// The set still resolved, and the pin the org DOES enable still dispatches:
	// a disabled default breaks the steps that inherit it, not the ones that
	// name a model of their own.
	if !got.Enabled().Has(domain.ModelHaiku) {
		t.Error("the refusal took the enable-set with it")
	}
}

// The team's own narrowing binds it too, not just the org's: a team that
// narrowed past its own default is broken the same way, and by the same rule.
func TestResolveAIModelForTeam_TeamNarrowedPastItsOwnDefault(t *testing.T) {
	stores, _ := enableSetStores(t, nil, []string{domain.ModelHaiku}, domain.ModelOpus)

	got, err := resolveAIModelForTeam(context.Background(), stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("resolveAIModelForTeam: %v", err)
	}
	if _, err := got.RequireDefault(); !errors.Is(err, domain.ErrModelNotEnabled) {
		t.Fatalf("err = %v, want ErrModelNotEnabled", err)
	}
}

// A read that does not answer is the check's input missing, which is not the
// check passing. Resolving through it would dispatch a model nobody can show is
// enabled, so it refuses rather than reaching for a shipped default — there
// isn't one.
func TestResolveAIModelForTeam_UnreadableStateRefuses(t *testing.T) {
	stores, conn := enableSetStores(t, nil, nil, domain.ModelSonnet)
	if err := conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := resolveAIModelForTeam(context.Background(), stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err == nil {
		t.Error("an unreadable settings row resolved a model instead of refusing")
	}
}
