package memoryentities

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TestSkipReason: a resolve that answers with no entity, or that refuses to
// create a Linear or Jira entity without its team or project, is a missing
// entity; anything else is an error.
func TestSkipReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, skipNoEntity},
		{fmt.Errorf("create: %w", db.ErrEntityJiraProjectRequired), skipNoEntity},
		{fmt.Errorf("create: %w", db.ErrEntityLinearTeamRequired), skipNoEntity},
		{errors.New("connection reset"), skipError},
	} {
		if got := skipReason(tc.err); got != tc.want {
			t.Errorf("skipReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
