package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// JiraAppsStoreFactory hands the conformance suite a wired store, the orgID to
// scope every call, and the user id to attribute a registration to (empty
// where the dialect has no users table to satisfy). org_jira_apps is one row
// per org, so there is nothing else to seed.
type JiraAppsStoreFactory func(t *testing.T) (store db.JiraAppsStore, orgID, userID string)

// RunJiraAppsReturnedRowConformance covers the returned-row standard on
// UpsertForOrg: the write hands back the row it persisted, and that row is the
// one a point read finds. registered_at is why the rule is here — the conflict
// arm preserves the original, so on a re-register the stored row disagrees
// with the struct the caller passed.
func RunJiraAppsReturnedRowConformance(t *testing.T, mk JiraAppsStoreFactory) {
	t.Helper()

	t.Run("UpsertForOrg_returns_the_stored_row", func(t *testing.T) {
		store, orgID, userID := mk(t)
		ctx := context.Background()
		read := func() (*domain.OrgJiraApp, error) { return store.GetForOrg(ctx, orgID) }

		first, err := store.UpsertForOrg(ctx, domain.OrgJiraApp{
			OrgID: orgID, ClientID: "atl-ret-1",
			ClientSecretRef: "jira_oauth_client_secret", RegisteredByUserID: userID,
		})
		if err != nil {
			t.Fatalf("UpsertForOrg (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertForOrg (insert)", first, read)
		if first.RegisteredAt.IsZero() {
			t.Error("UpsertForOrg returned a row with no registered_at, the column the insert defaults")
		}

		replaced, err := store.UpsertForOrg(ctx, domain.OrgJiraApp{
			OrgID: orgID, ClientID: "atl-ret-2",
			ClientSecretRef: "jira_oauth_client_secret", RegisteredByUserID: userID,
		})
		if err != nil {
			t.Fatalf("UpsertForOrg (replace): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertForOrg (replace)", replaced, read)
		if replaced.ClientID != "atl-ret-2" || !replaced.RegisteredAt.Equal(first.RegisteredAt) {
			t.Errorf("replace returned %+v, want the new client id with the original registered_at", replaced)
		}
	})
}
