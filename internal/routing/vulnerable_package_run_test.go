package routing

import (
	"context"
	"slices"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// vulnRunDelegator starts runs as stubDelegator does and takes every event
// folded into a running one, recording it.
type vulnRunDelegator struct {
	*stubDelegator
	folded []injectCall
}

func (s *vulnRunDelegator) StageOrDeliverAdditiveEvent(_ context.Context, orgID, conversationID, producer, body string, _ domain.NoteProvenance, firing delegate.AdditiveFiringRef) delegate.AdditiveDelivery {
	s.folded = append(s.folded, injectCall{orgID, conversationID, producer, body, firing})
	return delegate.AdditiveDelivery{Outcome: delegate.InjectDeliveredLocal}
}

// delegatedPackage is a vulnFixture whose team A runs a trigger on every
// alert, and the run that trigger started on the package's task for alert 1.
type delegatedPackage struct {
	*vulnFixture
	sp             *vulnRunDelegator
	taskID, convID string
	first          domain.DependabotAlert
}

func newDelegatedPackage(t *testing.T) delegatedPackage {
	t.Helper()
	f := newVulnFixture(t)
	sp := &vulnRunDelegator{stubDelegator: &stubDelegator{db: f.db}}
	f.router.spawner = sp
	enableTeamAutoDelegate(t, f.db, f.teamA)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamA, "", 0.6, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindTrigger, f.teamA, "", 0, false)

	first := vulnAlert(1, domain.DependabotSeverityHigh)
	f.listing(t, lodashSnapshot(), lodashSnapshot(first))
	tasks := f.activeTasks(t)
	if len(tasks) != 1 || sp.calls != 1 {
		t.Fatalf("setup: %d tasks and %d runs, want one of each", len(tasks), sp.calls)
	}
	var convID string
	if err := f.db.QueryRow(`SELECT id FROM conversations WHERE task_id = ?`, tasks[0].ID).Scan(&convID); err != nil {
		t.Fatalf("read the run's conversation: %v", err)
	}
	return delegatedPackage{vulnFixture: f, sp: sp, taskID: tasks[0].ID, convID: convID, first: first}
}

// An alert that opens on a package while a run works on it reaches that run
// as a follow-up carrying the new alert, and starts no second run.
func TestVulnerablePackage_LaterAlertFoldsIntoTheWorkingRun(t *testing.T) {
	d := newDelegatedPackage(t)

	second := vulnAlert(2, domain.DependabotSeverityCritical)
	d.listing(t, lodashSnapshot(d.first), lodashSnapshot(d.first, second))

	if d.sp.calls != 1 {
		t.Errorf("%d runs started, want only the first", d.sp.calls)
	}
	if len(d.sp.folded) != 1 {
		t.Fatalf("%d events folded into the run, want one", len(d.sp.folded))
	}
	got := d.sp.folded[0]
	if got.conversationID != d.convID || got.producer != domain.EventGitHubVulnerabilityAlertOpened || got.firing.TaskID != d.taskID {
		t.Errorf("folded %+v, want alert_opened on conversation %s of task %s", got, d.convID, d.taskID)
	}
	if !strings.Contains(got.body, `"alert_number":2`) {
		t.Errorf("the follow-up does not carry the new alert:\n%s", got.body)
	}
	if tasks := d.activeTasks(t); len(tasks) != 1 || tasks[0].ID != d.taskID {
		t.Errorf("tasks = %+v, want the one task the run is working on", tasks)
	}
}

// The package resolving closes the task under the working run and tells it,
// through the note every close sends.
func TestVulnerablePackage_ResolutionTellsTheWorkingRun(t *testing.T) {
	d := newDelegatedPackage(t)

	d.listing(t, lodashSnapshot(d.first), lodashSnapshot())

	if status, reason, eventType := taskRow(t, d.db, d.taskID); status != "done" || reason != "entity_closed" || eventType != domain.EventGitHubVulnerabilityResolved {
		t.Errorf("task = (%q, %q, %q), want closed by the resolution", status, reason, eventType)
	}
	if got := d.sp.notedIDs(); !slices.Equal(got, []string{d.convID}) {
		t.Errorf("told of the close = %v, want [%s]", got, d.convID)
	}
}

// A resolution while the run's own pull request awaits a person's verdict
// closes the package and holds the task's close. The run is told the close is
// held, and told the task closed once the person resolves the pull request.
func TestVulnerablePackage_ResolutionHeldWhileTheRunsPullRequestAwaitsAPerson(t *testing.T) {
	d := newDelegatedPackage(t)
	pr, _, err := sqlitestore.New(d.db).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "https://github.com", "octo/api#42", "", "pr", "Upgrade lodash to 4.17.21", "https://github.com/octo/api/pull/42")
	if err != nil {
		t.Fatalf("create the run's pull request: %v", err)
	}
	seedDraftPullRequest(t, d.db, d.convID, pr.ID)

	d.listing(t, lodashSnapshot(d.first), lodashSnapshot())

	var state string
	if err := d.db.QueryRow(`SELECT state FROM entities WHERE id = ?`, d.entity.ID).Scan(&state); err != nil {
		t.Fatalf("read the package: %v", err)
	}
	if state != "closed" {
		t.Errorf("package state = %q, want closed", state)
	}
	if heldClose(t, d.db, d.taskID) == "" {
		t.Fatal("the resolution's close was not held")
	}
	if status, _, _ := taskRow(t, d.db, d.taskID); status == "done" {
		t.Error("the task closed while the run's pull request awaits a person")
	}
	if got := d.sp.heldNotedIDs(); !slices.Equal(got, []string{d.convID}) {
		t.Errorf("told of the held close = %v, want [%s]", got, d.convID)
	}
	if got := d.sp.notedIDs(); len(got) != 0 {
		t.Errorf("told the task closed = %v before it did", got)
	}

	resolveDrafts(t, d.db, d.convID)
	d.router.sweepCloseDeferrals(t.Context())

	if status, reason, _ := taskRow(t, d.db, d.taskID); status != "done" || reason != "entity_closed" {
		t.Errorf("task after the person's verdict = (%q, %q), want closed by the resolution", status, reason)
	}
	if got := d.sp.notedIDs(); !slices.Equal(got, []string{d.convID}) {
		t.Errorf("told of the close = %v, want [%s]", got, d.convID)
	}
}
