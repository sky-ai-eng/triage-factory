package dbtest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// DeliverableListFixture is a fresh store with an org and a team in it, and a
// way to add a conversation to that team.
type DeliverableListFixture struct {
	Stores          db.Stores
	OrgID           string
	TeamID          string
	NewConversation func(t *testing.T) string
}

// RunDeliverableConversationListConformance covers ListByConversation's
// window: every row of the conversation and none of another's, newest first
// with the id breaking ties, offset pages that join back into that order, the
// conversation's total on every page, and the count-only read.
func RunDeliverableConversationListConformance(t *testing.T, mk func(t *testing.T) DeliverableListFixture) {
	t.Helper()
	ctx := context.Background()
	f := mk(t)
	conversationID := f.NewConversation(t)
	other := f.NewConversation(t)

	record := func(conversationID string, n int64) {
		t.Helper()
		a := domain.Deliverable{
			ConversationID: conversationID, OrgID: f.OrgID, TeamID: f.TeamID,
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Kind: domain.DeliverableKindPullRequest,
			Target: fmt.Sprintf("octo/api#%d", n), State: domain.DeliverableStatePRDraft,
			DedupKey: domain.PullRequestDedupKey(fmt.Sprintf("PR_%d", n)),
		}
		if _, err := f.Stores.Deliverables.UpsertSystem(ctx, f.OrgID, a); err != nil {
			t.Fatalf("record deliverable %d: %v", n, err)
		}
	}
	const rows = 5
	for n := int64(1); n <= rows; n++ {
		record(conversationID, n)
	}
	record(other, 99)

	all, total, err := f.Stores.Deliverables.ListByConversation(ctx, f.OrgID, conversationID, db.Unwindowed)
	if err != nil {
		t.Fatalf("ListByConversation unwindowed: %v", err)
	}
	if len(all) != rows || total != rows {
		t.Fatalf("unwindowed read = %d rows, total %d; want %d of %d (the other conversation's row must not appear)",
			len(all), total, rows, rows)
	}
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if cur.CreatedAt.After(prev.CreatedAt) || (cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID > prev.ID) {
			t.Errorf("row %d (%s, %s) is ahead of row %d (%s, %s); want created_at DESC, id DESC",
				i, cur.CreatedAt, cur.ID, i-1, prev.CreatedAt, prev.ID)
		}
	}

	var paged []string
	for offset := 0; offset < rows; offset += 2 {
		page, total, err := f.Stores.Deliverables.ListByConversation(ctx, f.OrgID, conversationID,
			db.ListOpts{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("ListByConversation at offset %d: %v", offset, err)
		}
		if total != rows {
			t.Errorf("total at offset %d = %d, want %d on every page", offset, total, rows)
		}
		if want := min(2, rows-offset); len(page) != want {
			t.Errorf("page at offset %d = %d rows, want %d", offset, len(page), want)
		}
		for _, a := range page {
			paged = append(paged, a.ID)
		}
	}
	want := make([]string, len(all))
	for i, a := range all {
		want[i] = a.ID
	}
	if !slices.Equal(paged, want) {
		t.Errorf("pages joined = %v, want the unwindowed order %v", paged, want)
	}

	counted, total, err := f.Stores.Deliverables.ListByConversation(ctx, f.OrgID, conversationID, db.ListOpts{CountOnly: true})
	if err != nil {
		t.Fatalf("ListByConversation count-only: %v", err)
	}
	if len(counted) != 0 || total != rows {
		t.Errorf("count-only read = %d rows, total %d; want no rows and %d", len(counted), total, rows)
	}
}
