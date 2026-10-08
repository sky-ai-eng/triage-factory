package server

import (
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSnapshotSecrets_Restores pins the local-mode rollback the credential
// writes rely on: keys put back as they were, and keys that were absent
// removed.
func TestSnapshotSecrets_Restores(t *testing.T) {
	r := newLinearAccessRig(t)
	r.putSecret(t, integrations.KeyLinearAPIKey, "lin_api_old")
	r.putSecret(t, integrations.KeyLinearAuthMethod, "api_key")

	restore, err := r.s.snapshotSecrets(t.Context(), runmode.LocalDefaultOrgID, integrations.LinearKeys())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	r.putSecret(t, integrations.KeyLinearAPIKey, "lin_api_new")
	r.putSecret(t, integrations.KeyLinearBoundAs, `{"name":"ada"}`)
	restore()

	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "lin_api_old" {
		t.Errorf("key = %q, want the prior key restored", v)
	}
	if v := r.secret(t, integrations.KeyLinearAuthMethod); v != "api_key" {
		t.Errorf("marker = %q, want it kept", v)
	}
	if v := r.secret(t, integrations.KeyLinearBoundAs); v != "" {
		t.Errorf("bound-as = %q, want it removed — it was absent before", v)
	}
}

// TestGuardLocalSecretWrite_SerializesLocalWrites: in local mode the guard
// holds the integration's mutex until its unlock runs, so a second write
// waits for the first to finish restoring. A caller that already holds its
// integration's lock passes no mutex and gets a restore all the same. Multi
// mode keeps the secrets inside the transaction, so it takes no lock and has
// nothing to restore.
func TestGuardLocalSecretWrite_SerializesLocalWrites(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		r := newLinearAccessRig(t)
		var mu sync.Mutex
		restore, unlock, err := r.s.guardLocalSecretWrite(t.Context(), &mu, runmode.LocalDefaultOrgID, integrations.LinearKeys()...)
		if err != nil {
			t.Fatalf("guard: %v", err)
		}
		if restore == nil {
			t.Error("local guard returned no restore")
		}
		if mu.TryLock() {
			mu.Unlock()
			t.Fatal("a second writer took the lock while the first held it")
		}
		unlock()
		if !mu.TryLock() {
			t.Fatal("the lock is still held after unlock")
		}
		mu.Unlock()
	})

	t.Run("local, lock already held", func(t *testing.T) {
		r := newLinearAccessRig(t)
		restore, unlock, err := r.s.guardLocalSecretWrite(t.Context(), nil, runmode.LocalDefaultOrgID, integrations.GitHubKeys()...)
		if err != nil {
			t.Fatalf("guard: %v", err)
		}
		if restore == nil {
			t.Error("local guard returned no restore")
		}
		unlock()
	})

	t.Run("refuses an unreadable snapshot and releases the lock", func(t *testing.T) {
		r := newLinearAccessRig(t)
		r.s.secrets = getFailingSecrets{SecretStore: r.s.secrets}
		var mu sync.Mutex
		if _, _, err := r.s.guardLocalSecretWrite(t.Context(), &mu, runmode.LocalDefaultOrgID, integrations.JiraKeys()...); err == nil {
			t.Fatal("guard took a snapshot it could not read")
		}
		if !mu.TryLock() {
			t.Fatal("a refused guard kept the lock")
		}
		mu.Unlock()
	})

	t.Run("multi", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		s := &Server{}
		var mu sync.Mutex
		restore, unlock, err := s.guardLocalSecretWrite(t.Context(), &mu, "org-1", integrations.LinearKeys()...)
		if err != nil {
			t.Fatalf("guard: %v", err)
		}
		if restore != nil {
			t.Error("multi guard returned a restore; the transaction's rollback covers the secrets")
		}
		if !mu.TryLock() {
			t.Fatal("multi guard took the lock")
		}
		mu.Unlock()
		unlock()
	})
}
