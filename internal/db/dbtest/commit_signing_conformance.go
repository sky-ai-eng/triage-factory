package dbtest

import (
	"context"
	"errors"
	"reflect"
	"slices"
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

	keyCheck := func(found bool) db.SignerKeyCheck {
		return db.SignerKeyCheck{GitHubUserID: 583231, PublicKey: signerKeyA, Login: "octocat", Name: "The Octocat", KeyFound: found}
	}

	t.Run("a_key_check_breaks_an_active_signer_and_restores_it", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if got, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(false)); err != nil || got != nil {
			t.Fatalf("RecordKeyCheckSystem with no signer = %+v, %v; want nil", got, err)
		}
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		active, err := store.Activate(ctx, orgID, signerKeyA)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		read := func() (*domain.OrgCommitSigner, error) { return store.GetSystem(ctx, orgID) }

		time.Sleep(5 * time.Millisecond)
		missing, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(false))
		if err != nil || missing == nil {
			t.Fatalf("RecordKeyCheckSystem (key gone) = %+v, %v", missing, err)
		}
		AssertWriteReturnedStoredRow(t, "RecordKeyCheckSystem (key gone)", *missing, read)
		if missing.Status != domain.CommitSignerBroken || missing.StatusReason != domain.CommitSignerReasonKeyMissing {
			t.Errorf("after the key went: status %q reason %q, want broken for key_missing", missing.Status, missing.StatusReason)
		}
		if !missing.StatusSince.After(active.StatusSince) {
			t.Errorf("status_since = %s, want it moved past %s", missing.StatusSince, active.StatusSince)
		}
		if missing.CheckedAt == nil || !missing.CheckedAt.After(*active.CheckedAt) {
			t.Errorf("checked_at = %v, want a new stamp after %s", missing.CheckedAt, active.CheckedAt)
		}

		time.Sleep(5 * time.Millisecond)
		again, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(false))
		if err != nil || again == nil {
			t.Fatalf("RecordKeyCheckSystem (still gone) = %+v, %v", again, err)
		}
		if again.Status != domain.CommitSignerBroken || !again.StatusSince.Equal(missing.StatusSince) {
			t.Errorf("a second missing check: status %q since %s, want broken since %s", again.Status, again.StatusSince, missing.StatusSince)
		}

		time.Sleep(5 * time.Millisecond)
		back, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(true))
		if err != nil || back == nil {
			t.Fatalf("RecordKeyCheckSystem (key back) = %+v, %v", back, err)
		}
		AssertWriteReturnedStoredRow(t, "RecordKeyCheckSystem (key back)", *back, read)
		if back.Status != domain.CommitSignerActive || back.StatusReason != "" {
			t.Errorf("after the key came back: status %q reason %q, want active with no reason", back.Status, back.StatusReason)
		}
		if !back.StatusSince.After(missing.StatusSince) {
			t.Errorf("status_since = %s, want it moved past %s", back.StatusSince, missing.StatusSince)
		}
	})

	t.Run("a_key_check_refreshes_the_account", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		renamed := keyCheck(true)
		renamed.Login, renamed.Name = "octocat-renamed", ""
		got, err := store.RecordKeyCheckSystem(ctx, orgID, renamed)
		if err != nil || got == nil {
			t.Fatalf("RecordKeyCheckSystem = %+v, %v", got, err)
		}
		if got.Login != "octocat-renamed" || got.Name != "" {
			t.Errorf("login %q name %q, want the account as GitHub answered it, with no profile name", got.Login, got.Name)
		}
		unread := keyCheck(true)
		unread.Login, unread.Name = "", "ignored"
		if got, err = store.RecordKeyCheckSystem(ctx, orgID, unread); err != nil || got == nil {
			t.Fatalf("RecordKeyCheckSystem (account unread) = %+v, %v", got, err)
		}
		if got.Login != "octocat-renamed" || got.Name != "" {
			t.Errorf("an unread account changed login %q name %q, want both kept", got.Login, got.Name)
		}
	})

	t.Run("a_key_check_leaves_other_statuses_and_other_keys", func(t *testing.T) {
		store, orgID, userID := mk(t)
		created, err := store.Create(ctx, newSigner(orgID, userID))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// A pending signer waits for an admin's Verify either way.
		for _, found := range []bool{true, false} {
			got, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(found))
			if err != nil || got == nil {
				t.Fatalf("RecordKeyCheckSystem on a pending signer (found=%v) = %+v, %v", found, got, err)
			}
			if got.Status != domain.CommitSignerPending || !got.StatusSince.Equal(created.StatusSince) {
				t.Errorf("pending signer after a check (found=%v): status %q since %s, want pending since %s",
					found, got.Status, got.StatusSince, created.StatusSince)
			}
		}
		// One a pushed commit broke keeps that reason until a Verify.
		if _, err := store.Activate(ctx, orgID, signerKeyA); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if _, err := store.BreakSystem(ctx, orgID, signerKeyA, "unverified_email"); err != nil {
			t.Fatalf("BreakSystem: %v", err)
		}
		for _, found := range []bool{true, false} {
			got, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(found))
			if err != nil || got == nil {
				t.Fatalf("RecordKeyCheckSystem on a commit-broken signer (found=%v) = %+v, %v", found, got, err)
			}
			if got.Status != domain.CommitSignerBroken || got.StatusReason != "unverified_email" {
				t.Errorf("commit-broken signer after a check (found=%v): status %q reason %q, want broken for unverified_email",
					found, got.Status, got.StatusReason)
			}
		}
		// A check of a key or account the signer no longer has is a miss.
		stale := keyCheck(false)
		stale.PublicKey = signerKeyB
		if got, err := store.RecordKeyCheckSystem(ctx, orgID, stale); err != nil || got != nil {
			t.Errorf("RecordKeyCheckSystem for another key = %+v, %v; want nil", got, err)
		}
		stale = keyCheck(false)
		stale.GitHubUserID = 1
		if got, err := store.RecordKeyCheckSystem(ctx, orgID, stale); err != nil || got != nil {
			t.Errorf("RecordKeyCheckSystem for another account = %+v, %v; want nil", got, err)
		}
	})

	t.Run("a_failing_commit_breaks_an_active_signer_and_the_first_reason_holds", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if got, err := store.BreakSystem(ctx, orgID, signerKeyA, "no_user"); err != nil || got != nil {
			t.Fatalf("BreakSystem with no signer = %+v, %v; want nil", got, err)
		}
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got, err := store.BreakSystem(ctx, orgID, signerKeyA, "no_user"); err != nil || got != nil {
			t.Errorf("BreakSystem on a pending signer = %+v, %v; want nil", got, err)
		}
		active, err := store.Activate(ctx, orgID, signerKeyA)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if got, err := store.BreakSystem(ctx, orgID, signerKeyB, "no_user"); err != nil || got != nil {
			t.Errorf("BreakSystem for another key = %+v, %v; want nil", got, err)
		}
		for _, reason := range []string{"", domain.CommitVerificationValid} {
			if _, err := store.BreakSystem(ctx, orgID, signerKeyA, reason); !errors.Is(err, db.ErrInvalidOrgCommitSigner) {
				t.Errorf("BreakSystem(%q) = %v, want ErrInvalidOrgCommitSigner", reason, err)
			}
		}

		time.Sleep(5 * time.Millisecond)
		broken, err := store.BreakSystem(ctx, orgID, signerKeyA, "unverified_email")
		if err != nil || broken == nil {
			t.Fatalf("BreakSystem = %+v, %v", broken, err)
		}
		AssertWriteReturnedStoredRow(t, "BreakSystem", *broken, func() (*domain.OrgCommitSigner, error) {
			return store.GetSystem(ctx, orgID)
		})
		if broken.Status != domain.CommitSignerBroken || broken.StatusReason != "unverified_email" {
			t.Errorf("after BreakSystem: status %q reason %q, want broken for unverified_email", broken.Status, broken.StatusReason)
		}
		if !broken.StatusSince.After(active.StatusSince) {
			t.Errorf("status_since = %s, want it moved past %s", broken.StatusSince, active.StatusSince)
		}
		if got, err := store.BreakSystem(ctx, orgID, signerKeyA, "no_user"); err != nil || got != nil {
			t.Errorf("a second failure on a broken signer = %+v, %v; want nil, the first reason kept", got, err)
		}
	})

	t.Run("a_failing_commit_replaces_key_missing_unless_it_is_the_missing_key", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := store.Activate(ctx, orgID, signerKeyA); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		missing, err := store.RecordKeyCheckSystem(ctx, orgID, keyCheck(false))
		if err != nil || missing == nil {
			t.Fatalf("RecordKeyCheckSystem = %+v, %v", missing, err)
		}
		if got, err := store.BreakSystem(ctx, orgID, signerKeyA, domain.CommitVerificationUnknownKey); err != nil || got != nil {
			t.Errorf("unknown_key on a signer missing its key = %+v, %v; want nil, key_missing kept", got, err)
		}
		got, err := store.BreakSystem(ctx, orgID, signerKeyA, "no_user")
		if err != nil || got == nil {
			t.Fatalf("BreakSystem (no_user) = %+v, %v", got, err)
		}
		if got.StatusReason != "no_user" || !got.StatusSince.Equal(missing.StatusSince) {
			t.Errorf("after no_user: reason %q since %s, want no_user, broken since %s", got.StatusReason, got.StatusSince, missing.StatusSince)
		}
	})

	t.Run("health_lists_each_signer", func(t *testing.T) {
		store, orgID, userID := mk(t)
		if _, err := store.Create(ctx, newSigner(orgID, userID)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := store.Activate(ctx, orgID, signerKeyA); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if _, err := store.BreakSystem(ctx, orgID, signerKeyA, "no_user"); err != nil {
			t.Fatalf("BreakSystem: %v", err)
		}
		health, err := store.ListHealthSystem(ctx)
		if err != nil {
			t.Fatalf("ListHealthSystem: %v", err)
		}
		want := db.SignerHealth{OrgID: orgID, Status: domain.CommitSignerBroken, Reason: "no_user"}
		if !slices.Contains(health, want) {
			t.Errorf("ListHealthSystem = %+v, want it to hold %+v", health, want)
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

	// Repository inserts a registry row for a push to land in and returns
	// its id.
	Repository func(t *testing.T) string
}

const (
	signatureDigest    = "1f40fc92da241694750979ee6cf582f2d5d7d28e18335de05abc54d0560e0f5302860c652bf08d560252aa5e74210546f369fbbbce8c12cfc7957b2652fe9a75"
	signatureArmored   = "-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----"
	signatureCommitSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// signedSignature is a signature already made at at, as a local daemon
// records one.
func signedSignature(orgID, conversationID, claimID string, at time.Time) domain.CommitSignature {
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
		Digest:         signatureDigest,
		State:          domain.CommitSignatureSigned,
		Signature:      signatureArmored,
		SignedAt:       &at,
	}
}

// requestedSignature is a request no control pod has decided yet.
func requestedSignature(orgID, conversationID, claimID string) domain.CommitSignature {
	return domain.CommitSignature{
		OrgID:          orgID,
		ConversationID: conversationID,
		ClaimID:        claimID,
		BootEpoch:      3,
		Signer:         domain.CommitSigningOrg,
		SignerHost:     "https://github.com",
		SignerUserID:   583231,
		Digest:         signatureDigest,
		State:          domain.CommitSignatureRequested,
	}
}

// RunCommitSignatureStoreConformance pins the signing log: a row enters as a
// request or as a signature already made, a commit id is written once, the
// log outlives the run that asked, and the two budget counts count exactly
// the signatures made.
func RunCommitSignatureStoreConformance(t *testing.T, mk CommitSignatureStoreFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		digest    = signatureDigest
		armored   = signatureArmored
		commitSHA = signatureCommitSHA
	)
	signed, requested := signedSignature, requestedSignature

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

	t.Run("a_commit_finds_the_signature_that_produced_it", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		now := time.Now()

		if none, err := store.ForCommitSystem(ctx, orgID, commitSHA); err != nil || none != nil {
			t.Fatalf("ForCommitSystem with no signature = %+v, %v; want nil", none, err)
		}
		withSHA := func(at time.Time, sha string) domain.CommitSignature {
			t.Helper()
			sig := signed(orgID, conversationID, claimID, at)
			sig.CommitSHA = sha
			row, err := store.InsertSystem(ctx, sig)
			if err != nil {
				t.Fatalf("InsertSystem: %v", err)
			}
			return row
		}
		withSHA(now.Add(-time.Hour), commitSHA)
		want := withSHA(now.Add(-time.Minute), commitSHA)
		withSHA(now, "1111111111111111111111111111111111111111")
		// A request names no commit until it is signed.
		if _, err := store.InsertSystem(ctx, requested(orgID, conversationID, claimID)); err != nil {
			t.Fatalf("InsertSystem (requested): %v", err)
		}

		got, err := store.ForCommitSystem(ctx, orgID, commitSHA)
		if err != nil {
			t.Fatalf("ForCommitSystem: %v", err)
		}
		if got == nil || got.ID != want.ID {
			t.Errorf("ForCommitSystem = %+v, want the newest signature of the commit, %s", got, want.ID)
		}
		if blank, err := store.ForCommitSystem(ctx, orgID, ""); err != nil || blank != nil {
			t.Errorf("ForCommitSystem with no commit id = %+v, %v; want nil", blank, err)
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

	t.Run("a_push_is_stamped_once", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		repositoryID := seed.Repository(t)
		sig := signed(orgID, conversationID, claimID, time.Now())
		sig.CommitSHA = commitSHA
		row, err := store.InsertSystem(ctx, sig)
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}

		if _, err := store.MarkPushedSystem(ctx, orgID, row.ID, ""); !errors.Is(err, db.ErrInvalidCommitSignature) {
			t.Errorf("MarkPushedSystem with no repository = %v, want ErrInvalidCommitSignature", err)
		}
		pushed, err := store.MarkPushedSystem(ctx, orgID, row.ID, repositoryID)
		if err != nil || pushed == nil {
			t.Fatalf("MarkPushedSystem = %+v, %v", pushed, err)
		}
		AssertWriteReturnedStoredRow(t, "MarkPushedSystem", *pushed, func() (*domain.CommitSignature, error) {
			return store.GetSystem(ctx, orgID, row.ID)
		})
		if pushed.RepositoryID != repositoryID || pushed.PushedAt == nil {
			t.Errorf("stamped repository %q pushed_at %v, want %s and a push time", pushed.RepositoryID, pushed.PushedAt, repositoryID)
		}
		if pushed.VerificationReason != "" || pushed.CheckedAt != nil {
			t.Errorf("a push stamped a verdict (%q at %v), want none until the check asks", pushed.VerificationReason, pushed.CheckedAt)
		}

		// The first push is the one kept.
		if again, err := store.MarkPushedSystem(ctx, orgID, row.ID, repositoryID); err != nil || again != nil {
			t.Errorf("a second MarkPushedSystem = %+v, %v; want nil", again, err)
		}
		if got, err := store.GetSystem(ctx, orgID, row.ID); err != nil || got == nil || !got.PushedAt.Equal(*pushed.PushedAt) {
			t.Errorf("after a second push the row is %+v, %v; want the first push kept", got, err)
		}
		// A request was never signed, and an unknown id is no signature.
		request, err := store.InsertSystem(ctx, requested(orgID, conversationID, claimID))
		if err != nil {
			t.Fatalf("InsertSystem (requested): %v", err)
		}
		if got, err := store.MarkPushedSystem(ctx, orgID, request.ID, repositoryID); err != nil || got != nil {
			t.Errorf("MarkPushedSystem on a request = %+v, %v; want nil", got, err)
		}
		if got, err := store.MarkPushedSystem(ctx, orgID, uuid.NewString(), repositoryID); err != nil || got != nil {
			t.Errorf("MarkPushedSystem on an unknown id = %+v, %v; want nil", got, err)
		}
	})

	t.Run("unchecked_pushes_are_the_newest_not_yet_asked_about", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		repositoryID := seed.Repository(t)
		insert := func(mode domain.CommitSigningMode, push bool) domain.CommitSignature {
			t.Helper()
			sig := signed(orgID, conversationID, claimID, time.Now())
			sig.Signer = mode
			row, err := store.InsertSystem(ctx, sig)
			if err != nil {
				t.Fatalf("InsertSystem: %v", err)
			}
			if push {
				time.Sleep(5 * time.Millisecond)
				if _, err := store.MarkPushedSystem(ctx, orgID, row.ID, repositoryID); err != nil {
					t.Fatalf("MarkPushedSystem: %v", err)
				}
			}
			return row
		}
		older := insert(domain.CommitSigningOrg, true)
		newer := insert(domain.CommitSigningOrg, true)
		insert(domain.CommitSigningOrg, false)
		checked := insert(domain.CommitSigningOrg, true)
		seed.SetVerification(t, checked.ID, domain.CommitVerificationValid)
		deployment := insert(domain.CommitSigningDeployment, true)

		ids := func(rows []domain.CommitSignature) []string {
			out := make([]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, r.ID)
			}
			return out
		}
		hourAgo := time.Now().Add(-time.Hour)
		got, err := store.UncheckedPushesSystem(ctx, orgID, domain.CommitSigningOrg, hourAgo, 10)
		if err != nil {
			t.Fatalf("UncheckedPushesSystem: %v", err)
		}
		if want := []string{newer.ID, older.ID}; !slices.Equal(ids(got), want) {
			t.Errorf("UncheckedPushesSystem = %v, want the two unchecked pushes newest first %v", ids(got), want)
		}
		if got, err = store.UncheckedPushesSystem(ctx, orgID, domain.CommitSigningOrg, hourAgo, 1); err != nil ||
			!slices.Equal(ids(got), []string{newer.ID}) {
			t.Errorf("UncheckedPushesSystem limit 1 = %v, %v; want only the newest", ids(got), err)
		}
		if got, err = store.UncheckedPushesSystem(ctx, orgID, domain.CommitSigningDeployment, hourAgo, 10); err != nil ||
			!slices.Equal(ids(got), []string{deployment.ID}) {
			t.Errorf("UncheckedPushesSystem (deployment) = %v, %v; want the deployment signer's push", ids(got), err)
		}
		if got, err = store.UncheckedPushesSystem(ctx, orgID, domain.CommitSigningOrg, time.Now().Add(time.Hour), 10); err != nil || len(got) != 0 {
			t.Errorf("UncheckedPushesSystem since a later time = %v, %v; want none", ids(got), err)
		}
		if got, err = store.UncheckedPushesSystem(ctx, orgID, domain.CommitSigningOrg, hourAgo, 0); err != nil || len(got) != 0 {
			t.Errorf("UncheckedPushesSystem limit 0 = %v, %v; want none", ids(got), err)
		}
	})
}

// RunCommitSignatureCheckConformance pins the post-push check's verdict write,
// a control-plane write the executor's role cannot make, so it runs apart from
// RunCommitSignatureStoreConformance, which the executor's role runs too.
func RunCommitSignatureCheckConformance(t *testing.T, mk CommitSignatureStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("a_verdict_is_stored_on_a_pushed_commit", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conversationID, claimID := seed.Claim(t)
		repositoryID := seed.Repository(t)
		row, err := store.InsertSystem(ctx, signedSignature(orgID, conversationID, claimID, time.Now()))
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}
		if _, err := store.SetVerificationSystem(ctx, orgID, row.ID, "no_user"); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SetVerificationSystem before a push = %v, want ErrNoSuchCommitSignature", err)
		}
		if _, err := store.SetVerificationSystem(ctx, orgID, uuid.NewString(), "no_user"); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SetVerificationSystem on an unknown id = %v, want ErrNoSuchCommitSignature", err)
		}
		if _, err := store.MarkPushedSystem(ctx, orgID, row.ID, repositoryID); err != nil {
			t.Fatalf("MarkPushedSystem: %v", err)
		}
		read := func() (*domain.CommitSignature, error) { return store.GetSystem(ctx, orgID, row.ID) }

		failed, err := store.SetVerificationSystem(ctx, orgID, row.ID, "unverified_email")
		if err != nil {
			t.Fatalf("SetVerificationSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetVerificationSystem", failed, read)
		if failed.VerificationReason != "unverified_email" || failed.CheckedAt == nil {
			t.Errorf("stored verdict %q at %v, want unverified_email with a check time", failed.VerificationReason, failed.CheckedAt)
		}
		if pending, err := store.UncheckedPushesSystem(ctx, orgID, domain.CommitSigningOrg, time.Now().Add(-time.Hour), 10); err != nil || len(pending) != 0 {
			t.Errorf("UncheckedPushesSystem after the verdict = %d rows, %v; want none", len(pending), err)
		}
		newest, err := store.NewestFailedVerificationSystem(ctx, orgID, "https://github.com", 583231)
		if err != nil || newest == nil || newest.ID != row.ID {
			t.Errorf("NewestFailedVerificationSystem = %+v, %v; want the commit just checked", newest, err)
		}

		// No answer: the commit or its repository is gone.
		gone, err := store.SetVerificationSystem(ctx, orgID, row.ID, "")
		if err != nil {
			t.Fatalf("SetVerificationSystem(\"\"): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetVerificationSystem (no answer)", gone, read)
		if gone.VerificationReason != "" || gone.CheckedAt == nil {
			t.Errorf("stored verdict %q at %v, want no reason with a check time", gone.VerificationReason, gone.CheckedAt)
		}
	})
}

// CommitSignatureQueueFixture is what the queue suite runs against: the
// signature store and the claims it checks liveness against, the org every
// call is scoped to, and a seeder for engagements.
type CommitSignatureQueueFixture struct {
	Signatures db.CommitSignatureStore
	Claims     db.ConversationQueueStore
	OrgID      string

	// Claim inserts a conversation and a claim on it at boot epoch 1,
	// claimed at claimedAt with a five-minute lease, released or not, and
	// returns both ids.
	Claim func(t *testing.T, claimedAt time.Time, released bool) (conversationID, claimID string)
}

// CommitSignatureQueueFactory hands the queue suite a fixture over storage no
// earlier subtest wrote to.
type CommitSignatureQueueFactory func(t *testing.T) CommitSignatureQueueFixture

// RunCommitSignatureQueueConformance pins the control-plane queue: a request
// is claimed by exactly one pod, only that pod stores its outcome, the sweep
// sees only what no pod has claimed, and a claim is live only while
// unreleased, within its lease and at the boot epoch named.
//
// It is the control pods' half of the store, run on their admin pool. The
// executor's tf_system role is refused these writes, which
// RunCommitSignatureStoreConformance's caller pins separately.
func RunCommitSignatureQueueConformance(t *testing.T, mk CommitSignatureQueueFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		digest  = "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"
		armored = "-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----"
	)
	request := func(t *testing.T, f CommitSignatureQueueFixture, conversationID, claimID string) domain.CommitSignature {
		t.Helper()
		row, err := f.Signatures.InsertSystem(ctx, domain.CommitSignature{
			OrgID:          f.OrgID,
			ConversationID: conversationID,
			ClaimID:        claimID,
			BootEpoch:      1,
			Signer:         domain.CommitSigningOrg,
			SignerHost:     "https://github.com",
			SignerUserID:   583231,
			Digest:         digest,
			State:          domain.CommitSignatureRequested,
		})
		if err != nil {
			t.Fatalf("InsertSystem (requested): %v", err)
		}
		return row
	}

	t.Run("a_request_is_claimed_by_one_pod", func(t *testing.T) {
		f := mk(t)
		conversationID, claimID := f.Claim(t, time.Now(), false)
		row := request(t, f, conversationID, claimID)

		got, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, row.ID, "pod-a")
		if err != nil {
			t.Fatalf("ClaimRequestSystem: %v", err)
		}
		if got == nil {
			t.Fatal("ClaimRequestSystem = nil, want the request it took")
		}
		AssertWriteReturnedStoredRow(t, "ClaimRequestSystem", *got, func() (*domain.CommitSignature, error) {
			return f.Signatures.GetSystem(ctx, f.OrgID, row.ID)
		})
		if got.State != domain.CommitSignatureSigning || got.ClaimedBy != "pod-a" {
			t.Errorf("claimed row = state %q by %q, want signing by pod-a", got.State, got.ClaimedBy)
		}

		again, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, row.ID, "pod-b")
		if err != nil {
			t.Fatalf("ClaimRequestSystem (second pod): %v", err)
		}
		if again != nil {
			t.Errorf("a second pod claimed %+v, want nil: the request is pod-a's", again)
		}
		for name, id := range map[string]string{"unknown": uuid.New().String(), "malformed": "not-a-uuid"} {
			if got, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, id, "pod-a"); err != nil || got != nil {
				t.Errorf("ClaimRequestSystem(%s id) = %+v, %v; want nil, nil", name, got, err)
			}
		}
	})

	t.Run("only_the_claiming_pod_signs", func(t *testing.T) {
		f := mk(t)
		conversationID, claimID := f.Claim(t, time.Now(), false)
		row := request(t, f, conversationID, claimID)
		signedAt := time.Now().UTC().Truncate(time.Millisecond)

		if _, err := f.Signatures.SignSystem(ctx, f.OrgID, row.ID, "pod-a", signerKeyA, armored, signedAt); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SignSystem on an unclaimed request = %v, want ErrNoSuchCommitSignature", err)
		}
		if _, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, row.ID, "pod-a"); err != nil {
			t.Fatalf("ClaimRequestSystem: %v", err)
		}
		if _, err := f.Signatures.SignSystem(ctx, f.OrgID, row.ID, "pod-b", signerKeyA, armored, signedAt); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("SignSystem by another pod = %v, want ErrNoSuchCommitSignature", err)
		}
		for name, args := range map[string][2]string{"no key": {"", armored}, "no signature": {signerKeyA, ""}} {
			if _, err := f.Signatures.SignSystem(ctx, f.OrgID, row.ID, "pod-a", args[0], args[1], signedAt); !errors.Is(err, db.ErrInvalidCommitSignature) {
				t.Errorf("SignSystem with %s = %v, want ErrInvalidCommitSignature", name, err)
			}
		}

		got, err := f.Signatures.SignSystem(ctx, f.OrgID, row.ID, "pod-a", signerKeyA, armored, signedAt)
		if err != nil {
			t.Fatalf("SignSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SignSystem", got, func() (*domain.CommitSignature, error) {
			return f.Signatures.GetSystem(ctx, f.OrgID, row.ID)
		})
		if got.State != domain.CommitSignatureSigned || got.PublicKey != signerKeyA || got.Signature != armored ||
			got.SignedAt == nil || !got.SignedAt.Equal(signedAt) || got.ClaimedBy != "pod-a" {
			t.Errorf("signed row = %+v, want signed by pod-a with its key, signature and time", got)
		}
		if n, err := f.Signatures.ClaimSignaturesSystem(ctx, f.OrgID, claimID); err != nil || n != 1 {
			t.Errorf("ClaimSignaturesSystem = %d, %v; want the signature counted", n, err)
		}
		if w, err := f.Signatures.OrgSignaturesSinceSystem(ctx, f.OrgID, signedAt.Add(-time.Minute)); err != nil || w.Count != 1 {
			t.Errorf("OrgSignaturesSinceSystem = %+v, %v; want the signature counted", w, err)
		}
		if _, err := f.Signatures.SignSystem(ctx, f.OrgID, row.ID, "pod-a", signerKeyB, armored, signedAt); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("signing a signed row again = %v, want ErrNoSuchCommitSignature", err)
		}
		if _, err := f.Signatures.RefuseSystem(ctx, f.OrgID, row.ID, "pod-a", "claim_budget", 0); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("refusing a signed row = %v, want ErrNoSuchCommitSignature", err)
		}
	})

	t.Run("only_the_claiming_pod_refuses", func(t *testing.T) {
		f := mk(t)
		conversationID, claimID := f.Claim(t, time.Now(), false)
		row := request(t, f, conversationID, claimID)

		if _, err := f.Signatures.RefuseSystem(ctx, f.OrgID, row.ID, "pod-a", "org_minute_budget", 12); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("RefuseSystem on an unclaimed request = %v, want ErrNoSuchCommitSignature", err)
		}
		if _, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, row.ID, "pod-a"); err != nil {
			t.Fatalf("ClaimRequestSystem: %v", err)
		}
		if _, err := f.Signatures.RefuseSystem(ctx, f.OrgID, row.ID, "pod-b", "org_minute_budget", 12); !errors.Is(err, db.ErrNoSuchCommitSignature) {
			t.Errorf("RefuseSystem by another pod = %v, want ErrNoSuchCommitSignature", err)
		}
		if _, err := f.Signatures.RefuseSystem(ctx, f.OrgID, row.ID, "pod-a", "", 0); !errors.Is(err, db.ErrInvalidCommitSignature) {
			t.Errorf("RefuseSystem with no reason = %v, want ErrInvalidCommitSignature", err)
		}
		if _, err := f.Signatures.RefuseSystem(ctx, f.OrgID, row.ID, "pod-a", "org_minute_budget", -1); !errors.Is(err, db.ErrInvalidCommitSignature) {
			t.Errorf("RefuseSystem with a negative wait = %v, want ErrInvalidCommitSignature", err)
		}

		got, err := f.Signatures.RefuseSystem(ctx, f.OrgID, row.ID, "pod-a", "org_minute_budget", 12)
		if err != nil {
			t.Fatalf("RefuseSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "RefuseSystem", got, func() (*domain.CommitSignature, error) {
			return f.Signatures.GetSystem(ctx, f.OrgID, row.ID)
		})
		if got.State != domain.CommitSignatureRefused || got.RefusalReason != "org_minute_budget" || got.RetryAfterSeconds != 12 {
			t.Errorf("refused row = %+v, want refused for org_minute_budget, retry in 12s", got)
		}
		if got.PublicKey != "" || got.Signature != "" || got.SignedAt != nil {
			t.Errorf("refused row = %+v, want nothing signed", got)
		}
		if n, err := f.Signatures.ClaimSignaturesSystem(ctx, f.OrgID, claimID); err != nil || n != 0 {
			t.Errorf("ClaimSignaturesSystem = %d, %v; want a refusal not counted", n, err)
		}

		other := request(t, f, conversationID, claimID)
		if _, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, other.ID, "pod-a"); err != nil {
			t.Fatalf("ClaimRequestSystem (other): %v", err)
		}
		plain, err := f.Signatures.RefuseSystem(ctx, f.OrgID, other.ID, "pod-a", "claim_inactive", 0)
		if err != nil {
			t.Fatalf("RefuseSystem (no wait): %v", err)
		}
		if plain.RetryAfterSeconds != 0 {
			t.Errorf("a refusal with no wait stored retry_after %d, want 0", plain.RetryAfterSeconds)
		}
	})

	t.Run("the_sweep_sees_only_unclaimed_requests", func(t *testing.T) {
		f := mk(t)
		conversationID, claimID := f.Claim(t, time.Now(), false)

		empty, err := f.Signatures.ListRequestedSystem(ctx, 0, 10)
		if err != nil {
			t.Fatalf("ListRequestedSystem (empty): %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("ListRequestedSystem with no requests = %d rows, want none", len(empty))
		}
		if n, age, err := f.Signatures.PendingRequestsSystem(ctx); err != nil || n != 0 || age != 0 {
			t.Errorf("PendingRequestsSystem with no requests = %d, %s, %v; want 0, 0", n, age, err)
		}

		first := request(t, f, conversationID, claimID)
		claimed := request(t, f, conversationID, claimID)
		third := request(t, f, conversationID, claimID)
		if _, err := f.Signatures.ClaimRequestSystem(ctx, f.OrgID, claimed.ID, "pod-a"); err != nil {
			t.Fatalf("ClaimRequestSystem: %v", err)
		}
		signedAt := time.Now()
		if _, err := f.Signatures.InsertSystem(ctx, domain.CommitSignature{
			OrgID: f.OrgID, ConversationID: conversationID, ClaimID: claimID, BootEpoch: 1,
			Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: 583231,
			PublicKey: signerKeyA, Digest: digest, State: domain.CommitSignatureSigned,
			Signature: armored, SignedAt: &signedAt,
		}); err != nil {
			t.Fatalf("InsertSystem (signed): %v", err)
		}

		got, err := f.Signatures.ListRequestedSystem(ctx, 0, 10)
		if err != nil {
			t.Fatalf("ListRequestedSystem: %v", err)
		}
		if len(got) != 2 || got[0].ID != first.ID || got[1].ID != third.ID {
			t.Errorf("ListRequestedSystem = %v, want the two unclaimed requests oldest first (%s, %s)", signatureIDs(got), first.ID, third.ID)
		}
		if limited, err := f.Signatures.ListRequestedSystem(ctx, 0, 1); err != nil || len(limited) != 1 || limited[0].ID != first.ID {
			t.Errorf("ListRequestedSystem(limit 1) = %v, %v; want the oldest alone", signatureIDs(limited), err)
		}
		if young, err := f.Signatures.ListRequestedSystem(ctx, time.Hour, 10); err != nil || len(young) != 0 {
			t.Errorf("ListRequestedSystem(min age 1h) = %v, %v; want none: every request is younger", signatureIDs(young), err)
		}

		n, age, err := f.Signatures.PendingRequestsSystem(ctx)
		if err != nil {
			t.Fatalf("PendingRequestsSystem: %v", err)
		}
		if n != 2 {
			t.Errorf("PendingRequestsSystem count = %d, want the 2 unclaimed requests", n)
		}
		if age < 0 || age > time.Minute {
			t.Errorf("PendingRequestsSystem oldest age = %s, want the few moments since the first request", age)
		}
	})

	t.Run("a_claim_is_live_only_unreleased_within_its_lease_at_its_epoch", func(t *testing.T) {
		f := mk(t)
		_, live := f.Claim(t, time.Now(), false)
		_, released := f.Claim(t, time.Now(), true)
		_, lapsed := f.Claim(t, time.Now().Add(-10*time.Minute), false)

		for name, c := range map[string]struct {
			claimID string
			epoch   int64
			want    bool
		}{
			"live at its epoch":     {live, 1, true},
			"live at another epoch": {live, 2, false},
			"released":              {released, 1, false},
			"past its lease":        {lapsed, 1, false},
			"unknown":               {uuid.New().String(), 1, false},
		} {
			got, err := f.Claims.ClaimLiveSystem(ctx, f.OrgID, c.claimID, c.epoch)
			if err != nil {
				t.Fatalf("ClaimLiveSystem (%s): %v", name, err)
			}
			if got != c.want {
				t.Errorf("ClaimLiveSystem (%s) = %v, want %v", name, got, c.want)
			}
		}
	})
}

func signatureIDs(rows []domain.CommitSignature) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
