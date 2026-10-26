package dbtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgCommitSignerStoreFactory hands the conformance suite a wired store over
// storage no earlier subtest wrote to, the org every call is scoped to, and a
// user in that org to stamp as the signer's creator. Both rows must exist:
// org_commit_signers carries foreign keys to orgs and users.
type OrgCommitSignerStoreFactory func(t *testing.T) (store db.OrgCommitSignerStore, orgID, userID string)

const (
	signerKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	signerKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	signerKeyC = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICcccccccccccccccccccccccccccccccccccccccccc"
)

// RunOrgCommitSignerStoreConformance pins the signer row's lifecycle: a
// registration starts pending whatever the caller passed, an org has at most
// one signer, a rotation's key is promoted only once it is the key found on
// the account, and every write hands back the row it stored.
func RunOrgCommitSignerStoreConformance(t *testing.T, mk OrgCommitSignerStoreFactory) {
	t.Helper()
	ctx := context.Background()

	newSigner := func(orgID, userID string) domain.OrgCommitSigner {
		return domain.OrgCommitSigner{
			OrgID:        orgID,
			Host:         "https://GitHub.com/",
			GitHubUserID: 583231,
			Login:        "octocat",
			Name:         "The Octocat",
			PublicKey:    signerKeyA,
			CreatedBy:    userID,
		}
	}

	t.Run("an_org_without_a_signer_reads_as_absent", func(t *testing.T) {
		store, orgID, _ := mk(t)
		for name, get := range map[string]func(context.Context, string) (*domain.OrgCommitSigner, error){
			"Get": store.Get, "GetSystem": store.GetSystem,
		} {
			got, err := get(ctx, orgID)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got != nil {
				t.Errorf("%s = %+v, want nil", name, got)
			}
		}
	})

	t.Run("create_stores_a_pending_signer_whatever_the_caller_passed", func(t *testing.T) {
		store, orgID, userID := mk(t)
		in := newSigner(orgID, userID)
		// None of these belong to a new signer, so none may be stored.
		in.Status = domain.CommitSignerActive
		in.StatusReason = "valid"
		in.PendingPublicKey = signerKeyB
		before := time.Now().UTC().Add(-time.Second)

		created, err := store.Create(ctx, in)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Create", created, func() (*domain.OrgCommitSigner, error) {
			return store.Get(ctx, orgID)
		})
		if created.Host != "https://github.com" {
			t.Errorf("host = %q, want the canonical https://github.com", created.Host)
		}
		if created.GitHubUserID != 583231 || created.Login != "octocat" || created.Name != "The Octocat" ||
			created.PublicKey != signerKeyA || created.CreatedBy != userID || created.EmailOverride != "" {
			t.Errorf("Create stored %+v, want the account, key and creator it was given", created)
		}
		if created.Status != domain.CommitSignerPending || created.StatusReason != "" || created.PendingPublicKey != "" {
			t.Errorf("Create stored status %q reason %q pending %q, want a pending signer with no reason or rotation",
				created.Status, created.StatusReason, created.PendingPublicKey)
		}
		if created.CheckedAt != nil {
			t.Errorf("checked_at = %v, want nil: no check has run", created.CheckedAt)
		}
		if created.StatusSince.Before(before) || created.CreatedAt.Before(before) {
			t.Errorf("status_since %s / created_at %s, want stamps from this write (after %s)",
				created.StatusSince, created.CreatedAt, before)
		}
		system, err := store.GetSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("GetSystem: %v", err)
		}
		if system == nil || !reflect.DeepEqual(*system, created) {
			t.Errorf("GetSystem = %+v, want the row Get reads", system)
		}
	})

	t.Run("an_org_has_one_signer", func(t *testing.T) {
		store, orgID, userID := mk(t)
		first, err := store.Create(ctx, newSigner(orgID, userID))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		second := newSigner(orgID, userID)
		second.Login, second.GitHubUserID, second.PublicKey = "hubot", 1, signerKeyB
		if _, err := store.Create(ctx, second); !errors.Is(err, db.ErrOrgCommitSignerExists) {
			t.Fatalf("second Create = %v, want ErrOrgCommitSignerExists", err)
		}
		got, err := store.Get(ctx, orgID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil || !reflect.DeepEqual(*got, first) {
			t.Errorf("after a refused second Create the row is %+v, want the first one unchanged", got)
		}
	})

	t.Run("create_refuses_a_signer_with_no_account_or_key", func(t *testing.T) {
		store, orgID, userID := mk(t)
		for name, mutate := range map[string]func(*domain.OrgCommitSigner){
			"no host":       func(s *domain.OrgCommitSigner) { s.Host = "" },
			"no account id": func(s *domain.OrgCommitSigner) { s.GitHubUserID = 0 },
			"no login":      func(s *domain.OrgCommitSigner) { s.Login = "" },
			"no key":        func(s *domain.OrgCommitSigner) { s.PublicKey = "" },
		} {
			in := newSigner(orgID, userID)
			mutate(&in)
			if _, err := store.Create(ctx, in); !errors.Is(err, db.ErrInvalidOrgCommitSigner) {
				t.Errorf("Create with %s = %v, want ErrInvalidOrgCommitSigner", name, err)
			}
		}
		got, err := store.Get(ctx, orgID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("a refused Create stored %+v", got)
		}
	})

	t.Run("the_email_override_is_set_and_cleared", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if _, err := store.SetEmailOverride(ctx, orgID, "bot@example.com"); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Fatalf("SetEmailOverride with no signer = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		read := func() (*domain.OrgCommitSigner, error) { return store.Get(ctx, orgID) }

		set, err := store.SetEmailOverride(ctx, orgID, "bot@example.com")
		if err != nil {
			t.Fatalf("SetEmailOverride: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetEmailOverride (set)", set, read)
		if set.EmailOverride != "bot@example.com" {
			t.Errorf("email_override = %q, want bot@example.com", set.EmailOverride)
		}

		cleared, err := store.SetEmailOverride(ctx, orgID, "")
		if err != nil {
			t.Fatalf("SetEmailOverride(\"\"): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetEmailOverride (clear)", cleared, read)
		if cleared.EmailOverride != "" {
			t.Errorf("email_override = %q after a clear, want none", cleared.EmailOverride)
		}
	})

	t.Run("a_rotation_promotes_its_key_only_once_found", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if _, err := store.StagePendingKey(ctx, orgID, signerKeyB); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Fatalf("StagePendingKey with no signer = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		read := func() (*domain.OrgCommitSigner, error) { return store.Get(ctx, orgID) }

		active, err := store.Activate(ctx, orgID, signerKeyA)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Activate (current key)", active, read)

		staged, err := store.StagePendingKey(ctx, orgID, signerKeyB)
		if err != nil {
			t.Fatalf("StagePendingKey: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "StagePendingKey", staged, read)
		if staged.PublicKey != signerKeyA || staged.PendingPublicKey != signerKeyB || staged.Status != domain.CommitSignerActive {
			t.Errorf("after StagePendingKey: key %q pending %q status %q, want the current key still signing beside the pending one",
				staged.PublicKey, staged.PendingPublicKey, staged.Status)
		}

		// Finding the current key again changes nothing about the rotation.
		again, err := store.Activate(ctx, orgID, signerKeyA)
		if err != nil {
			t.Fatalf("Activate (current key, mid-rotation): %v", err)
		}
		if again.PublicKey != signerKeyA || again.PendingPublicKey != signerKeyB {
			t.Errorf("Activate on the current key mid-rotation: key %q pending %q, want the rotation left pending",
				again.PublicKey, again.PendingPublicKey)
		}

		// A second rotation before the verify replaces the pending key, so a
		// verify that found the first one is too late and must not activate
		// a key nobody has checked.
		if _, err := store.StagePendingKey(ctx, orgID, signerKeyC); err != nil {
			t.Fatalf("StagePendingKey (second rotation): %v", err)
		}
		if _, err := store.Activate(ctx, orgID, signerKeyB); !errors.Is(err, db.ErrCommitSignerKeyChanged) {
			t.Fatalf("Activate with the replaced pending key = %v, want ErrCommitSignerKeyChanged", err)
		}
		if got, err := read(); err != nil || got == nil || got.PublicKey != signerKeyA || got.PendingPublicKey != signerKeyC {
			t.Fatalf("after a refused Activate the signer reads %+v (%v), want key A signing and key C pending", got, err)
		}

		promoted, err := store.Activate(ctx, orgID, signerKeyC)
		if err != nil {
			t.Fatalf("Activate (pending key): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Activate (pending key)", promoted, read)
		if promoted.PublicKey != signerKeyC || promoted.PendingPublicKey != "" {
			t.Errorf("Activate on the pending key: key %q pending %q, want the pending key promoted and the rotation over",
				promoted.PublicKey, promoted.PendingPublicKey)
		}
	})

	t.Run("activate_moves_status_since_only_on_a_change", func(t *testing.T) {
		store, orgID, userID := mk(t)
		created, err := store.Create(ctx, newSigner(orgID, userID))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
		first, err := store.Activate(ctx, orgID, signerKeyA)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if first.Status != domain.CommitSignerActive || first.StatusReason != "" {
			t.Errorf("Activate stored status %q reason %q, want active with no reason", first.Status, first.StatusReason)
		}
		if !first.StatusSince.After(created.StatusSince) {
			t.Errorf("status_since = %s, want it moved past the pending stamp %s", first.StatusSince, created.StatusSince)
		}
		if first.CheckedAt == nil {
			t.Fatal("checked_at = nil after Activate, want the verify stamped")
		}
		time.Sleep(5 * time.Millisecond)
		second, err := store.Activate(ctx, orgID, signerKeyA)
		if err != nil {
			t.Fatalf("Activate again: %v", err)
		}
		if !second.StatusSince.Equal(first.StatusSince) {
			t.Errorf("status_since = %s on an active signer verified again, want it kept at %s",
				second.StatusSince, first.StatusSince)
		}
		if second.CheckedAt == nil || !second.CheckedAt.After(*first.CheckedAt) {
			t.Errorf("checked_at = %v, want a new stamp after %s", second.CheckedAt, first.CheckedAt)
		}
	})

	t.Run("activate_with_no_signer_is_a_miss", func(t *testing.T) {
		store, orgID, _ := mk(t)
		if _, err := store.Activate(ctx, orgID, signerKeyA); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Fatalf("Activate with no signer = %v, want ErrNoSuchOrgCommitSigner", err)
		}
	})

	t.Run("delete_removes_the_signer", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if err := store.Delete(ctx, orgID); !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			t.Fatalf("Delete with no signer = %v, want ErrNoSuchOrgCommitSigner", err)
		}
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Delete(ctx, orgID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err := store.Get(ctx, orgID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("Get after Delete = %+v, want nil", got)
		}
		// The org can register again once the old signer is gone.
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create after Delete: %v", err)
		}
	})
}

// CommitSignatureStoreFactory hands the conformance suite a wired store over
// storage no earlier subtest wrote to, the org every call is scoped to, and a
// seeder for the engagements a signature belongs to.
type CommitSignatureStoreFactory func(t *testing.T) (store db.CommitSignatureStore, orgID string, seed CommitSignatureSeeder)

// CommitSignatureSeeder stages the rows the store does not own.
type CommitSignatureSeeder struct {
	// Claim inserts a conversation and a claim on it and returns both ids.
	Claim func(t *testing.T) (conversationID, claimID string)

	// DeleteConversation purges a conversation, which takes its claims with
	// it.
	DeleteConversation func(t *testing.T, conversationID string)

	// SetVerification records reason as GitHub's verdict on signature id's
	// pushed commit, the column the post-push check writes.
	SetVerification func(t *testing.T, id, reason string)
}

// RunCommitSignatureStoreConformance pins the signing log: a row enters as a
// request or as a signature already made, a commit id is written once, the
// log outlives the run that asked, and the two budget counts count exactly
// the signatures made.
func RunCommitSignatureStoreConformance(t *testing.T, mk CommitSignatureStoreFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		digest    = "1f40fc92da241694750979ee6cf582f2d5d7d28e18335de05abc54d0560e0f5302860c652bf08d560252aa5e74210546f369fbbbce8c12cfc7957b2652fe9a75"
		armored   = "-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----"
		commitSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	)
	signed := func(orgID, conversationID, claimID string, at time.Time) domain.CommitSignature {
		at = at.UTC().Truncate(time.Millisecond)
		return domain.CommitSignature{
			OrgID:          orgID,
			ConversationID: conversationID,
			ClaimID:        claimID,
			BootEpoch:      3,
			Signer:         domain.CommitSigningOrg,
			SignerHost:     "https://github.com/",
			SignerUserID:   583231,
			PublicKey:      signerKeyA,
			Digest:         digest,
			State:          domain.CommitSignatureSigned,
			Signature:      armored,
			SignedAt:       &at,
		}
	}
	requested := func(orgID, conversationID, claimID string) domain.CommitSignature {
		return domain.CommitSignature{
			OrgID:          orgID,
			ConversationID: conversationID,
			ClaimID:        claimID,
			BootEpoch:      3,
			Signer:         domain.CommitSigningOrg,
			SignerHost:     "https://github.com",
			SignerUserID:   583231,
			Digest:         digest,
			State:          domain.CommitSignatureRequested,
		}
	}

	t.Run("a_signature_already_made_is_stored_as_given", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		in := signed(orgID, conversationID, claimID, time.Now())
		in.CommitSHA = commitSHA
		// Written by the queue and the push, never by an insert.
		in.RefusalReason, in.ClaimedBy, in.VerificationReason = "claim_budget", "pod-1", "valid"
		before := time.Now().UTC().Add(-time.Second)

		got, err := store.InsertSystem(ctx, in)
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "InsertSystem (signed)", got, func() (*domain.CommitSignature, error) {
			return store.GetSystem(ctx, orgID, got.ID)
		})
		if got.ID == "" {
			t.Error("InsertSystem minted no id")
		}
		if got.SignerHost != "https://github.com" {
			t.Errorf("signer_host = %q, want the canonical https://github.com", got.SignerHost)
		}
		if got.State != domain.CommitSignatureSigned || got.Signature != armored || got.PublicKey != signerKeyA ||
			got.SignedAt == nil || !got.SignedAt.Equal(*in.SignedAt) || got.CommitSHA != commitSHA {
			t.Errorf("InsertSystem stored %+v, want the signature, its key, its time and its commit id", got)
		}
		if got.ConversationID != conversationID || got.ClaimID != claimID || got.BootEpoch != 3 ||
			got.Signer != domain.CommitSigningOrg || got.SignerUserID != 583231 || got.Digest != digest {
			t.Errorf("InsertSystem stored %+v, want the run, the engagement, the account and the digest", got)
		}
		if got.RefusalReason != "" || got.ClaimedBy != "" || got.VerificationReason != "" || got.RepositoryID != "" || got.PushedAt != nil {
			t.Errorf("InsertSystem stored %+v, want no refusal, claiming pod, push or verification on a new row", got)
		}
		if got.CreatedAt.Before(before) {
			t.Errorf("created_at = %s, want a stamp from this write (after %s)", got.CreatedAt, before)
		}
	})

	t.Run("a_request_is_stored_with_nothing_signed", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		got, err := store.InsertSystem(ctx, requested(orgID, conversationID, claimID))
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "InsertSystem (requested)", got, func() (*domain.CommitSignature, error) {
			return store.GetSystem(ctx, orgID, got.ID)
		})
		if got.State != domain.CommitSignatureRequested || got.PublicKey != "" || got.Signature != "" || got.SignedAt != nil {
			t.Errorf("InsertSystem stored %+v, want a request with no key, signature or signing time", got)
		}
	})

	t.Run("insert_refuses_a_row_that_is_no_signature", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		now := time.Now()
		for name, sig := range map[string]domain.CommitSignature{
			"a request with a signature": func() domain.CommitSignature {
				s := requested(orgID, conversationID, claimID)
				s.Signature = armored
				return s
			}(),
			"a request with a commit id": func() domain.CommitSignature {
				s := requested(orgID, conversationID, claimID)
				s.CommitSHA = commitSHA
				return s
			}(),
			"a signature with no signature": func() domain.CommitSignature {
				s := signed(orgID, conversationID, claimID, now)
				s.Signature = ""
				return s
			}(),
			"a signature with no time": func() domain.CommitSignature {
				s := signed(orgID, conversationID, claimID, now)
				s.SignedAt = nil
				return s
			}(),
			"a row in a queue state": func() domain.CommitSignature {
				s := requested(orgID, conversationID, claimID)
				s.State = domain.CommitSignatureSigning
				return s
			}(),
			"no signer": func() domain.CommitSignature {
				s := requested(orgID, conversationID, claimID)
				s.Signer = domain.CommitSigningNone
				return s
			}(),
			"no claim": requested(orgID, conversationID, ""),
			"no digest": func() domain.CommitSignature {
				s := requested(orgID, conversationID, claimID)
				s.Digest = ""
				return s
			}(),
			"no account": func() domain.CommitSignature {
				s := requested(orgID, conversationID, claimID)
				s.SignerUserID = 0
				return s
			}(),
		} {
			if _, err := store.InsertSystem(ctx, sig); !errors.Is(err, db.ErrInvalidCommitSignature) {
				t.Errorf("InsertSystem with %s = %v, want ErrInvalidCommitSignature", name, err)
			}
		}
		if n, err := store.ClaimSignaturesSystem(ctx, orgID, claimID); err != nil || n != 0 {
			t.Errorf("ClaimSignaturesSystem after refused inserts = %d, %v; want 0", n, err)
		}
	})

	t.Run("an_unknown_id_reads_as_absent", func(t *testing.T) {
		store, orgID, _ := mk(t)
		got, err := store.GetSystem(ctx, orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("GetSystem: %v", err)
		}
		if got != nil {
			t.Errorf("GetSystem(unknown) = %+v, want nil", got)
		}
	})

	t.Run("a_commit_id_is_written_once", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		row, err := store.InsertSystem(ctx, signed(orgID, conversationID, claimID, time.Now()))
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}
		read := func() (*domain.CommitSignature, error) { return store.GetSystem(ctx, orgID, row.ID) }

		set, err := store.SetCommitSHASystem(ctx, orgID, row.ID, commitSHA)
		if err != nil {
			t.Fatalf("SetCommitSHASystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetCommitSHASystem", set, read)
		if set.CommitSHA != commitSHA {
			t.Errorf("commit_sha = %q, want %q", set.CommitSHA, commitSHA)
		}
		// A resent relay record carries the same id and is a no-op.
		resent, err := store.SetCommitSHASystem(ctx, orgID, row.ID, commitSHA)
		if err != nil {
			t.Fatalf("SetCommitSHASystem (resent): %v", err)
		}
		if !reflect.DeepEqual(resent, set) {
			t.Errorf("a resent commit id changed the row: %+v, want %+v", resent, set)
		}
		if _, err := store.SetCommitSHASystem(ctx, orgID, row.ID, "0000000000000000000000000000000000000000"); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SetCommitSHASystem with a different id = %v, want ErrNoSuchCommitSignature", err)
		}
		if got, err := read(); err != nil || got == nil || got.CommitSHA != commitSHA {
			t.Errorf("after a refused second id the row reads %+v (%v), want commit_sha %q", got, err, commitSHA)
		}

		request, err := store.InsertSystem(ctx, requested(orgID, conversationID, claimID))
		if err != nil {
			t.Fatalf("InsertSystem (requested): %v", err)
		}
		if _, err := store.SetCommitSHASystem(ctx, orgID, request.ID, commitSHA); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SetCommitSHASystem on a request = %v, want ErrNoSuchCommitSignature: nothing was signed", err)
		}
		if _, err := store.SetCommitSHASystem(ctx, orgID, uuid.New().String(), commitSHA); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SetCommitSHASystem on an unknown id = %v, want ErrNoSuchCommitSignature", err)
		}
		fresh, err := store.InsertSystem(ctx, signed(orgID, conversationID, claimID, time.Now()))
		if err != nil {
			t.Fatalf("InsertSystem (fresh): %v", err)
		}
		if _, err := store.SetCommitSHASystem(ctx, orgID, fresh.ID, ""); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SetCommitSHASystem with no commit id = %v, want ErrNoSuchCommitSignature", err)
		}
	})

	t.Run("the_org_window_counts_the_signatures_of_the_last_minute", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		now := time.Now().UTC().Truncate(time.Millisecond)
		since := now.Add(-time.Minute)

		empty, err := store.OrgSignaturesSinceSystem(ctx, orgID, since)
		if err != nil {
			t.Fatalf("OrgSignaturesSinceSystem (empty): %v", err)
		}
		if empty.Count != 0 || empty.Oldest != nil {
			t.Errorf("an org that signed nothing = %+v, want zero with no oldest", empty)
		}

		for _, age := range []time.Duration{90 * time.Second, 50 * time.Second, 10 * time.Second} {
			if _, err := store.InsertSystem(ctx, signed(orgID, conversationID, claimID, now.Add(-age))); err != nil {
				t.Fatalf("InsertSystem (signed %s ago): %v", age, err)
			}
		}
		// A request is not a signature until a control pod makes one.
		if _, err := store.InsertSystem(ctx, requested(orgID, conversationID, claimID)); err != nil {
			t.Fatalf("InsertSystem (requested): %v", err)
		}

		got, err := store.OrgSignaturesSinceSystem(ctx, orgID, since)
		if err != nil {
			t.Fatalf("OrgSignaturesSinceSystem: %v", err)
		}
		if got.Count != 2 {
			t.Errorf("count = %d, want the 2 signatures made in the last minute", got.Count)
		}
		if want := now.Add(-50 * time.Second); got.Oldest == nil || !got.Oldest.Equal(want) {
			t.Errorf("oldest = %v, want %s: the budget admits another once that one is a minute old", got.Oldest, want)
		}
	})

	t.Run("the_claim_count_counts_one_engagement", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationA, claimA := seed.Claim(t)
		conversationB, claimB := seed.Claim(t)
		now := time.Now()
		for _, sig := range []domain.CommitSignature{
			signed(orgID, conversationA, claimA, now.Add(-2*time.Hour)),
			signed(orgID, conversationA, claimA, now),
			requested(orgID, conversationA, claimA),
			signed(orgID, conversationB, claimB, now),
		} {
			if _, err := store.InsertSystem(ctx, sig); err != nil {
				t.Fatalf("InsertSystem: %v", err)
			}
		}
		for claimID, want := range map[string]int{claimA: 2, claimB: 1, uuid.New().String(): 0} {
			got, err := store.ClaimSignaturesSystem(ctx, orgID, claimID)
			if err != nil {
				t.Fatalf("ClaimSignaturesSystem(%s): %v", claimID, err)
			}
			if got != want {
				t.Errorf("ClaimSignaturesSystem(%s) = %d, want %d", claimID, got, want)
			}
		}
	})

	t.Run("the_newest_failed_verification_is_the_signers_latest_failing_commit", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		now := time.Now()

		none, err := store.NewestFailedVerificationSystem(ctx, orgID, "https://github.com", 583231)
		if err != nil {
			t.Fatalf("NewestFailedVerificationSystem (empty): %v", err)
		}
		if none != nil {
			t.Errorf("an org with no checked commit = %+v, want nil", none)
		}

		insert := func(sig domain.CommitSignature, reason string) domain.CommitSignature {
			t.Helper()
			row, err := store.InsertSystem(ctx, sig)
			if err != nil {
				t.Fatalf("InsertSystem: %v", err)
			}
			if reason != "" {
				seed.SetVerification(t, row.ID, reason)
			}
			return row
		}
		older := insert(signed(orgID, conversationID, claimID, now.Add(-3*time.Hour)), "unknown_key")
		want := insert(signed(orgID, conversationID, claimID, now.Add(-2*time.Hour)), "no_user")
		// Newer, and none of them a failure of this signer's: a commit that
		// verified, one never checked, one made as the deployment's signer and
		// one made as another account.
		insert(signed(orgID, conversationID, claimID, now.Add(-time.Hour)), domain.CommitVerificationValid)
		insert(signed(orgID, conversationID, claimID, now.Add(-time.Hour)), "")
		deployment := signed(orgID, conversationID, claimID, now.Add(-time.Hour))
		deployment.Signer = domain.CommitSigningDeployment
		insert(deployment, "unknown_key")
		otherAccount := signed(orgID, conversationID, claimID, now.Add(-time.Hour))
		otherAccount.SignerUserID = 1
		insert(otherAccount, "unknown_key")

		got, err := store.NewestFailedVerificationSystem(ctx, orgID, "https://GitHub.com/", 583231)
		if err != nil {
			t.Fatalf("NewestFailedVerificationSystem: %v", err)
		}
		if got == nil || got.ID != want.ID || got.VerificationReason != "no_user" {
			t.Errorf("NewestFailedVerificationSystem = %+v, want row %s (no_user), not the older %s", got, want.ID, older.ID)
		}
		if other, err := store.NewestFailedVerificationSystem(ctx, orgID, "https://ghe.example.com", 583231); err != nil || other != nil {
			t.Errorf("the same account id on another host = %+v, %v; want nil", other, err)
		}
	})

	t.Run("the_log_outlives_the_run_that_asked", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		row, err := store.InsertSystem(ctx, signed(orgID, conversationID, claimID, time.Now()))
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}
		seed.DeleteConversation(t, conversationID)

		got, err := store.GetSystem(ctx, orgID, row.ID)
		if err != nil {
			t.Fatalf("GetSystem: %v", err)
		}
		if got == nil {
			t.Fatal("the signature went with its conversation, want it kept as the signing log")
		}
		if got.ConversationID != "" || got.ClaimID != "" {
			t.Errorf("after the purge the row names conversation %q claim %q, want both cleared", got.ConversationID, got.ClaimID)
		}
		window, err := store.OrgSignaturesSinceSystem(ctx, orgID, time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("OrgSignaturesSinceSystem: %v", err)
		}
		if window.Count != 1 {
			t.Errorf("org window after the purge = %d, want the signature still counted", window.Count)
		}
	})
}
