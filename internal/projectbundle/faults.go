package projectbundle

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// The import path fails for two reasons that are not the server's, and both
// used to arrive at the handler as an undifferentiated error — answered 500,
// which tells the uploader nothing they can act on.
var (
	// ErrBadBundle marks a failure that is a property of the uploaded file:
	// a zip that won't open, an entry whose path escapes the tree, a manifest
	// that won't parse, an entry past the extraction budget. Wrapped at each
	// site so IsClientFault can recognize it however deep it surfaced from.
	ErrBadBundle = errors.New("invalid bundle")

	// ErrPermissionDenied marks the one import step that can fail on
	// authorization rather than on data: merging the bundle's pinned repos
	// into the team's tracked set is an org-wide write that requires
	// team-admin in multi mode, and RLS refuses it for anyone else.
	ErrPermissionDenied = errors.New("permission denied")
)

// IsClientFault reports whether err is the caller's to fix — a malformed
// bundle in any of its forms — as opposed to a server-side failure. The three
// arms are the three ways a bad bundle announces itself: the wrapped sentinel,
// the missing-manifest sentinel, and the typed unsupported-version error.
func IsClientFault(err error) bool {
	var unsupported *UnsupportedFormatError
	return errors.Is(err, ErrBadBundle) ||
		errors.Is(err, ErrManifestMissing) ||
		errors.As(err, &unsupported)
}

// isPermissionDenial reports whether a store error is the database refusing
// the write on authorization grounds rather than failing it. Postgres answers
// an RLS-refused INSERT/UPDATE with SQLSTATE 42501, so the check is on the
// code and not on the message text — wording varies by server version and
// locale, and "permission denied" appears in errors that are not this one.
// SQLite (local, N=1, no RLS) never produces a PgError, so the check is inert
// there rather than wrong.
func isPermissionDenial(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrCodeInsufficientPrivilege
}

// pgErrCodeInsufficientPrivilege is SQLSTATE 42501, which Postgres raises when
// a row-level security policy refuses a write.
const pgErrCodeInsufficientPrivilege = "42501"
