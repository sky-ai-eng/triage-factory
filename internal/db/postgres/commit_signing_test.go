package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

const pgSignerKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func pgSigner(orgID, userID string) domain.OrgCommitSigner {
	return domain.OrgCommitSigner{
		OrgID:        orgID,
		Host:         "https://github.com",
		GitHubUserID: 583231,
		Login:        "octocat",
		PublicKey:    pgSignerKey,
		CreatedBy:    userID,
	}
}

// TestOrgCommitSignerStore_Postgres_Conformance runs the shared contract
// against the Postgres impl. Both pools bind the admin (BYPASSRLS) connection,
// the convention for a store suite; the RLS split is covered below.
func TestOrgCommitSignerStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunOrgCommitSignerStoreConformance(t, func(t *testing.T) (db.OrgCommitSignerStore, string, string) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "signers")
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).OrgCommitSigners, orgID, userID
	})
}

// TestOrgCommitSignerStore_Postgres_OrgScoped pins the isolation the SQLite
// suite structurally cannot: one org's signer is not another's, and two orgs
// may register the same account, each with its own key.
func TestOrgCommitSignerStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "signer-alice")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "signer-bob")
	store := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).OrgCommitSigners

	if _, err := store.Create(t.Context(), pgSigner(orgA, userA)); err != nil {
		t.Fatalf("Create orgA: %v", err)
	}
	if got, err := store.GetSystem(t.Context(), orgB); err != nil || got != nil {
		t.Fatalf("GetSystem orgB = %+v, %v; want nil — orgA's signer is not orgB's", got, err)
	}
	same := pgSigner(orgB, userB)
	same.PublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.Create(t.Context(), same); err != nil {
		t.Fatalf("orgB registering the account orgA uses: %v — nothing is unique on the account", err)
	}
	if err := store.Delete(t.Context(), orgB); err != nil {
		t.Fatalf("Delete orgB: %v", err)
	}
	if got, err := store.GetSystem(t.Context(), orgA); err != nil || got == nil {
		t.Fatalf("GetSystem orgA after orgB's delete = %+v, %v; want orgA's signer untouched", got, err)
	}
}

// TestOrgCommitSignerStore_Postgres_ReturnedRow_AppPool runs every write's
// returned-row contract through the APP pool under an org admin's claims,
// which the conformance suite cannot: on its BYPASSRLS connection a RETURNING
// clause hands back its row unconditionally, while under RLS the row has to
// satisfy the SELECT policy too.
func TestOrgCommitSignerStore_Postgres_ReturnedRow_AppPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, adminID, _ := pgtest.SeedOrgWithUser(t, h, "signer-rls")

	if err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).OrgCommitSigners
		read := func() (*domain.OrgCommitSigner, error) { return store.Get(t.Context(), orgID) }

		created, err := store.Create(t.Context(), pgSigner(orgID, adminID))
		if err != nil {
			t.Fatalf("Create on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "Create", created, read)

		email, err := store.SetEmailOverride(t.Context(), orgID, "bot@example.com")
		if err != nil {
			t.Fatalf("SetEmailOverride on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "SetEmailOverride", email, read)

		staged, err := store.StagePendingKey(t.Context(), orgID, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if err != nil {
			t.Fatalf("StagePendingKey on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "StagePendingKey", staged, read)

		active, err := store.Activate(t.Context(), orgID, staged.PendingPublicKey)
		if err != nil {
			t.Fatalf("Activate on the app pool: %v", err)
		}
		dbtest.AssertWriteReturnedStoredRow(t, "Activate", active, read)

		if err := store.Delete(t.Context(), orgID); err != nil {
			t.Fatalf("Delete on the app pool: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestOrgCommitSignerStore_Postgres_MemberReadsAdminWrites pins the half of the
// gate that lives in the database. The routes' org-admin check is what a caller
// sees; under it, a plain member reads the org's signer — it is who their
// org's commits are authored as — and can neither register, change nor remove
// one.
func TestOrgCommitSignerStore_Postgres_MemberReadsAdminWrites(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, adminID, teamID := pgtest.SeedOrgWithUser(t, h, "signer-gate")
	memberID := pgtest.SeedUser(t, h, "signer-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")
	outsiderOrg, outsiderID, _ := pgtest.SeedOrgWithUser(t, h, "signer-outsider")

	// The refused write is tried first, in a transaction of its own: an RLS
	// denial aborts the transaction it happens in.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).OrgCommitSigners.Create(t.Context(), pgSigner(orgID, memberID))
		return err
	}); err == nil {
		t.Fatal("a plain member registered a signer; the write policy is org-admin only")
	} else {
		pgtest.AssertRLSViolation(t, err)
	}

	if err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).OrgCommitSigners.Create(t.Context(), pgSigner(orgID, adminID))
		return err
	}); err != nil {
		t.Fatalf("org admin Create: %v", err)
	}

	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).OrgCommitSigners
		got, err := store.Get(t.Context(), orgID)
		if err != nil {
			return err
		}
		if got == nil {
			t.Error("a member reads no signer; members must see who the org's commits are authored as")
		}
		// The update and delete policies filter the row out of a member's
		// reach, so the writes find nothing to change.
		if _, err := store.SetEmailOverride(t.Context(), orgID, "member@example.com"); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Errorf("member SetEmailOverride = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		if _, err := store.StagePendingKey(t.Context(), orgID, "ssh-ed25519 AAAAmember"); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Errorf("member StagePendingKey = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		// The key is the signer's own, so the miss is the policy's, not a
		// rotation that moved the keys.
		if _, err := store.Activate(t.Context(), orgID, pgSignerKey); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Errorf("member Activate = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		if err := store.Delete(t.Context(), orgID); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Errorf("member Delete = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("member reads: %v", err)
	}

	got, err := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).OrgCommitSigners.GetSystem(t.Context(), orgID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if got == nil || got.EmailOverride != "" || got.PendingPublicKey != "" || got.Status != domain.CommitSignerPending {
		t.Errorf("after the member's writes the signer reads %+v, want it as the admin left it", got)
	}

	// Another org's admin sees nothing of this org's signer.
	if err := h.WithUser(t, outsiderID, outsiderOrg, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).OrgCommitSigners.Get(t.Context(), orgID)
		if err != nil {
			return err
		}
		if got != nil {
			t.Errorf("another org's admin reads %+v, want nothing", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("outsider read: %v", err)
	}
}

// pgCommitSignatureSeeder stages the task → conversation → claim chain a
// signature belongs to.
func pgCommitSignatureSeeder(t *testing.T, h *pgtest.Harness, orgID, userID string) dbtest.CommitSignatureSeeder {
	t.Helper()
	return dbtest.CommitSignatureSeeder{
		Claim: func(t *testing.T) (string, string) {
			t.Helper()
			conversationID := seedPgSnapshotConversation(t, h.AdminDB, orgID, userID, seedPgTask(t, h, orgID, userID))
			return conversationID, seedPgSnapshotClaim(t, h.AdminDB, orgID, conversationID, time.Now(), false)
		},
		DeleteConversation: func(t *testing.T, conversationID string) {
			t.Helper()
			pgtest.MustExec(t, h.AdminDB, `DELETE FROM conversations WHERE id = $1`, conversationID)
		},
	}
}

// TestCommitSignatureStore_Postgres_Conformance runs the shared signing-log
// contract against the Postgres impl on the admin pool, its production wiring.
func TestCommitSignatureStore_Postgres_Conformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunCommitSignatureStoreConformance(t, func(t *testing.T) (db.CommitSignatureStore, string, dbtest.CommitSignatureSeeder) {
		h.Reset(t)
		orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "signatures")
		return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).CommitSignatures, orgID, pgCommitSignatureSeeder(t, h, orgID, userID)
	})
}

// TestCommitSignatureQueue_Postgres_Conformance runs the control-plane queue's
// contract on the admin pool, the control pods' wiring.
func TestCommitSignatureQueue_Postgres_Conformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunCommitSignatureQueueConformance(t, func(t *testing.T) dbtest.CommitSignatureQueueFixture {
		h.Reset(t)
		orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "signature-queue")
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.CommitSignatureQueueFixture{
			Signatures: stores.CommitSignatures,
			Claims:     stores.ConversationQueue,
			OrgID:      orgID,
			Claim: func(t *testing.T, claimedAt time.Time, released bool) (string, string) {
				t.Helper()
				conversationID := seedPgSnapshotConversation(t, h.AdminDB, orgID, userID, seedPgTask(t, h, orgID, userID))
				return conversationID, seedPgSnapshotClaim(t, h.AdminDB, orgID, conversationID, claimedAt, released)
			},
		}
	})
}

// TestCommitSignatureQueue_Postgres_PodsRaceForOneRequest pins the claim's
// conditional update under real concurrency: pods racing for one request get
// it exactly once.
func TestCommitSignatureQueue_Postgres_PodsRaceForOneRequest(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "signature-race")
	store := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).CommitSignatures
	conversationID, claimID := pgCommitSignatureSeeder(t, h, orgID, userID).Claim(t)
	row, err := store.InsertSystem(t.Context(), domain.CommitSignature{
		OrgID: orgID, ConversationID: conversationID, ClaimID: claimID, BootEpoch: 1,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: 583231,
		Digest: strings.Repeat("ab", 64), State: domain.CommitSignatureRequested,
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}

	const pods = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		takes []string
	)
	start := make(chan struct{})
	for i := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pod := fmt.Sprintf("pod-%d", i)
			got, err := store.ClaimRequestSystem(t.Context(), orgID, row.ID, pod)
			if err != nil {
				t.Errorf("ClaimRequestSystem (%s): %v", pod, err)
				return
			}
			if got != nil {
				mu.Lock()
				takes = append(takes, pod)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(takes) != 1 {
		t.Fatalf("pods that claimed the request = %v, want exactly one", takes)
	}
	stored, err := store.GetSystem(t.Context(), orgID, row.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetSystem: %+v, %v", stored, err)
	}
	if stored.ClaimedBy != takes[0] {
		t.Errorf("claimed_by = %q, want the one pod that took it (%s)", stored.ClaimedBy, takes[0])
	}
}

// TestCommitSignatureStore_Postgres_OrgScoped pins org_id defense in depth on
// the admin pool: one org's signatures are not in another's budget window, and
// another org cannot read or stamp them.
func TestCommitSignatureStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "signatures-alice")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "signatures-bob")
	store := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).CommitSignatures
	conversationID, claimID := pgCommitSignatureSeeder(t, h, orgA, userA).Claim(t)

	row := seedPgSignature(t, store, orgA, conversationID, claimID)
	window, err := store.OrgSignaturesSinceSystem(t.Context(), orgB, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("OrgSignaturesSinceSystem orgB: %v", err)
	}
	if window.Count != 0 {
		t.Errorf("orgB's window counts %d, want 0 — orgA's signatures are not orgB's budget", window.Count)
	}
	if n, err := store.ClaimSignaturesSystem(t.Context(), orgB, claimID); err != nil || n != 0 {
		t.Errorf("ClaimSignaturesSystem under orgB = %d, %v; want 0", n, err)
	}
	if got, err := store.GetSystem(t.Context(), orgB, row.ID); err != nil || got != nil {
		t.Errorf("GetSystem under orgB = %+v, %v; want nil", got, err)
	}
	if _, err := store.SetCommitSHASystem(t.Context(), orgB, row.ID, "4b825dc642cb6eb9a060e54bf8d69288fbee4904"); !errors.Is(err, db.ErrNoSuchCommitSignature) {
		t.Errorf("SetCommitSHASystem under orgB = %v, want ErrNoSuchCommitSignature", err)
	}
}

// TestCommitSignatures_Postgres_MembersReadOnly pins the table's app-pool
// surface: an org member reads the org's signing log, another org's member
// reads none of it, and no request path can write a signature.
func TestCommitSignatures_Postgres_MembersReadOnly(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, adminID, teamID := pgtest.SeedOrgWithUser(t, h, "signatures-rls")
	memberID := pgtest.SeedUser(t, h, "signatures-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")
	outsiderOrg, outsiderID, _ := pgtest.SeedOrgWithUser(t, h, "signatures-outsider")
	conversationID, claimID := pgCommitSignatureSeeder(t, h, orgID, adminID).Claim(t)
	seedPgSignature(t, pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).CommitSignatures, orgID, conversationID, claimID)

	count := func(userID, claimOrg string) int {
		t.Helper()
		var n int
		if err := h.WithUser(t, userID, claimOrg, func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT count(*) FROM commit_signatures`).Scan(&n)
		}); err != nil {
			t.Fatalf("count as %s: %v", userID, err)
		}
		return n
	}
	if n := count(memberID, orgID); n != 1 {
		t.Errorf("a member reads %d signatures, want the org's 1", n)
	}
	if n := count(outsiderID, outsiderOrg); n != 0 {
		t.Errorf("another org's member reads %d signatures, want 0", n)
	}

	err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO commit_signatures (org_id, conversation_id, claim_id, boot_epoch, signer, signer_host,
				signer_user_id, digest, state)
			VALUES ($1, $2, $3, 1, 'org', 'https://github.com', 583231, 'ab', 'requested')`,
			orgID, conversationID, claimID)
		return err
	})
	pgtest.AssertRLSViolation(t, err)
}

func seedPgSignature(t *testing.T, store db.CommitSignatureStore, orgID, conversationID, claimID string) domain.CommitSignature {
	t.Helper()
	now := time.Now().UTC()
	row, err := store.InsertSystem(t.Context(), domain.CommitSignature{
		OrgID:          orgID,
		ConversationID: conversationID,
		ClaimID:        claimID,
		BootEpoch:      1,
		Signer:         domain.CommitSigningOrg,
		SignerHost:     "https://github.com",
		SignerUserID:   583231,
		PublicKey:      pgSignerKey,
		Digest:         uuid.New().String(),
		State:          domain.CommitSignatureSigned,
		Signature:      "-----BEGIN SSH SIGNATURE-----\n-----END SSH SIGNATURE-----",
		SignedAt:       &now,
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}
	return row
}
