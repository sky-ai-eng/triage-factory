package domain

import (
	"strings"
	"testing"
)

// TestAdditiveEventInjection_NamesTypeAndMetadata: the copy names the event
// type and includes the metadata JSON, plus tells the agent it won't spawn a
// separate run.
func TestAdditiveEventInjection_NamesTypeAndMetadata(t *testing.T) {
	injection := AdditiveEventInjection("slack:mention", `{"channel":"C123","ts":"1.2"}`)
	for _, want := range []string{"slack:mention", `"channel":"C123"`, "will not spawn a separate run"} {
		if !strings.Contains(injection, want) {
			t.Errorf("injection missing %q: %q", want, injection)
		}
	}
}

// TestAdditiveEventInjection_EmptyMetadata_StillNamesType: a best-effort
// metadata lookup failure degrades to a body naming the event type alone,
// never an empty/blank injection.
func TestAdditiveEventInjection_EmptyMetadata_StillNamesType(t *testing.T) {
	injection := AdditiveEventInjection("github:pr:ci_check_failed", "")
	if injection == "" {
		t.Fatal("injection is empty")
	}
	if !strings.Contains(injection, "github:pr:ci_check_failed") {
		t.Errorf("injection missing event type: %q", injection)
	}
	if strings.Contains(injection, "Event metadata:") {
		t.Errorf("empty metadata should omit the metadata section: %q", injection)
	}
}
