package credprovision

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestIncludeToolsFor_StampsTheAvailabilityAnswer pins the happy path of the
// manifest stamp: the brain resolves the org's available sources through the
// claims-free door and hands them to Put — a configured source is in the
// answer, an unconfigured one is not, and the answer is non-nil (a real
// answer, distinguishable from the nil a failed resolve degrades to).
func TestIncludeToolsFor_StampsTheAvailabilityAnswer(t *testing.T) {
	keyring.MockInit()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)
	if err := integrations.Save(t.Context(), stores.Secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubPAT: "ghp_test"}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	m := &Manager{stores: stores}
	got := m.includeToolsFor(t.Context(), runmode.LocalDefaultOrgID, "conv-1")
	if got == nil {
		t.Fatal("includeToolsFor = nil, want a real (non-nil) answer from a working resolve")
	}
	if !slices.Contains(got, "github") {
		t.Errorf("includeToolsFor = %v, want the configured github source included", got)
	}
	if slices.Contains(got, "jira") {
		t.Errorf("includeToolsFor = %v, want the unconfigured jira source excluded", got)
	}
}

// TestIncludeToolsFor_DegradesToNil pins the failure contract: stores that
// cannot run the availability probes stamp nil — "no answer", which the
// executor reads as fall-back — never an empty answer that would read as
// "nothing available".
func TestIncludeToolsFor_DegradesToNil(t *testing.T) {
	m := &Manager{stores: db.Stores{}}
	if got := m.includeToolsFor(t.Context(), "org-1", "conv-1"); got != nil {
		t.Errorf("includeToolsFor over unusable stores = %v, want nil", got)
	}
}
