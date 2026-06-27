package agenthost

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The TFAC-483 capture tests prove the bot funnels append the right
// external_actions audit row alongside the artifact upsert — the finer-grained
// action discriminator the artifact's state can't carry, the org credential, the
// run + actor attribution, and the branch hook+proxy dedup. The LocalClient is
// the one seam the multi daemon dispatches through, so this covers both modes.

// newCaptureStores builds a real SQLite store bundle (Artifacts + ExternalActions
// + Tx live) with a seeded run, no Jira credential needed (the branch path and
// the injected-error paths don't call out). eventTriggered picks the write path:
// admin pool (no user) when true, a synthetic-claims tx (manual run, with a user)
// when false.
func newCaptureStores(t *testing.T, eventTriggered bool) (db.Stores, RunInfo) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	const runID = "11111111-1111-1111-1111-111111111111"
	if _, err := conn.Exec(`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'running')`, runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	userID := ""
	if !eventTriggered {
		userID = runmode.LocalDefaultUserID
	}
	return sqlitestore.New(conn), RunInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		TeamID:           runmode.LocalDefaultTeamID,
		RunID:            runID,
		UserID:           userID,
		IsEventTriggered: eventTriggered,
	}
}

func listExternalActions(t *testing.T, stores db.Stores) []domain.ExternalAction {
	t.Helper()
	rows, err := stores.ExternalActions.ListByOrgSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	return rows
}

// TestCapture_JiraActions_RecordExternalActions pins that a Jira create and a
// transition each append one external_actions row with the right action (finer
// than the artifact's created/updated state), the org Jira credential, the run +
// actor attribution, and the transition's to_state — across both write paths.
func TestCapture_JiraActions_RecordExternalActions(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStoresForCapture(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if _, err := client.JiraCreateIssue(context.Background(), "SKY", "Task", "do a thing", "", "", ""); err != nil {
					t.Fatalf("JiraCreateIssue: %v", err)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 {
					t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
				}
				a := acts[0]
				if a.Action != domain.ActionIssueCreated || a.Provider != domain.ArtifactProviderJira ||
					a.Credential != domain.CredentialJiraOrg || a.Target != "SKY-1" || a.RunID != info.RunID ||
					a.TeamID != runmode.LocalDefaultTeamID {
					t.Errorf("create action mismatch: %+v", a)
				}
				assertActor(t, a, eventTriggered)
			})

			t.Run("transition carries from/to", func(t *testing.T) {
				jira := startFakeJira(t)
				stores, info := newJiraRecordingStoresForCapture(t, jira.URL, eventTriggered)
				client := NewLocal(stores, info)

				if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
					t.Fatalf("JiraTransitionTo: %v", err)
				}
				acts := listExternalActions(t, stores)
				if len(acts) != 1 {
					t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
				}
				a := acts[0]
				if a.Action != domain.ActionIssueTransitioned || a.ToState != "Done" || a.Credential != domain.CredentialJiraOrg {
					t.Errorf("transition action mismatch: %+v", a)
				}
			})
		})
	}
}

// TestCapture_BranchPush_RecordsActionAndDedupsTwin pins the branch capture: a
// push records ActionBranchPushed under the github credential with the
// deterministic run:ref:sha dedup key, and a second observation of the SAME push
// (the git hook+proxy twin) collapses to one action row.
func TestCapture_BranchPush_RecordsActionAndDedupsTwin(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			client := NewLocal(stores, info)
			ctx := context.Background()

			branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
			if !ok {
				t.Fatal("NewBranchArtifact returned ok=false")
			}
			// The hook fires, then the proxy backstop observes the same push.
			if _, err := client.UpsertArtifact(ctx, branch); err != nil {
				t.Fatalf("hook UpsertArtifact: %v", err)
			}
			if _, err := client.UpsertArtifact(ctx, branch); err != nil {
				t.Fatalf("proxy UpsertArtifact (twin): %v", err)
			}

			acts := listExternalActions(t, stores)
			if len(acts) != 1 {
				t.Fatalf("branch twin did not collapse: want 1 action, got %d: %+v", len(acts), acts)
			}
			a := acts[0]
			if a.Action != domain.ActionBranchPushed || a.Provider != domain.ArtifactProviderGitHub ||
				a.Credential != domain.CredentialGitHubApp || a.Target != "octo/repo" ||
				a.ExternalID != "refs/heads/feat" {
				t.Errorf("branch action mismatch: %+v", a)
			}
			if want := domain.BranchPushDedupKey(info.RunID, "refs/heads/feat", "abc123"); a.DedupKey != want {
				t.Errorf("branch dedup_key = %q, want %q", a.DedupKey, want)
			}
			// A genuinely new push (new sha) is recorded distinctly.
			next, _ := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "def456", false)
			if _, err := client.UpsertArtifact(ctx, next); err != nil {
				t.Fatalf("new-push UpsertArtifact: %v", err)
			}
			if got := len(listExternalActions(t, stores)); got != 2 {
				t.Errorf("a new push (new sha) should append: want 2 actions, got %d", got)
			}
		})
	}
}

// TestCapture_EventPath_RecordFailure_DoesNotFailAction pins the event-path
// best-effort contract: a forced RecordSystem failure is swallowed (the action
// row is absent) while the artifact still lands and the call returns success —
// the external write already took effect, so recording must never fail it.
func TestCapture_EventPath_RecordFailure_DoesNotFailAction(t *testing.T) {
	jira := startFakeJira(t)
	stores, info := newJiraRecordingStoresForCapture(t, jira.URL, true) // event-triggered → admin pool
	stores.ExternalActions = erroringExternalActions{}
	client := NewLocal(stores, info)

	if err := client.JiraTransitionTo(context.Background(), "SKY-9", "Done"); err != nil {
		t.Fatalf("transition must not fail when action recording fails: %v", err)
	}
	// The artifact (the separate admin write) still committed.
	if arts := listRunArtifacts(t, stores, info.RunID); len(arts) != 1 {
		t.Errorf("artifact should persist despite the action-record failure, got %d", len(arts))
	}
}

// assertActor checks the actor model: an event-triggered (autonomous) run has no
// actor (NULL); a manual run carries the kicking-off user.
func assertActor(t *testing.T, a domain.ExternalAction, eventTriggered bool) {
	t.Helper()
	if eventTriggered {
		if a.ActorUserID != "" {
			t.Errorf("event-triggered action should have no actor, got %q", a.ActorUserID)
		}
	} else if a.ActorUserID != runmode.LocalDefaultUserID {
		t.Errorf("manual action actor = %q, want the kicking-off user", a.ActorUserID)
	}
}

// newJiraRecordingStoresForCapture is newJiraRecordingStores but also sets a
// non-empty UserID on the manual path so the actor assertion has something to
// check (the shared helper leaves it empty).
func newJiraRecordingStoresForCapture(t *testing.T, jiraURL string, eventTriggered bool) (db.Stores, RunInfo) {
	t.Helper()
	stores, info := newJiraRecordingStores(t, jiraURL, eventTriggered)
	if !eventTriggered {
		info.UserID = runmode.LocalDefaultUserID
	}
	return stores, info
}

// erroringExternalActions fails every Record so the best-effort path is
// exercised. The List methods are unused here (nil embedded interface).
type erroringExternalActions struct{ db.ExternalActionStore }

func (erroringExternalActions) Record(context.Context, string, domain.ExternalAction) error {
	return errors.New("boom")
}

func (erroringExternalActions) RecordSystem(context.Context, string, domain.ExternalAction) error {
	return errors.New("boom")
}
