package slack

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
)

// TestAvailability_MissingStoreBundleIsAFault: a transaction carrying no Slack
// store bundle is a miswired build, and the probe has to say so. Answering
// `unconfigured` would file a wiring bug under the one state that invites the
// reader to go connect a workspace — which would never help, and would look
// exactly like an org that simply has not connected one.
func TestAvailability_MissingStoreBundleIsAFault(t *testing.T) {
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	state, err := availability(t.Context(), db.TxStores{}, "00000000-0000-0000-0000-000000000001")
	if err == nil {
		t.Fatalf("state = %q with no error; a missing store bundle must fail the read", state)
	}
	if state != "" {
		t.Errorf("state = %q alongside an error; a failed probe reports no state", state)
	}
}

// The entitlement answer comes first and reads nothing, so an unlicensed org
// is told about its licence rather than about our store wiring.
func TestAvailability_UnlicensedAnswersBeforeTouchingTheStore(t *testing.T) {
	entitlements.RegisterProvider(entitlements.Static())
	t.Cleanup(entitlements.Reset)

	state, err := availability(t.Context(), db.TxStores{}, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("availability: %v", err)
	}
	if state != eventsource.StateUnlicensed {
		t.Errorf("state = %q, want %q", state, eventsource.StateUnlicensed)
	}
}

// The system probe's twin arms mirror the claims probe's, over db.Stores: a
// missing bundle is the same fault, and the entitlement still answers first
// without touching a store.
func TestAvailabilitySystem_MissingStoreBundleIsAFault(t *testing.T) {
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	state, err := availabilitySystem(t.Context(), db.Stores{}, "00000000-0000-0000-0000-000000000001")
	if err == nil {
		t.Fatalf("state = %q with no error; a missing store bundle must fail the read", state)
	}
	if state != "" {
		t.Errorf("state = %q alongside an error; a failed probe reports no state", state)
	}
}

func TestAvailabilitySystem_UnlicensedAnswersBeforeTouchingTheStore(t *testing.T) {
	entitlements.RegisterProvider(entitlements.Static())
	t.Cleanup(entitlements.Reset)

	state, err := availabilitySystem(t.Context(), db.Stores{}, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("availabilitySystem: %v", err)
	}
	if state != eventsource.StateUnlicensed {
		t.Errorf("state = %q, want %q", state, eventsource.StateUnlicensed)
	}
}
