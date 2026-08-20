package server

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// --- gatedEventTypes (pure unit test) ------------------------------------
//
// The visibility rule itself — "hide a blueprint iff it has ≥1 trigger and
// every one of them fires on a gated type" — now lives in the store's query,
// where it can filter and count the same rows. What stays here is the handler's
// half: deriving WHICH event types this org is not entitled to.

func TestGatedEventTypes_ListsOnlyUnlicensedTypes(t *testing.T) {
	gateJiraForTest(t)
	gated := gatedEventTypes("any-org")
	if !slices.Contains(gated, domain.EventJiraIssueAssigned) {
		t.Errorf("gated = %v, want it to contain the unlicensed jira type", gated)
	}
	if slices.Contains(gated, domain.EventGitHubPROpened) {
		t.Errorf("gated = %v, want it NOT to contain an ungated github type", gated)
	}
}

func TestGatedEventTypes_FeatureGranted_IsEmptyOfThatType(t *testing.T) {
	gateJiraForTest(t)
	grantTestGateFeature(t)
	if gated := gatedEventTypes("any-org"); slices.Contains(gated, domain.EventJiraIssueAssigned) {
		t.Errorf("gated = %v, want the jira type absent once the org holds the feature", gated)
	}
}

// --- the blueprint list filter (HTTP-level, single-trigger reality) ---

func TestHandleBlueprintsList_HidesBlueprintWithOnlyGatedTrigger(t *testing.T) {
	s := newTestServer(t)
	gatedBP := createBareBlueprint(t, s, "GatedOnly")
	attachTrigger(t, s, gatedBP, domain.EventJiraIssueAssigned)
	ungatedBP := createBareBlueprint(t, s, "Ungated")
	attachTrigger(t, s, ungatedBP, domain.EventGitHubPROpened)
	triggerlessBP := createBareBlueprint(t, s, "TriggerLess")

	gateJiraForTest(t)

	page := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/blueprints/list", map[string]any{}))
	seen := map[string]bool{}
	for _, bp := range page.Items {
		seen[bp["id"].(string)] = true
	}
	// The total counts what the caller can see, not what the table holds: a
	// hidden blueprint that still showed up in the count would render as a
	// missing row on a page that claims more.
	if page.Total() != len(page.Items) {
		t.Errorf("total = %d but the page held %d rows; a gated blueprint must not be counted",
			page.Total(), len(page.Items))
	}
	if seen[gatedBP] {
		t.Error("blueprint whose only trigger is gated off must be hidden")
	}
	if !seen[ungatedBP] {
		t.Error("blueprint with an ungated trigger must stay visible")
	}
	if !seen[triggerlessBP] {
		t.Error("trigger-less blueprint must stay visible (unrelated to gating)")
	}
}

func TestHandleBlueprintsList_GatedBlueprintVisibleWithFeature(t *testing.T) {
	s := newTestServer(t)
	gatedBP := createBareBlueprint(t, s, "GatedOnly")
	attachTrigger(t, s, gatedBP, domain.EventJiraIssueAssigned)

	gateJiraForTest(t)
	grantTestGateFeature(t)

	page := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/blueprints/list", map[string]any{}))
	var seen bool
	for _, bp := range page.Items {
		if bp["id"] == gatedBP {
			seen = true
		}
	}
	if !seen {
		t.Error("gated blueprint should reappear once the org holds the gating feature")
	}
}
