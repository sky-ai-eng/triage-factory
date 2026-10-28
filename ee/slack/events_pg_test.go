package slack

// pgtest-backed suite for channelInFaults, slack:message's predicate reference
// check. It runs under a plain team member's claims, as the event-handler
// write routes do, so it also covers RLS letting a handler's author read their
// team's tracked channels.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server"
)

func TestChannelInFaults(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-in-faults")
	member := pgtest.SeedUser(t, r.h, "chan-in-faults-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")
	other := pgtest.SeedTeam(t, r.h, orgID, "chan-in-faults-other")
	otherAdmin := pgtest.SeedUser(t, r.h, "chan-in-faults-other-admin")
	pgtest.AddOrgMember(t, r.h, otherAdmin, orgID, other, "member", "admin")
	r.listChannels(orgID, owner, "C1", "C2", "C3")

	track := func(team, admin string, channelIDs ...string) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+team+"/channels", admin, orgID, team,
			map[string]any{"channel_ids": channelIDs}))
		if rec.Code != http.StatusOK {
			t.Fatalf("track %v: status = %d (body=%s)", channelIDs, rec.Code, rec.Body.String())
		}
	}
	track(teamID, owner, "C1", "C2")
	track(other, otherAdmin, "C3")
	rowID := func(channelID string) string {
		t.Helper()
		row, err := slackstore.FromStores(r.stor).Channels.ResolveSystem(t.Context(), orgID, channelID)
		if err != nil || row == nil {
			t.Fatalf("registry row for %s = %+v, %v", channelID, row, err)
		}
		return row.ID
	}
	c1, c2, c3 := rowID("C1"), rowID("C2"), rowID("C3")

	pred := func(ids ...string) *string {
		s := `{"channel_in":["` + strings.Join(ids, `","`) + `"]}`
		return &s
	}
	faults := func(predicate, prior *string) []server.PredicateReferenceFault {
		t.Helper()
		var out []server.PredicateReferenceFault
		if err := r.stor.Tx.WithTx(t.Context(), orgID, member, func(tx db.TxStores) error {
			var e error
			out, e = channelInFaults(t.Context(), tx, orgID, teamID, predicate, prior)
			return e
		}); err != nil {
			t.Fatalf("channelInFaults: %v", err)
		}
		return out
	}

	cases := []struct {
		name             string
		predicate, prior *string
		refused          []string
	}{
		{name: "match-all", predicate: nil},
		{name: "channels the team tracks", predicate: pred(c1, c2)},
		// The value a caller reaches for first: Slack's own id for a channel
		// the team does track.
		{name: "a Slack channel id", predicate: pred(c1, "C2"), refused: []string{"C2"}},
		{name: "a channel only another team tracks", predicate: pred(c3), refused: []string{c3}},
		{name: "a repeated entry is reported once", predicate: pred("C9", "C9"), refused: []string{"C9"}},
		// The row already stores c3 (as if the team had untracked it since):
		// an edit that keeps it passes, and only what it adds is checked.
		{name: "an entry the row already carries", predicate: pred(c3, c1), prior: pred(c3)},
		{name: "an entry added beside a carried one", predicate: pred(c3, "C1"), prior: pred(c3), refused: []string{"C1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := faults(tc.predicate, tc.prior)
			if len(got) != len(tc.refused) {
				t.Fatalf("faults = %q, want one each for %q", got, tc.refused)
			}
			for i, id := range tc.refused {
				if got[i].Key != "channel_in" || !strings.HasPrefix(got[i].Message, `channel_in: "`+id+`"`) {
					t.Errorf("fault %d = %+v, want it on channel_in, naming %q", i, got[i], id)
				}
			}
		})
	}
}
