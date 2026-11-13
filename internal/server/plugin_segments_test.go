package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRedactedPluginSegments: a reader who is not a team admin sees whether
// the plugins loaded, their names and versions, and the bare failure code,
// and no path or load-error text.
func TestRedactedPluginSegments(t *testing.T) {
	segments := []pluginSegment{{
		ClaimID: "c1",
		Outcome: "failed",
		Error:   "plugin_not_loaded: /home/op/plugins/guard",
		PluginsLoaded: &domain.PluginsLoaded{
			OK:      false,
			Plugins: []domain.LoadedPlugin{{Name: "extra", Version: "2.0.0", Path: "/home/op/plugins/extra"}},
			Failed:  []domain.PluginFailure{{Path: "/home/op/plugins/guard", Reason: "hook-load-failed: /home/op/plugins/guard/hooks/hooks.json", Required: true}},
		},
	}, {ClaimID: "c2", Error: "something else"}}

	got := redactedPluginSegments(segments)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/home/op") {
		t.Fatalf("redacted segments still name a path: %s", b)
	}
	if got[0].Error != "plugin_not_loaded" || got[1].Error != "something else" {
		t.Errorf("errors = %q, %q; want the bare code and the other error untouched", got[0].Error, got[1].Error)
	}
	pl := got[0].PluginsLoaded
	if pl.OK || len(pl.Plugins) != 1 || pl.Plugins[0].Name != "extra" || pl.Plugins[0].Version != "2.0.0" ||
		len(pl.Failed) != 1 || !pl.Failed[0].Required {
		t.Errorf("redacted report = %+v, want ok, names, versions and required flags kept", pl)
	}
	if segments[0].PluginsLoaded.Plugins[0].Path == "" {
		t.Error("redaction changed the caller's segments")
	}
}
