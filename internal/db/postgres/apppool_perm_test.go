package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// fieldError is the exact error a multi-mode executor's sampler and reaper got
// back from Postgres before sandbox_stats had its tf_system grant: SQLSTATE
// 42501, the object named in the primary message, and every structured
// name field empty. Reproduced verbatim (not hand-shaped to suit the parser)
// because the whole point of the fallback is that the operator's log had the
// answer in it and the wrapper printed "unknown" anyway.
func fieldError() *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     "42501",
		Message:  "permission denied for table sandbox_stats",
	}
}

func TestWrapAdminPoolPermErr_NamesTheTableFromTheMessage(t *testing.T) {
	err := wrapAdminPoolPermErr(fieldError(), "sandbox_stats.RecordBatch")
	if err == nil {
		t.Fatal("wrapAdminPoolPermErr returned nil for a 42501")
	}
	if !strings.Contains(err.Error(), "sandbox_stats") {
		t.Errorf("wrapped error does not name the table: %v", err)
	}
	if strings.Contains(err.Error(), "unknown") {
		t.Errorf("wrapped error still reports the table as unknown: %v", err)
	}
	if !strings.Contains(err.Error(), "sandbox_stats.RecordBatch") {
		t.Errorf("wrapped error dropped the callsite: %v", err)
	}
	// Callers that branch on SQLSTATE must still be able to; diagnosis never
	// replaces the error it explains.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("wrapped error no longer unwraps to the 42501 PgError: %v", err)
	}
}

func TestPermDeniedTable(t *testing.T) {
	tests := []struct {
		name  string
		pgErr *pgconn.PgError
		want  string
	}{
		{
			name:  "field_error",
			pgErr: fieldError(),
			want:  "sandbox_stats",
		},
		{
			name: "structured_field_wins_when_populated",
			pgErr: &pgconn.PgError{
				Code:      "42501",
				Message:   "permission denied for table sandbox_stats",
				TableName: "from_the_field",
			},
			want: "from_the_field",
		},
		{
			// The pre-PG12 spelling of the same message. Accepted so a server
			// version change can't quietly regress the name back to "unknown".
			name: "relation_spelling",
			pgErr: &pgconn.PgError{
				Code:    "42501",
				Message: "permission denied for relation instance_stats",
			},
			want: "instance_stats",
		},
		{
			// A denial on a non-table object: no table name exists, so the
			// parser must decline rather than report the sequence as one.
			name: "other_object_kind_declines",
			pgErr: &pgconn.PgError{
				Code:    "42501",
				Message: "permission denied for sequence messages_id_seq",
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := permDeniedTable(tc.pgErr); got != tc.want {
				t.Errorf("permDeniedTable = %q, want %q", got, tc.want)
			}
		})
	}
}

// A 42501 that names no table still has to produce a usable message — and it
// must point at where the name actually lives (the wrapped message), not at a
// DETAIL field the privilege check never populates.
func TestWrapAdminPoolPermErr_UnknownTablePointsAtTheMessage(t *testing.T) {
	err := wrapAdminPoolPermErr(&pgconn.PgError{
		Code:    "42501",
		Message: "permission denied for sequence messages_id_seq",
	}, "messages.InsertMessageSystem")
	if err == nil {
		t.Fatal("wrapAdminPoolPermErr returned nil for a 42501")
	}
	if strings.Contains(err.Error(), "DETAIL") {
		t.Errorf("wrapped error still points the reader at DETAIL: %v", err)
	}
	if !strings.Contains(err.Error(), "messages_id_seq") {
		t.Errorf("wrapped error lost the underlying message: %v", err)
	}
}

func TestWrapAdminPoolPermErr_PassesThroughNonPermissionErrors(t *testing.T) {
	if got := wrapAdminPoolPermErr(nil, "x"); got != nil {
		t.Errorf("nil error must pass through, got %v", got)
	}
	plain := fmt.Errorf("connection reset")
	if got := wrapAdminPoolPermErr(plain, "x"); !errors.Is(got, plain) {
		t.Errorf("non-pg error must pass through untouched, got %v", got)
	}
	notFound := &pgconn.PgError{Code: "42P01", Message: `relation "nope" does not exist`}
	got := wrapAdminPoolPermErr(notFound, "x")
	if !errors.Is(got, notFound) || strings.Contains(got.Error(), "Triage Factory bug") {
		t.Errorf("a non-42501 pg error must pass through untouched, got %v", got)
	}
}
