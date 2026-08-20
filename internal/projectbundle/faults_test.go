package projectbundle

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsPermissionDenial_KeysOnSQLSTATE pins the classifier to the code rather
// than the message. The false arms are the reason: a wrapped 42501 must be
// recognized however deep it surfaced, and an unrelated error whose prose
// happens to contain "permission denied" must not be mistaken for an RLS
// refusal and answered 403.
func TestIsPermissionDenial_KeysOnSQLSTATE(t *testing.T) {
	rlsRefusal := &pgconn.PgError{
		Code:    "42501",
		Message: `new row violates row-level security policy for table "team_github_repos"`,
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rls refusal", rlsRefusal, true},
		{"rls refusal, wrapped", fmt.Errorf("track imported repos: %w", rlsRefusal), true},
		{"other pg error", &pgconn.PgError{Code: "23505", Message: "duplicate key value"}, false},
		{"prose that merely says so", errors.New("upstream said: permission denied"), false},
		{"our own sentinel is not the signal", ErrPermissionDenied, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermissionDenial(tc.err); got != tc.want {
				t.Errorf("isPermissionDenial(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDecodeManifest_ParseErrorStaysInspectable pins both halves of the
// double-wrap: the fault classifies as a bad bundle, AND the parser's own
// error survives the chain so a log or a test can name what failed to parse.
func TestDecodeManifest_ParseErrorStaysInspectable(t *testing.T) {
	_, err := decodeManifest([]byte("format_version: [unterminated\n"))
	if err == nil {
		t.Fatal("decodeManifest accepted unparseable yaml")
	}
	if !errors.Is(err, ErrBadBundle) || !IsClientFault(err) {
		t.Errorf("err = %v, want it to classify as ErrBadBundle", err)
	}
	if !hasYAMLError(err) {
		t.Errorf("err = %v, want the yaml parse error reachable through the unwrap chain", err)
	}
}

// hasYAMLError walks the unwrap tree for the parse error yaml.v3 returns for
// malformed input. It is a plain *errors.errorString, so identity is the only
// handle on it — its message is the one place the line and column appear. The
// walk has to branch: fmt.Errorf with two %w verbs yields Unwrap() []error, not
// the single-error form, so a linear chain walk stops at the first hop.
func hasYAMLError(err error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case interface{ Unwrap() []error }:
		for _, sub := range e.Unwrap() {
			if hasYAMLError(sub) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		if hasYAMLError(e.Unwrap()) {
			return true
		}
	}
	return strings.HasPrefix(err.Error(), "yaml:")
}
