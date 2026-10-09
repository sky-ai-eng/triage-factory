package dbtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgsStoreFactory hands the returned-row conformance suite a wired OrgsStore
// plus the orgID every write in the suite addresses. Callers own row and
// claims seeding — this harness is schema- and auth-blind.
type OrgsStoreFactory func(t *testing.T) (store db.OrgsStore, orgID string)

// RunOrgsReturnedRowConformance pins the returned-row standard on OrgsStore's
// settings writers: what each write hands back is what a follow-up
// GetSettings finds. Intended for a claims-carrying connection (the Postgres
// app pool) in addition to the admin pool — see
// TestOrgsStore_Postgres_ReturnedRowConformance's doc for why the admin pool
// alone (BYPASSRLS) can't prove the RETURNING clause survives RLS.
//
// The whole suite runs against ONE org_settings row, in subtest declaration
// order (Go runs t.Run subtests sequentially): org_settings is one row per
// org, so a create arm is only reachable while none exists yet.
// UpdateSettingsVersioned's create arm (expected=0) therefore runs FIRST, and
// every later write shares the row it created — mirroring
// TestJiraAppsStore_Postgres_ReturnedRowConformance's single-shared-row shape.
func RunOrgsReturnedRowConformance(t *testing.T, mk OrgsStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store, orgID := mk(t)
	read := func() (*domain.OrgSettings, error) {
		set, err := store.GetSettings(ctx, orgID)
		if err != nil {
			return nil, err
		}
		return &set, nil
	}

	var version int
	t.Run("UpdateSettingsVersioned_create_returns_the_stored_row", func(t *testing.T) {
		created, err := store.UpdateSettingsVersioned(ctx, orgID, domain.OrgSettings{
			GitHubBaseURL: "https://vret-1.example.com", GitHubPollInterval: 5 * time.Minute,
			JiraPollInterval: 5 * time.Minute, GitHubCloneProtocol: "ssh",
		}, 0)
		if err != nil {
			t.Fatalf("UpdateSettingsVersioned (create): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateSettingsVersioned (create)", created, read)
		if created.Version != 1 {
			t.Fatalf("UpdateSettingsVersioned (create) version = %d, want 1", created.Version)
		}
		version = created.Version
	})

	t.Run("UpdateSettingsVersioned_update_returns_the_stored_row_and_conflict_writes_nothing", func(t *testing.T) {
		updated, err := store.UpdateSettingsVersioned(ctx, orgID, domain.OrgSettings{
			GitHubBaseURL: "https://vret-2.example.com", GitHubPollInterval: 5 * time.Minute,
			JiraPollInterval: 5 * time.Minute, GitHubCloneProtocol: "ssh",
		}, version)
		if err != nil {
			t.Fatalf("UpdateSettingsVersioned (update): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateSettingsVersioned (update)", updated, read)
		if updated.Version != version+1 {
			t.Errorf("UpdateSettingsVersioned (update) version = %d, want %d", updated.Version, version+1)
		}
		version = updated.Version

		// A stale version conflict returns the zero value alongside the
		// sentinel, and writes nothing — the row a follow-up read finds is
		// still the winner's.
		zero, err := store.UpdateSettingsVersioned(ctx, orgID, domain.OrgSettings{
			GitHubBaseURL: "https://loser.example.com", GitHubPollInterval: 5 * time.Minute,
			JiraPollInterval: 5 * time.Minute, GitHubCloneProtocol: "ssh",
		}, version-1)
		if !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Fatalf("stale UpdateSettingsVersioned err = %v, want ErrOrgSettingsVersion", err)
		}
		if !reflect.DeepEqual(zero, domain.OrgSettings{}) {
			t.Errorf("refused UpdateSettingsVersioned returned %+v, want the zero value", zero)
		}
		after, rerr := read()
		if rerr != nil {
			t.Fatalf("read after refused write: %v", rerr)
		}
		if after.GitHubBaseURL != "https://vret-2.example.com" {
			t.Errorf("refused write changed the row: %+v", after)
		}
	})

	t.Run("UpdateSettings_returns_the_stored_row", func(t *testing.T) {
		saved, err := store.UpdateSettings(ctx, orgID, domain.OrgSettings{
			GitHubBaseURL: "https://uret.example.com", GitHubPollInterval: 6 * time.Minute,
			JiraPollInterval: 6 * time.Minute, GitHubCloneProtocol: "ssh",
		})
		if err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateSettings", saved, read)
		if saved.GitHubBaseURL != "https://uret.example.com" {
			t.Errorf("UpdateSettings returned %+v, want the new URL", saved)
		}
	})

	t.Run("SetLinearWorkspace_returns_the_stored_row", func(t *testing.T) {
		got, err := store.SetLinearWorkspace(ctx, orgID, "ret-workspace", "ret")
		if err != nil {
			t.Fatalf("SetLinearWorkspace: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetLinearWorkspace", got, read)
		if got.LinearWorkspaceID != "ret-workspace" || got.LinearWorkspaceURLKey != "ret" {
			t.Errorf("SetLinearWorkspace returned (%q, %q), want (ret-workspace, ret)", got.LinearWorkspaceID, got.LinearWorkspaceURLKey)
		}
	})

	t.Run("SetGitHubCredentialClass_returns_the_stored_row", func(t *testing.T) {
		got, err := store.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("SetGitHubCredentialClass: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetGitHubCredentialClass", got, read)
		if got.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
			t.Errorf("SetGitHubCredentialClass returned class %q, want %q", got.GitHubCredentialClass, domain.GitHubCredentialClassBYOApp)
		}
	})

	t.Run("SetSourceBaseURL_returns_the_stored_row", func(t *testing.T) {
		before, err := read()
		if err != nil {
			t.Fatalf("read before SetSourceBaseURL: %v", err)
		}
		got, err := store.SetSourceBaseURL(ctx, orgID, "jira", "https://sret.example.com")
		if err != nil {
			t.Fatalf("SetSourceBaseURL: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetSourceBaseURL", got, read)
		if got.JiraBaseURL != "https://sret.example.com" || got.Version != before.Version+1 {
			t.Errorf("SetSourceBaseURL returned host %q version %d, want the new host and version %d", got.JiraBaseURL, got.Version, before.Version+1)
		}

		// The same host again writes nothing; what it hands back is still the
		// stored row.
		same, err := store.SetSourceBaseURL(ctx, orgID, "jira", "https://sret.example.com")
		if err != nil {
			t.Fatalf("SetSourceBaseURL (same host): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetSourceBaseURL (same host)", same, read)
		if same.Version != got.Version {
			t.Errorf("same-host SetSourceBaseURL moved the version %d -> %d", got.Version, same.Version)
		}

		cleared, err := store.SetSourceBaseURL(ctx, orgID, "jira", "")
		if err != nil {
			t.Fatalf("SetSourceBaseURL (clear): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetSourceBaseURL (clear)", cleared, read)
		if cleared.JiraBaseURL != "" {
			t.Errorf("SetSourceBaseURL (clear) returned host %q, want it cleared", cleared.JiraBaseURL)
		}
	})

	for _, c := range []struct {
		name  string
		write func(db.OrgsStore, context.Context, string, string) (domain.OrgSettings, error)
		ref   func(domain.OrgSettings) string
	}{
		{"SetAnthropicKeyRef", db.OrgsStore.SetAnthropicKeyRef,
			func(o domain.OrgSettings) string { return o.AnthropicAPIKeyRef }},
		{"SetBedrockCredentialsRef", db.OrgsStore.SetBedrockCredentialsRef,
			func(o domain.OrgSettings) string { return o.BedrockCredentialsRef }},
	} {
		t.Run(c.name+"_returns_the_stored_row", func(t *testing.T) {
			before, err := read()
			if err != nil {
				t.Fatalf("read before %s: %v", c.name, err)
			}
			bound, err := c.write(store, ctx, orgID, "ret-ref")
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			AssertWriteReturnedStoredRow(t, c.name, bound, read)
			if c.ref(bound) != "ret-ref" || bound.LLMAuthMethod != domain.LLMAuthBYOK || bound.Version != before.Version+1 {
				t.Errorf("%s returned ref %q method %q version %d, want ret-ref, %q, %d",
					c.name, c.ref(bound), bound.LLMAuthMethod, bound.Version, domain.LLMAuthBYOK, before.Version+1)
			}

			// An unchanged ref writes nothing; what it hands back is still the
			// stored row.
			same, err := c.write(store, ctx, orgID, "ret-ref")
			if err != nil {
				t.Fatalf("%s (unchanged): %v", c.name, err)
			}
			AssertWriteReturnedStoredRow(t, c.name+" (unchanged)", same, read)
			if same.Version != bound.Version {
				t.Errorf("unchanged %s moved the version %d -> %d", c.name, bound.Version, same.Version)
			}

			cleared, err := c.write(store, ctx, orgID, "")
			if err != nil {
				t.Fatalf("%s (clear): %v", c.name, err)
			}
			AssertWriteReturnedStoredRow(t, c.name+" (clear)", cleared, read)
			if c.ref(cleared) != "" || cleared.Version != bound.Version+1 {
				t.Errorf("%s (clear) returned ref %q version %d, want it cleared at %d", c.name, c.ref(cleared), cleared.Version, bound.Version+1)
			}

			again, err := c.write(store, ctx, orgID, "")
			if err != nil {
				t.Fatalf("%s (clear again): %v", c.name, err)
			}
			AssertWriteReturnedStoredRow(t, c.name+" (clear again)", again, read)
		})
	}
}
