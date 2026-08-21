package slack

import (
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAvailability_ThreeWaySplit walks what an org is told about Slack, and
// the point is that the three answers are DIFFERENT: a missing licence is not
// the reader's to fix, a missing workspace is exactly theirs, and a connected
// one is available. Collapsing any two would send someone to the wrong place.
func TestAvailability_ThreeWaySplit(t *testing.T) {
	cases := map[string]struct {
		licensed  bool
		connected bool
		want      eventsource.State
	}{
		"no licence":                 {licensed: false, connected: false, want: eventsource.StateUnlicensed},
		"no licence, but connected":  {licensed: false, connected: true, want: eventsource.StateUnlicensed},
		"licensed, no workspace":     {licensed: true, connected: false, want: eventsource.StateUnconfigured},
		"licensed and workspace set": {licensed: true, connected: true, want: eventsource.StateAvailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			if tc.licensed {
				entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
			} else {
				entitlements.RegisterProvider(entitlements.Static())
			}
			t.Cleanup(entitlements.Reset)

			h := pgtest.Shared(t)
			h.Reset(t)
			stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
			orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "slack-availability")

			if tc.connected {
				if err := stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
					return slackstore.FromTx(tx).Workspaces.Upsert(t.Context(), slackstore.Workspace{
						WorkspaceID:        "T0AVAIL",
						APIAppID:           "A0AVAIL",
						OrgID:              orgID,
						WorkspaceName:      "Acme",
						Transport:          "events_api",
						BotUserID:          "U0BOT",
						RegisteredByUserID: owner,
					})
				}); err != nil {
					t.Fatalf("connect workspace: %v", err)
				}
			}

			var got eventsource.State
			if err := stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
				var e error
				got, e = availability(t.Context(), tx, orgID)
				return e
			}); err != nil {
				t.Fatalf("resolve availability: %v", err)
			}
			if got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAvailability_KeysOnTheRowNotLiveSocketHealth: the workspace above is an
// Events API app, which has no Socket Mode connection to report and none
// before the reconciler's first pass either. Keying availability on live
// connection status would report a perfectly healthy webhook workspace as
// unconfigured, so the row is the whole test.
func TestAvailability_KeysOnTheRowNotLiveSocketHealth(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "slack-webhook-only")

	if err := stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(t.Context(), slackstore.Workspace{
			WorkspaceID: "T0HOOK", APIAppID: "A0HOOK", OrgID: orgID,
			WorkspaceName: "Acme", Transport: "events_api", BotUserID: "U0BOT",
			RegisteredByUserID: owner,
		})
	}); err != nil {
		t.Fatalf("connect workspace: %v", err)
	}

	// No socketManager anywhere in this test: nothing is dialing Slack, so a
	// connection-derived answer could only be "unknown".
	var got eventsource.State
	if err := stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		got, e = availability(t.Context(), tx, orgID)
		return e
	}); err != nil {
		t.Fatalf("resolve availability: %v", err)
	}
	if got != eventsource.StateAvailable {
		t.Errorf("state = %q, want available", got)
	}
}

// TestAvailability_RegisteredMultiOnly pins what init() declared: Slack is
// registered under its own kind, and it is MultiOnly — the workspace store is
// Postgres-only, so local mode omits it rather than reporting it off.
func TestAvailability_RegisteredMultiOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "slack-local-omit")

	var known bool
	if err := stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		_, known, e = eventsource.StateFor(t.Context(), tx, orgID, "slack")
		return e
	}); err != nil {
		t.Fatalf("resolve availability: %v", err)
	}
	if known {
		t.Error("slack reported as a known source in local mode; a Postgres-only source is omitted there")
	}
}
