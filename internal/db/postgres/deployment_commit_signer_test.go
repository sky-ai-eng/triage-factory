package postgres_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

const (
	deploymentHost   = "https://github.com"
	deploymentKey    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDdddddddddddddddddddddddddddddddddddddddddd"
	deploymentUserID = 90001
)

func deploymentSignerStore(t *testing.T) (db.DeploymentCommitSignerStore, *pgtest.Harness) {
	t.Helper()
	h := pgtest.Shared(t)
	h.Reset(t)
	return pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).DeploymentCommitSigners, h
}

// TestDeploymentCommitSignerStore_Postgres_Register pins the row every control
// pod creates for its configured key at boot: the first registration creates
// it pending under the configured login, and a later one, from another pod or
// a restart, hands back the row as stored, whatever login it names.
func TestDeploymentCommitSignerStore_Postgres_Register(t *testing.T) {
	store, _ := deploymentSignerStore(t)
	ctx := t.Context()
	read := func() (*domain.DeploymentCommitSigner, error) {
		return store.GetSystem(ctx, deploymentHost, deploymentKey)
	}

	if got, err := read(); err != nil || got != nil {
		t.Fatalf("GetSystem before any registration = %+v, %v; want nil", got, err)
	}
	created, err := store.RegisterSystem(ctx, "https://GitHub.com/", deploymentKey, "TriageFactory")
	if err != nil {
		t.Fatalf("RegisterSystem: %v", err)
	}
	if created.Host != deploymentHost || created.Login != "TriageFactory" || created.GitHubUserID != 0 ||
		created.Status != domain.CommitSignerPending || created.CheckedAt != nil {
		t.Errorf("registered row = %+v, want pending under the login, no account, never checked", created)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "RegisterSystem", created, read)

	checked, err := store.RecordKeyCheckSystem(ctx, created, db.DeploymentSignerKeyCheck{
		GitHubUserID: deploymentUserID, Login: "TriageFactory", Name: "Triage Factory", KeyFound: true,
	})
	if err != nil || checked == nil {
		t.Fatalf("RecordKeyCheckSystem = %+v, %v", checked, err)
	}
	again, err := store.RegisterSystem(ctx, deploymentHost, deploymentKey, "renamed-in-config")
	if err != nil {
		t.Fatalf("second RegisterSystem: %v", err)
	}
	if !reflect.DeepEqual(again, *checked) {
		t.Errorf("second RegisterSystem = %+v, want the stored row %+v unchanged", again, *checked)
	}

	other, err := store.RegisterSystem(ctx, "https://ghe.acme.example", deploymentKey, "TriageFactory")
	if err != nil {
		t.Fatalf("RegisterSystem on another host: %v", err)
	}
	if other.GitHubUserID != 0 || other.Status != domain.CommitSignerPending {
		t.Errorf("the same key on another host = %+v, want a row of its own, pending", other)
	}

	for name, args := range map[string][3]string{
		"no host":  {"", deploymentKey, "TriageFactory"},
		"no key":   {deploymentHost, "", "TriageFactory"},
		"no login": {deploymentHost, deploymentKey, ""},
	} {
		if _, err := store.RegisterSystem(ctx, args[0], args[1], args[2]); !errors.Is(err, db.ErrInvalidOrgCommitSigner) {
			t.Errorf("RegisterSystem with %s = %v, want ErrInvalidOrgCommitSigner", name, err)
		}
	}
}

// TestDeploymentCommitSignerStore_Postgres_KeyCheck walks every status a check
// can find the signer in, with the key found and not, and pins that a check
// read against a status that has since moved changes nothing.
func TestDeploymentCommitSignerStore_Postgres_KeyCheck(t *testing.T) {
	account := db.DeploymentSignerKeyCheck{GitHubUserID: deploymentUserID, Login: "TriageFactory", Name: "Triage Factory"}
	for _, tc := range []struct {
		name       string
		from       domain.CommitSignerStatus
		fromReason string
		found      bool
		want       domain.CommitSignerStatus
		wantReason string
	}{
		{"pending, key found", domain.CommitSignerPending, "", true, domain.CommitSignerActive, ""},
		{"pending, key missing", domain.CommitSignerPending, "", false, domain.CommitSignerPending, ""},
		{"active, key found", domain.CommitSignerActive, "", true, domain.CommitSignerActive, ""},
		{"active, key missing", domain.CommitSignerActive, "", false, domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing},
		{"broken for its key, key back", domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing, true, domain.CommitSignerActive, ""},
		{"broken by a commit, key found", domain.CommitSignerBroken, "unverified_email", true, domain.CommitSignerBroken, "unverified_email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, h := deploymentSignerStore(t)
			ctx := t.Context()
			if _, err := store.RegisterSystem(ctx, deploymentHost, deploymentKey, "TriageFactory"); err != nil {
				t.Fatalf("RegisterSystem: %v", err)
			}
			pgtest.MustExec(t, h.AdminDB, `
				UPDATE deployment_commit_signer SET status = $1, status_reason = NULLIF($2, ''),
					status_since = now() - interval '1 hour'`, string(tc.from), tc.fromReason)
			before, err := store.GetSystem(ctx, deploymentHost, deploymentKey)
			if err != nil || before == nil {
				t.Fatalf("GetSystem: %+v, %v", before, err)
			}

			check := account
			check.KeyFound = tc.found
			got, err := store.RecordKeyCheckSystem(ctx, *before, check)
			if err != nil || got == nil {
				t.Fatalf("RecordKeyCheckSystem = %+v, %v", got, err)
			}
			if got.Status != tc.want || got.StatusReason != tc.wantReason {
				t.Errorf("status = %s (%q), want %s (%q)", got.Status, got.StatusReason, tc.want, tc.wantReason)
			}
			if got.GitHubUserID != deploymentUserID || got.Login != "TriageFactory" || got.Name != "Triage Factory" || got.CheckedAt == nil {
				t.Errorf("row = %+v, want the account recorded and checked_at stamped", got)
			}
			if moved := !got.StatusSince.Equal(before.StatusSince); moved != (tc.want != tc.from) {
				t.Errorf("status_since moved = %v, want it to move only with the status", moved)
			}
			dbtest.AssertWriteReturnedStoredRow(t, "RecordKeyCheckSystem", *got, func() (*domain.DeploymentCommitSigner, error) {
				return store.GetSystem(ctx, deploymentHost, deploymentKey)
			})

			// The row moved since before was read, so a check made against
			// before is a lost race and changes nothing.
			if stale, err := store.RecordKeyCheckSystem(ctx, *before, check); err != nil || (stale != nil && tc.want != tc.from) {
				t.Errorf("a check against a status that has moved = %+v, %v; want (nil, nil)", stale, err)
			}
		})
	}

	t.Run("an account the check could not find keeps the row's", func(t *testing.T) {
		store, _ := deploymentSignerStore(t)
		ctx := t.Context()
		row, _ := store.RegisterSystem(ctx, deploymentHost, deploymentKey, "TriageFactory")
		first, err := store.RecordKeyCheckSystem(ctx, row, db.DeploymentSignerKeyCheck{
			GitHubUserID: deploymentUserID, Login: "TriageFactory", Name: "Triage Factory", KeyFound: true,
		})
		if err != nil || first == nil {
			t.Fatalf("first check = %+v, %v", first, err)
		}
		gone, err := store.RecordKeyCheckSystem(ctx, *first, db.DeploymentSignerKeyCheck{})
		if err != nil || gone == nil {
			t.Fatalf("check of a gone account = %+v, %v", gone, err)
		}
		if gone.GitHubUserID != deploymentUserID || gone.Login != "TriageFactory" || gone.Name != "Triage Factory" {
			t.Errorf("row = %+v, want the account kept as stored", gone)
		}
		if gone.Status != domain.CommitSignerBroken || gone.StatusReason != domain.CommitSignerReasonKeyMissing {
			t.Errorf("status = %s (%q), want broken (key_missing): an account GitHub no longer has holds no key", gone.Status, gone.StatusReason)
		}
	})

	t.Run("another account than the one the row follows", func(t *testing.T) {
		store, _ := deploymentSignerStore(t)
		ctx := t.Context()
		row, _ := store.RegisterSystem(ctx, deploymentHost, deploymentKey, "TriageFactory")
		first, err := store.RecordKeyCheckSystem(ctx, row, db.DeploymentSignerKeyCheck{GitHubUserID: deploymentUserID, Login: "TriageFactory", KeyFound: true})
		if err != nil || first == nil {
			t.Fatalf("first check = %+v, %v", first, err)
		}
		if got, err := store.RecordKeyCheckSystem(ctx, *first, db.DeploymentSignerKeyCheck{GitHubUserID: 42, Login: "squatter", KeyFound: true}); err != nil || got != nil {
			t.Errorf("a check that read another account = %+v, %v; want (nil, nil)", got, err)
		}
	})
}

// TestDeploymentCommitSignerStore_Postgres_ActivateAndBreak pins Verify's write
// and the post-push check's, with BreakSystem's precedence.
func TestDeploymentCommitSignerStore_Postgres_ActivateAndBreak(t *testing.T) {
	store, _ := deploymentSignerStore(t)
	ctx := t.Context()
	read := func() (*domain.DeploymentCommitSigner, error) {
		return store.GetSystem(ctx, deploymentHost, deploymentKey)
	}
	if _, err := store.ActivateSystem(ctx, deploymentHost, deploymentKey, deploymentUserID, "TriageFactory", ""); !errors.Is(err, db.ErrNoSuchDeploymentCommitSigner) {
		t.Fatalf("ActivateSystem with no row = %v, want ErrNoSuchDeploymentCommitSigner", err)
	}
	if _, err := store.RegisterSystem(ctx, deploymentHost, deploymentKey, "TriageFactory"); err != nil {
		t.Fatalf("RegisterSystem: %v", err)
	}

	active, err := store.ActivateSystem(ctx, deploymentHost, deploymentKey, deploymentUserID, "TriageFactory", "Triage Factory")
	if err != nil {
		t.Fatalf("ActivateSystem: %v", err)
	}
	if active.Status != domain.CommitSignerActive || active.GitHubUserID != deploymentUserID || active.Name != "Triage Factory" || active.CheckedAt == nil {
		t.Errorf("activated row = %+v, want active on the account, checked", active)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "ActivateSystem", active, read)
	if _, err := store.ActivateSystem(ctx, deploymentHost, deploymentKey, 42, "squatter", ""); !errors.Is(err, db.ErrNoSuchDeploymentCommitSigner) {
		t.Errorf("ActivateSystem for another account = %v, want ErrNoSuchDeploymentCommitSigner", err)
	}

	for _, reason := range []string{"", domain.CommitVerificationValid} {
		if _, err := store.BreakSystem(ctx, deploymentHost, deploymentKey, reason); !errors.Is(err, db.ErrInvalidOrgCommitSigner) {
			t.Errorf("BreakSystem(%q) = %v, want ErrInvalidOrgCommitSigner", reason, err)
		}
	}
	if got, err := store.BreakSystem(ctx, deploymentHost, "ssh-ed25519 AAAAother", "no_user"); err != nil || got != nil {
		t.Errorf("BreakSystem for another key = %+v, %v; want (nil, nil)", got, err)
	}
	broken, err := store.BreakSystem(ctx, deploymentHost, deploymentKey, "unverified_email")
	if err != nil || broken == nil {
		t.Fatalf("BreakSystem = %+v, %v", broken, err)
	}
	if broken.Status != domain.CommitSignerBroken || broken.StatusReason != "unverified_email" {
		t.Errorf("broken row = %+v, want broken (unverified_email)", broken)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "BreakSystem", *broken, read)
	if got, err := store.BreakSystem(ctx, deploymentHost, deploymentKey, "no_user"); err != nil || got != nil {
		t.Errorf("BreakSystem of a signer a commit already broke = %+v, %v; want it to keep its reason", got, err)
	}

	recovered, err := store.ActivateSystem(ctx, deploymentHost, deploymentKey, deploymentUserID, "TriageFactory", "Triage Factory")
	if err != nil || recovered.Status != domain.CommitSignerActive || recovered.StatusReason != "" {
		t.Fatalf("ActivateSystem of a broken signer = %+v, %v; want active with no reason", recovered, err)
	}
}

// TestDeploymentCommitSignerStore_Postgres_NewestFailedVerification pins
// Verify's read of the newest failing commit signed as the deployment signer,
// across every org.
func TestDeploymentCommitSignerStore_Postgres_NewestFailedVerification(t *testing.T) {
	store, h := deploymentSignerStore(t)
	ctx := t.Context()
	sigs := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).CommitSignatures
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "deploy-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "deploy-b")
	seedA := pgCommitSignatureSeeder(t, h, orgA, userA)
	seedB := pgCommitSignatureSeeder(t, h, orgB, userB)

	hour := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	sign := func(seed dbtest.CommitSignatureSeeder, orgID string, signer domain.CommitSigningMode, at time.Time) domain.CommitSignature {
		t.Helper()
		conversationID, claimID := seed.Claim(t)
		row, err := sigs.InsertSystem(ctx, domain.CommitSignature{
			OrgID: orgID, ConversationID: conversationID, ClaimID: claimID, BootEpoch: 1,
			Signer: signer, SignerHost: deploymentHost, SignerUserID: deploymentUserID,
			PublicKey: deploymentKey, Digest: "ab", State: domain.CommitSignatureSigned,
			Signature: "-----BEGIN SSH SIGNATURE-----", SignedAt: &at,
		})
		if err != nil {
			t.Fatalf("InsertSystem: %v", err)
		}
		return row
	}

	a1 := sign(seedA, orgA, domain.CommitSigningDeployment, hour.Add(5*time.Minute))
	a2 := sign(seedA, orgA, domain.CommitSigningDeployment, hour.Add(70*time.Minute))
	b1 := sign(seedB, orgB, domain.CommitSigningDeployment, hour.Add(10*time.Minute))
	orgSigned := sign(seedA, orgA, domain.CommitSigningOrg, hour.Add(20*time.Minute))

	if got, err := store.NewestFailedVerificationSystem(ctx, deploymentHost, deploymentUserID); err != nil || got != nil {
		t.Fatalf("NewestFailedVerificationSystem with no failure = %+v, %v; want nil", got, err)
	}
	seedA.SetVerification(t, a1.ID, "unverified_email")
	seedB.SetVerification(t, b1.ID, "no_user")
	seedA.SetVerification(t, a2.ID, domain.CommitVerificationValid)
	seedA.SetVerification(t, orgSigned.ID, "no_user")
	got, err := store.NewestFailedVerificationSystem(ctx, deploymentHost, deploymentUserID)
	if err != nil || got == nil || got.ID != b1.ID {
		t.Errorf("NewestFailedVerificationSystem = %+v, %v; want orgB's commit, the newest deployment signature that failed", got, err)
	}
	if got, err := store.NewestFailedVerificationSystem(ctx, deploymentHost, 42); err != nil || got != nil {
		t.Errorf("NewestFailedVerificationSystem for another account = %+v, %v; want nil", got, err)
	}
}
