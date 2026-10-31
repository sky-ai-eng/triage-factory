package notifytest_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
)

type team struct {
	ID string `json:"id"`
}

func teamID(t team) string { return t.ID }

func render(t team, _ notify.Recipient) notify.Content { return notify.Content{Title: t.ID} }

var (
	linearKind = notify.Condition(notify.ConditionSpec[team]{
		Name: "notifytest.linear", Source: "linear", Priority: notify.Always(notify.High),
		Key: teamID, Audience: notify.OrgAdmins(), Render: render, Example: team{ID: "x"},
	})
	jiraKind = notify.Condition(notify.ConditionSpec[team]{
		Name: "notifytest.jira", Source: "jira", Priority: notify.Always(notify.High),
		Key: teamID, Audience: notify.OrgAdmins(), Render: render, Example: team{ID: "x"},
	})
	sourcelessKind = notify.Condition(notify.ConditionSpec[team]{
		Name: "notifytest.sourceless", Priority: notify.Always(notify.High),
		Key: teamID, Audience: notify.OrgAdmins(), Render: render, Example: team{ID: "x"},
	})
	linearNotice = notify.Notice(notify.NoticeSpec[team]{
		Name: "notifytest.notice", Source: "linear", TTL: notify.DefaultNoticeTTL,
		Key: teamID, Audience: notify.OrgMembers(), Render: render, Example: team{ID: "x"},
	})
)

func TestRecorder_ModelsTheVerbs(t *testing.T) {
	rec := notifytest.Record(t)
	ctx := context.Background()
	const org = "org-verbs"

	linearKind.Set(ctx, org, team{ID: "a"}, true)
	linearKind.Set(ctx, org, team{ID: "a"}, true)
	rec.Active(t, org, linearKind, "a")
	if open := rec.Open(org, linearKind); len(open) != 1 || open[0].Seen != 2 || open[0].Params != (team{ID: "a"}) {
		t.Errorf("Open = %+v; want a, seen twice, with its params", open)
	}
	linearKind.Set(ctx, org, team{ID: "a"}, false)
	rec.Inactive(t, org, linearKind)

	linearKind.Sync(ctx, org, []team{{ID: "a"}, {ID: "b"}, {ID: "b"}})
	linearKind.Sync(ctx, org, []team{{ID: "b"}, {ID: "c"}})
	rec.Active(t, org, linearKind, "b", "c")
	// b was raised once per pass, as the engine's seen_count would be.
	if open := rec.Open(org, linearKind); len(open) != 2 || open[0].Seen != 2 {
		t.Errorf("Open after the second Sync = %+v; want b seen twice and c", open)
	}

	jiraKind.Set(ctx, org, team{ID: "j"}, true)
	linearNotice.Post(ctx, org, team{ID: "n"})
	// An empty source names no kind's, so a kind with none stays open.
	sourcelessKind.Set(ctx, org, team{ID: "s"}, true)
	notify.ClearSource(ctx, org, "")
	rec.Active(t, org, sourcelessKind, "s")
	notify.ClearSource(ctx, org, "linear")
	rec.Inactive(t, org, linearKind)
	rec.Active(t, org, jiraKind, "j")
	rec.Active(t, org, linearNotice, "n")
}

// Every recorder sees every call; the org is what keeps parallel tests apart.
func TestRecorder_AssertionsReadTheirOwnOrg(t *testing.T) {
	for _, org := range []string{"org-a", "org-b"} {
		t.Run(org, func(t *testing.T) {
			t.Parallel()
			rec := notifytest.Record(t)
			linearKind.Set(context.Background(), org, team{ID: org}, true)
			rec.Active(t, org, linearKind, org)
			if open := rec.Open(org, linearKind); len(open) != 1 {
				t.Errorf("Open(%s) = %+v; want only this test's occurrence", org, open)
			}
		})
	}
}
