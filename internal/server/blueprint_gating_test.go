package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// --- the blueprint list filter (HTTP-level, single-trigger reality) ---

func TestHandleBlueprintsList_HidesBlueprintWithOnlyGatedTrigger(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	gatedBP := createBareBlueprint(t, s, "GatedOnly")
	attachTrigger(t, s, gatedBP, domain.EventJiraIssueAssigned)
	ungatedBP := createBareBlueprint(t, s, "Ungated")
	attachTrigger(t, s, ungatedBP, domain.EventGitHubPROpened)
	triggerlessBP := createBareBlueprint(t, s, "TriggerLess")

	gateJiraForTest(t)

	page := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/blueprints/list", map[string]any{}))
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
	configureEventSources(t, s)
	gatedBP := createBareBlueprint(t, s, "GatedOnly")
	attachTrigger(t, s, gatedBP, domain.EventJiraIssueAssigned)

	gateJiraForTest(t)
	grantTestGateFeature(t)

	page := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/blueprints/list", map[string]any{}))
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
