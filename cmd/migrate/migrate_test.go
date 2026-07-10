package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TestMigrateExitCode pins the error→exit-code mapping the container
// entrypoint keys on. ExitSchemaAhead MUST stay 3 — docker/entrypoint.sh
// hardcodes that literal to fail fast on an ahead schema; changing it here
// without updating the entrypoint would silently break the fail-fast.
func TestMigrateExitCode(t *testing.T) {
	if ExitSchemaAhead != 3 {
		t.Fatalf("ExitSchemaAhead = %d, want 3 (docker/entrypoint.sh hardcodes 3)", ExitSchemaAhead)
	}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ahead is the distinct code", db.ErrExecutorSchemaAhead, ExitSchemaAhead},
		{"wrapped ahead still maps", fmt.Errorf("migrate up: %w", db.ErrExecutorSchemaAhead), ExitSchemaAhead},
		{"behind is retryable", db.ErrExecutorSchemaBehind, 1},
		{"generic error is retryable", errors.New("connection refused"), 1},
	}
	for _, tc := range cases {
		if got := migrateExitCode(tc.err); got != tc.want {
			t.Errorf("%s: migrateExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}
