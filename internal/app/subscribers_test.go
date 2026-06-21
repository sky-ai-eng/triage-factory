package app

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestPollEventProfilesGitHub pins the "profiler" subscriber's gate: a
// github poll-complete sentinel schedules a profiling pass for its org; a
// jira completion, a non-poll event, and malformed metadata do not.
func TestPollEventProfilesGitHub(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		metadata  string
		orgID     string
		wantOrg   string
		wantOK    bool
	}{
		{
			name:      "github poll completion profiles its org",
			eventType: domain.EventSystemPollCompleted,
			metadata:  `{"source":"github","entities":3}`,
			orgID:     "org-a",
			wantOrg:   "org-a",
			wantOK:    true,
		},
		{
			name:      "jira poll completion does not profile",
			eventType: domain.EventSystemPollCompleted,
			metadata:  `{"source":"jira","entities":3}`,
			orgID:     "org-a",
			wantOK:    false,
		},
		{
			name:      "non-poll event ignored",
			eventType: "github:pr:opened",
			metadata:  `{"source":"github"}`,
			orgID:     "org-a",
			wantOK:    false,
		},
		{
			name:      "malformed metadata ignored",
			eventType: domain.EventSystemPollCompleted,
			metadata:  `{not json`,
			orgID:     "org-a",
			wantOK:    false,
		},
		{
			name:      "missing source ignored",
			eventType: domain.EventSystemPollCompleted,
			metadata:  `{"entities":3}`,
			orgID:     "org-a",
			wantOK:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			org, ok := pollEventProfilesGitHub(domain.Event{
				OrgID:        tt.orgID,
				EventType:    tt.eventType,
				MetadataJSON: tt.metadata,
			})
			if ok != tt.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tt.wantOK)
			}
			if ok && org != tt.wantOrg {
				t.Errorf("org = %q; want %q", org, tt.wantOrg)
			}
		})
	}
}
