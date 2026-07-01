package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// --- blueprintGatedOff (pure unit tests) ---------------------------------
//
// The schema enforces at most one trigger per blueprint today
// (event_handlers_one_trigger_per_blueprint), so the HTTP-level tests below
// can only exercise the single-trigger cases. blueprintGatedOff is written
// generically against a []domain.EventHandler (per TFAC-524's "≥1 attached
// trigger AND every trigger gated" rule) so it stays correct if that
// cardinality ever loosens; these pure tests cover the multi-trigger cases
// directly.

func TestBlueprintGatedOff_NoTriggers_NeverGated(t *testing.T) {
	if blueprintGatedOff("any-org", nil) {
		t.Error("a trigger-less blueprint must never be reported as gated off")
	}
}

func TestBlueprintGatedOff_SingleGatedTrigger(t *testing.T) {
	gateJiraForTest(t)
	triggers := []domain.EventHandler{{EventType: domain.EventJiraIssueAssigned}}
	if !blueprintGatedOff("any-org", triggers) {
		t.Error("a blueprint whose sole trigger is gated off must be reported as gated off")
	}
}

func TestBlueprintGatedOff_SingleUngatedTrigger(t *testing.T) {
	gateJiraForTest(t)
	triggers := []domain.EventHandler{{EventType: domain.EventGitHubPROpened}}
	if blueprintGatedOff("any-org", triggers) {
		t.Error("a blueprint whose sole trigger is ungated must not be reported as gated off")
	}
}

func TestBlueprintGatedOff_MixedTriggers_OneAllowedKeepsItVisible(t *testing.T) {
	gateJiraForTest(t)
	triggers := []domain.EventHandler{
		{EventType: domain.EventJiraIssueAssigned}, // gated
		{EventType: domain.EventGitHubPROpened},    // allowed
	}
	if blueprintGatedOff("any-org", triggers) {
		t.Error("a blueprint with at least one allowed trigger must not be gated off, even if another trigger is gated")
	}
}

func TestBlueprintGatedOff_AllTriggersGated(t *testing.T) {
	gateJiraForTest(t)
	triggers := []domain.EventHandler{
		{EventType: domain.EventJiraIssueAssigned},
		{EventType: domain.EventJiraIssueCompleted},
	}
	if !blueprintGatedOff("any-org", triggers) {
		t.Error("a blueprint whose every trigger is gated off must be reported as gated off")
	}
}

func TestBlueprintGatedOff_FeatureGranted_NoLongerGated(t *testing.T) {
	gateJiraForTest(t)
	grantTestGateFeature(t)
	triggers := []domain.EventHandler{{EventType: domain.EventJiraIssueAssigned}}
	if blueprintGatedOff("any-org", triggers) {
		t.Error("once the org holds the gating feature, a blueprint's jira trigger must not be reported as gated off")
	}
}

// --- GET /api/blueprints list filter (HTTP-level, single-trigger reality) ---

func TestHandleBlueprintsList_HidesBlueprintWithOnlyGatedTrigger(t *testing.T) {
	s := newTestServer(t)
	gatedBP := createBareBlueprint(t, s, "GatedOnly")
	attachTrigger(t, s, gatedBP, domain.EventJiraIssueAssigned)
	ungatedBP := createBareBlueprint(t, s, "Ungated")
	attachTrigger(t, s, ungatedBP, domain.EventGitHubPROpened)
	triggerlessBP := createBareBlueprint(t, s, "TriggerLess")

	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodGet, "/api/blueprints", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	seen := map[string]bool{}
	for _, bp := range got {
		seen[bp["id"].(string)] = true
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

	rec := doJSON(t, s, http.MethodGet, "/api/blueprints", nil)
	var got []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	var seen bool
	for _, bp := range got {
		if bp["id"] == gatedBP {
			seen = true
		}
	}
	if !seen {
		t.Error("gated blueprint should reappear once the org holds the gating feature")
	}
}
