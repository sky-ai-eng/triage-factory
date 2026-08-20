package postgres

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// disabledSecretStore is the db.SecretStore wired on TF_ROLE=executor
// (TFAC-614): every method returns db.ErrSecretStoreUnavailable
// immediately, without touching the database or any key material. It
// exists so a consumer that was missed when converting to the sealed
// claim_credentials bundle path fails loudly and greppably at the first call,
// rather than an executor holding (or worse, silently lacking) a real
// decrypting SecretStore.
type disabledSecretStore struct{}

func newDisabledSecretStore() db.SecretStore { return disabledSecretStore{} }

var _ db.SecretStore = disabledSecretStore{}

func (disabledSecretStore) Put(context.Context, string, string, string, string) error {
	return db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) Get(context.Context, string, string) (string, error) {
	return "", db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) GetSystem(context.Context, string, string) (string, error) {
	return "", db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) Delete(context.Context, string, string) (bool, error) {
	return false, db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) PutUser(context.Context, string, string, string, string, string) error {
	return db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) GetUser(context.Context, string, string, string) (string, error) {
	return "", db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) GetUserSystem(context.Context, string, string, string) (string, error) {
	return "", db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) PutUserSystem(context.Context, string, string, string, string, string) error {
	return db.ErrSecretStoreUnavailable
}

func (disabledSecretStore) DeleteUser(context.Context, string, string, string) (bool, error) {
	return false, db.ErrSecretStoreUnavailable
}
