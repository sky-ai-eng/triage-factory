package db

import "database/sql"

// Tiny SQL-binding helpers used by the package-`db` raw-function files
// that haven't been migrated to per-resource stores yet (curator,
// projects, repos, ...). When those move behind their
// own AgentStore/etc. interfaces this file goes away — the per-
// backend impl files own their own copies of these helpers there.
//
// Lived on internal/db/agent.go pre-SKY-285; lifted here when the
// AgentRunStore migration retired that file's raw functions.

// nullIfEmpty maps an empty string to a SQL NULL bind. Non-empty
// strings pass through.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullStr produces a sql.NullString that is Valid only when s is
// non-empty. Mirrors nullIfEmpty for use sites that need the typed
// NullString wrapper (e.g. INSERT bindings where the column type
// requires it).
func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullInt produces a sql.NullInt64 from an *int — nil pointer maps
// to a NULL bind, non-nil to the int's value.
func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
