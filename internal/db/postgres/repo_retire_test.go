package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRepoRetire_Postgres_AdminPoolAndTeamAdminClaims runs the registration
// that a retire unblocks on the pools production uses: the registration under
// a team admin's claims with RLS live, the retire on the admin pool with no
// claims at all, and the reads after it back under the team admin's claims.
func TestRepoRetire_Postgres_AdminPoolAndTeamAdminClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	asAlice := func(fn func(tx db.TxStores) error) error {
		return stores.Tx.WithTx(ctx, orgA, alice, fn)
	}
	ref := func(id string) domain.RepoRef {
		return domain.RepoRef{Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "api", ExternalID: id}
	}

	// alice registers and tracks acme/api under GitHub id 801, and records a
	// pull request in it.
	var old domain.Repository
	if err := asAlice(func(tx db.TxStores) error {
		var e error
		if old, e = tx.Repos.FindOrCreateByExternalID(ctx, orgA, ref("801")); e != nil {
			return e
		}
		return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, teamA, dbtest.TestGitHubHost, []string{old.ID})
	}); err != nil {
		t.Fatalf("register and track acme/api: %v", err)
	}
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgA, "github", dbtest.TestGitHubHost, "acme/api#3", "", "pr", "Old #3", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	// GitHub now serves acme/api under 802; the registration is refused while
	// the old row holds the name.
	if err := asAlice(func(tx db.TxStores) error {
		_, e := tx.Repos.FindOrCreateByExternalID(ctx, orgA, ref("802"))
		return e
	}); !errors.Is(err, db.ErrRepoSlugOccupied) {
		t.Fatalf("register 802 before the retire: err = %v, want ErrRepoSlugOccupied", err)
	}

	// The retire runs on the admin pool, claims-free, and closes the entity.
	retired, err := stores.Repos.RetireSystem(ctx, orgA, ref("801"))
	if err != nil || retired == nil || retired.ID != old.ID || retired.RetiredAt == nil {
		t.Fatalf("RetireSystem = %+v, err=%v; want the old row retired", retired, err)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "RetireSystem", *retired, func() (*domain.Repository, error) {
		return stores.Repos.GetSystem(ctx, orgA, old.ID)
	})
	if ent, _ := stores.Entities.GetSystem(ctx, orgA, entity.ID); ent == nil || ent.State != "closed" {
		t.Errorf("entity = %+v, want it closed", ent)
	}

	// Back under alice's claims: the registration succeeds as a new row, and
	// the row it returns is the row a read under the same claims sees.
	var registered domain.Repository
	if err := asAlice(func(tx db.TxStores) error {
		var e error
		registered, e = tx.Repos.FindOrCreateByExternalID(ctx, orgA, ref("802"))
		return e
	}); err != nil {
		t.Fatalf("register 802 after the retire: %v", err)
	}
	if registered.ID == old.ID || registered.RetiredAt != nil {
		t.Fatalf("registered = %+v, want a new live row", registered)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "FindOrCreateByExternalID", registered, func() (*domain.Repository, error) {
		var got *domain.Repository
		err := asAlice(func(tx db.TxStores) error {
			var e error
			got, e = tx.Repos.Get(ctx, orgA, registered.ID)
			return e
		})
		return got, err
	})

	if err := asAlice(func(tx db.TxStores) error {
		byName, e := tx.Repos.GetByRef(ctx, orgA, ref(""))
		if e != nil {
			return e
		}
		if byName == nil || byName.ID != registered.ID {
			t.Errorf("acme/api by name = %+v, want the new row", byName)
		}
		byID, e := tx.Repos.Get(ctx, orgA, old.ID)
		if e != nil {
			return e
		}
		if byID.RetiredAt == nil {
			t.Errorf("old row by id = %+v, want it retired", byID)
		}
		set, e := tx.TeamGitHubRepos.ListForTeam(ctx, teamA, dbtest.TestGitHubHost)
		if e != nil {
			return e
		}
		if len(set) != 1 || set[0].RepositoryID != old.ID || set[0].RetiredAt == nil {
			t.Errorf("teamA's tracked set = %+v, want the retired row, marked", set)
		}
		return nil
	}); err != nil {
		t.Fatalf("reads under alice's claims: %v", err)
	}
}
