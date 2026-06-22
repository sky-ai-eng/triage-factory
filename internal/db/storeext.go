package db

import (
	"context"
	"database/sql"
	"sync"
)

// Execer is the minimal database handle a store-extension factory
// receives — the same three methods *sql.DB and *sql.Tx both satisfy
// (it mirrors the per-dialect `queryer` declared in db/postgres and
// db/sqlite). Core hands the app-pool and admin-pool handles to
// ee-registered factories as Execers so an out-of-core package (the
// Enterprise Edition's SSO stores) can build pool-split stores that
// participate in the same claims-set transaction as core's own stores,
// without core ever naming the extension's types.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// StoreExtensionFactory builds an opaque store bundle from the app-side
// and admin-side handles for one construction (one dialect, one tx or
// the non-tx aggregate). The returned value is stored as-is on
// TxStores/Stores and retrieved by the registering package through a
// typed accessor; core treats it as an opaque any and never inspects it.
//
// app carries the RLS-scoped pool/tx (Postgres WithTx has set the JWT
// claims before the factory runs); admin carries the BYPASSRLS pool for
// the by-design claims-less reads (login-time provider lookups, etc.).
// In SQLite both collapse to the single connection/tx, exactly as core's
// own stores receive them.
type StoreExtensionFactory func(app, admin Execer) any

var (
	storeExtMu        sync.RWMutex
	storeExtFactories = map[string]map[string]StoreExtensionFactory{}
)

// RegisterStoreExtension registers a factory under (dialect, key). Called
// from an extension package's init() — blank-imported by package main —
// before any store is constructed. dialect is "postgres" or "sqlite";
// key namespaces the bundle (e.g. "sso"). Re-registering the same
// (dialect, key) overwrites, which keeps tests that re-init deterministic.
func RegisterStoreExtension(dialect, key string, f StoreExtensionFactory) {
	storeExtMu.Lock()
	defer storeExtMu.Unlock()
	if storeExtFactories[dialect] == nil {
		storeExtFactories[dialect] = map[string]StoreExtensionFactory{}
	}
	storeExtFactories[dialect][key] = f
}

// BuildStoreExtensions runs every factory registered for dialect against
// (app, admin) and returns the keyed bundles. Each backend's store + tx
// builders call it with the SAME handles they pass their own stores, so
// an extension's pool split is identical to core's — no RLS posture is
// weakened by moving a store out of core. Returns nil when no factory is
// registered for the dialect (a community build with no Enterprise
// Edition linked in); callers store the nil map and the typed accessors
// return their zero value.
func BuildStoreExtensions(dialect string, app, admin Execer) map[string]any {
	storeExtMu.RLock()
	defer storeExtMu.RUnlock()
	fs := storeExtFactories[dialect]
	if len(fs) == 0 {
		return nil
	}
	out := make(map[string]any, len(fs))
	for key, f := range fs {
		out[key] = f(app, admin)
	}
	return out
}
