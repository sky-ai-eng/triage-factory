package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func testSecretKey(t *testing.T) aead.Key {
	t.Helper()
	var k aead.Key
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// TestDisabledSecretStore_EveryMethodReturnsTheSentinel pins TFAC-614's
// acceptance criterion: on TF_ROLE=executor, Secrets.Get* returns a
// distinct, greppable sentinel rather than attempting any decrypt.
func TestDisabledSecretStore_EveryMethodReturnsTheSentinel(t *testing.T) {
	ctx := context.Background()
	s := newDisabledSecretStore()

	if err := s.Put(ctx, "org", "key", "value", ""); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("Put error = %v, want ErrSecretStoreUnavailable", err)
	}
	if _, err := s.Get(ctx, "org", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("Get error = %v, want ErrSecretStoreUnavailable", err)
	}
	if _, err := s.GetSystem(ctx, "org", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("GetSystem error = %v, want ErrSecretStoreUnavailable", err)
	}
	if _, err := s.Delete(ctx, "org", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("Delete error = %v, want ErrSecretStoreUnavailable", err)
	}
	if err := s.PutUser(ctx, "org", "user", "key", "value", ""); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("PutUser error = %v, want ErrSecretStoreUnavailable", err)
	}
	if _, err := s.GetUser(ctx, "org", "user", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("GetUser error = %v, want ErrSecretStoreUnavailable", err)
	}
	if _, err := s.GetUserSystem(ctx, "org", "user", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("GetUserSystem error = %v, want ErrSecretStoreUnavailable", err)
	}
	if err := s.PutUserSystem(ctx, "org", "user", "key", "value", ""); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("PutUserSystem error = %v, want ErrSecretStoreUnavailable", err)
	}
	if _, err := s.DeleteUser(ctx, "org", "user", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("DeleteUser error = %v, want ErrSecretStoreUnavailable", err)
	}
}

// TestNewStoreBundle_SecretsRouting pins the gap this ticket closes: every
// internal SecretStore construction (the top-level bundle AND both
// tx-bound rebuilds in tx.go) must route through Store.buildSecrets, so a
// nil secretKey (TF_ROLE=executor) can never produce a real, if
// pointlessly zero-keyed, decrypting store anywhere in this package.
func TestNewStoreBundle_SecretsRouting(t *testing.T) {
	disabled := (&Store{secretKey: nil}).buildSecrets(nil, nil)
	if _, err := disabled.Get(context.Background(), "org", "key"); !errors.Is(err, db.ErrSecretStoreUnavailable) {
		t.Errorf("buildSecrets(nil key).Get error = %v, want ErrSecretStoreUnavailable", err)
	}

	key := testSecretKey(t)
	real := (&Store{secretKey: &key}).buildSecrets(nil, nil)
	if _, ok := real.(disabledSecretStore); ok {
		t.Error("buildSecrets with a non-nil key returned the disabled store")
	}
}
