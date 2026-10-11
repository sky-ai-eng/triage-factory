package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepositoryUpsertReturnedRowFactory hands RunRepositoryUpsertReturnedRowConformance
// a RepositoryStore and the orgID every call passes. A backend that enforces
// row-level security wires a store bound to a claims-carrying transaction, so
// the upsert's RETURNING and the point read beside it are both what an
// RLS-scoped caller sees.
type RepositoryUpsertReturnedRowFactory func(t *testing.T) (store db.RepositoryStore, orgID string)

// RunRepositoryUpsertReturnedRowConformance covers the returned-row standard on
// Upsert for host-carrying writes: on each arm (insert, update, and the arm
// where the provider id is withheld because another row on the host holds it)
// the write hands back the row it persisted, and that row is the one a point
// read finds. The id rule reads the repositories table inside the statement,
// and the conflict arm returns a row the caller did not insert, so under RLS
// both halves are visibility questions a BYPASSRLS connection never asks.
func RunRepositoryUpsertReturnedRowConformance(t *testing.T, mk RepositoryUpsertReturnedRowFactory) {
	t.Helper()

	t.Run("Upsert_returns_the_stored_row_on_each_host", func(t *testing.T) {
		s, orgID := mk(t)
		ctx := context.Background()
		read := func(id string) func() (*domain.Repository, error) {
			return func() (*domain.Repository, error) { return s.Get(ctx, orgID, id) }
		}
		// ProfiledAt is set so the comparison follows the row's one pointer
		// field rather than comparing two nils.
		profiled := time.Now().UTC().Truncate(time.Second)

		dotcom, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "Acme", Repo: "Api",
			ExternalID: "1296269", ProfileText: "v1", DefaultBranch: "main", ProfiledAt: &profiled,
		})
		if err != nil {
			t.Fatalf("Upsert(github.com, insert arm): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(github.com, insert arm)", dotcom, read(dotcom.ID))
		if dotcom.Host != TestGitHubHost {
			t.Errorf("returned host = %q, want %q", dotcom.Host, TestGitHubHost)
		}

		ghe, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestOtherGitHubHost, Owner: "acme", Repo: "api",
			ExternalID: "1296269", ProfileText: "ghe", DefaultBranch: "main", ProfiledAt: &profiled,
		})
		if err != nil {
			t.Fatalf("Upsert(ghe, insert arm): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(ghe, insert arm)", ghe, read(ghe.ID))
		if ghe.ID == dotcom.ID || ghe.Host != TestOtherGitHubHost || ghe.ExternalID != "1296269" {
			t.Errorf("ghe upsert returned %+v; want its own row on %s carrying the same provider id", ghe, TestOtherGitHubHost)
		}

		reprofiled := profiled.Add(time.Hour)
		updated, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "acme", Repo: "api",
			ProfileText: "v2", DefaultBranch: "main", ProfiledAt: &reprofiled,
		})
		if err != nil {
			t.Fatalf("Upsert(github.com, update arm): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(github.com, update arm)", updated, read(dotcom.ID))
		if updated.ID != dotcom.ID || updated.Slug() != "Acme/Api" || updated.ExternalID != "1296269" {
			t.Errorf("update arm returned %+v; want row %s under its stored casing with its id kept", updated, dotcom.ID)
		}

		claimant, err := s.Upsert(ctx, orgID, domain.Repository{
			Host: TestGitHubHost, Owner: "acme", Repo: "api-renamed",
			ExternalID: "1296269", DefaultBranch: "main", ProfiledAt: &profiled,
		})
		if err != nil {
			t.Fatalf("Upsert(github.com, id held by another row): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert(github.com, id held by another row)", claimant, read(claimant.ID))
		if claimant.ExternalID != "" {
			t.Errorf("claimant returned external id %q; want none — Acme/Api holds it on this host", claimant.ExternalID)
		}
	})
}
