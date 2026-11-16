package kinds

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// TestTriggerRefused_ASpendCapCarriesNoFigures renders the notice for each
// spend cap with the cap's own message handed in as Detail, and checks that
// the copy names the cap and none of the day's figures: spend is an org
// admin's read, and the notice goes to a team's admins.
func TestTriggerRefused_ASpendCapCarriesNoFigures(t *testing.T) {
	base := TriggerRefusal{
		TriggerID: "00000000-0000-0000-0000-000000000004",
		TeamID:    "00000000-0000-0000-0000-000000000003",
		EventType: domain.EventGitHubPRCICheckFailed,
		Subject:   "acme/widgets#18",
		Cause:     domain.DelegationRefusedSpendCap,
	}
	org := base
	org.Detail = "daily spend cap reached: org at $12.34 of $10.00 today"
	team := base
	team.TeamCap, team.CapTeamName = true, "Platform"
	team.Detail = `daily spend cap reached: team "Platform" at $12.34 of $10.00 today`

	for _, tc := range []struct {
		name  string
		r     TriggerRefusal
		names string
	}{
		{"org", org, "The organization's daily spend cap is reached."},
		{"team", team, "Team Platform's daily spend cap is reached."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := renderTriggerRefused(tc.r, notify.Recipient{})
			text := c.Title + "\n" + c.Body
			if !strings.Contains(c.Body, tc.names) {
				t.Errorf("body %q does not name the cap with %q", c.Body, tc.names)
			}
			for _, figure := range []string{"$", "12.34", "10.00"} {
				if strings.Contains(text, figure) {
					t.Errorf("copy carries %q: %q", figure, text)
				}
			}
		})
	}
}
