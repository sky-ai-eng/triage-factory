package entitlements

import "testing"

func TestFeatureForEventType_UngatedSourcesReportNotGated(t *testing.T) {
	t.Cleanup(Reset)
	for _, et := range []string{"github:pr:opened", "jira:issue:assigned", "system:poll:done"} {
		if _, ok := FeatureForEventType(et); ok {
			t.Errorf("FeatureForEventType(%q) reported gated; core sources must be ungated by default", et)
		}
	}
}

func TestGateEventSource_MatchesBySourcePrefix(t *testing.T) {
	t.Cleanup(Reset)
	GateEventSource("jira", Feature("test-gate"))

	f, ok := FeatureForEventType("jira:issue:assigned")
	if !ok || f != Feature("test-gate") {
		t.Fatalf("FeatureForEventType(jira:issue:assigned) = (%q, %v), want (test-gate, true)", f, ok)
	}
	// A different source is untouched.
	if _, ok := FeatureForEventType("github:pr:opened"); ok {
		t.Error("gating jira must not affect github event types")
	}
}

// FeatureForEventType splits on the FIRST colon only — "jira" gates every
// jira:* type regardless of how many further segments the type carries.
func TestFeatureForEventType_SplitsOnFirstColonOnly(t *testing.T) {
	t.Cleanup(Reset)
	GateEventSource("jira", Feature("test-gate"))

	f, ok := FeatureForEventType("jira:issue:status:changed")
	if !ok || f != Feature("test-gate") {
		t.Fatalf("FeatureForEventType(jira:issue:status:changed) = (%q, %v), want (test-gate, true)", f, ok)
	}
}

// An event type with no colon at all is treated as its own (whole-string)
// source — defensive: every real event_type carries a "source:..." shape, but
// the function must not panic or misbehave on a malformed one.
func TestFeatureForEventType_NoColon_TreatsWholeStringAsSource(t *testing.T) {
	t.Cleanup(Reset)
	GateEventSource("standalone", Feature("test-gate"))

	f, ok := FeatureForEventType("standalone")
	if !ok || f != Feature("test-gate") {
		t.Fatalf("FeatureForEventType(standalone) = (%q, %v), want (test-gate, true)", f, ok)
	}
}

func TestEventTypeAllowed_UngatedAlwaysAllowed(t *testing.T) {
	t.Cleanup(Reset)
	// No provider registered (community default) and no gate — still allowed.
	if !EventTypeAllowed("any-org", "github:pr:opened") {
		t.Error("ungated event type must always be allowed")
	}
}

func TestEventTypeAllowed_GatedRequiresFeature(t *testing.T) {
	t.Cleanup(Reset)
	GateEventSource("jira", Feature("test-gate"))

	if EventTypeAllowed("org-without-feature", "jira:issue:assigned") {
		t.Error("gated event type must be disallowed when the org lacks the feature")
	}

	RegisterProvider(Static(Feature("test-gate")))
	if !EventTypeAllowed("org-with-feature", "jira:issue:assigned") {
		t.Error("gated event type must be allowed once the org holds the feature")
	}
}

func TestReset_ClearsEventSourceGates(t *testing.T) {
	GateEventSource("jira", Feature("test-gate"))
	RegisterProvider(Static(Feature("test-gate")))
	if !EventTypeAllowed("any-org", "jira:issue:assigned") {
		t.Fatal("sanity: gate should be active before Reset")
	}

	Reset()

	if !EventTypeAllowed("any-org", "jira:issue:assigned") {
		t.Error("after Reset, a previously-gated source must be ungated (no leakage into sibling tests)")
	}
	if _, ok := FeatureForEventType("jira:issue:assigned"); ok {
		t.Error("after Reset, FeatureForEventType must report ungated")
	}
}
