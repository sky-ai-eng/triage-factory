package events

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// coreRegisteredSources are the event sources whose schemas register from
// this package's own init()s (github.go, jira.go, linear.go, system.go) — the only
// ones TestAllDomainEventTypesRegistered can verify. An out-of-core source
// (an ee package's own init(), e.g. ee/slack for "slack") registers its
// schema only when ee/ is actually linked in, which this package must never
// do (core never imports ee/). The equivalent completeness check for those
// lives at the composition root (package main, which does link ee/ via
// blank imports) — same precedent as feature_parity_test.go's
// TestRegisteredFeaturesAreDeclared.
var coreRegisteredSources = map[string]bool{"github": true, "jira": true, "linear": true, "system": true}

// TestAllDomainEventTypesRegistered asserts every core-sourced event type
// seeded into events_catalog has a matching schema registered. Catches the
// footgun of adding a new event ID in domain/event.go without the matching
// Go struct.
func TestAllDomainEventTypesRegistered(t *testing.T) {
	all := All()
	for _, et := range domain.AllEventTypes() {
		if !coreRegisteredSources[et.Source] {
			continue
		}
		if _, ok := all[et.ID]; !ok {
			t.Errorf("event type %q is in domain.AllEventTypes() but not registered in events package", et.ID)
		}
	}
}

// TestReset_RemovesRegistration pins the test-cleanup helper: after Reset,
// the type is unregistered (Get reports ok=false) and can be re-registered
// without tripping Register's duplicate panic.
func TestReset_RemovesRegistration(t *testing.T) {
	const eventType = "fake:reset_test:thing"

	Register(EventSchema{
		EventType: eventType,
		Ownership: OwnershipPool,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
	if _, ok := Get(eventType); !ok {
		t.Fatal("expected the type to be registered")
	}

	Reset(eventType)
	if _, ok := Get(eventType); ok {
		t.Error("expected Reset to remove the registration")
	}

	// Must not panic — Reset cleared the slot.
	Register(EventSchema{
		EventType: eventType,
		Ownership: OwnershipPool,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
	t.Cleanup(func() { Reset(eventType) })
}

// TestRegistryFieldSchemaGeneration asserts the reflect-based FieldSchema
// derivation handles pointer/bool/string/slice kinds correctly for a
// realistic predicate.
func TestRegistryFieldSchemaGeneration(t *testing.T) {
	s, ok := Get(domain.EventGitHubPRLabelAdded)
	if !ok {
		t.Fatalf("label_added schema not registered")
	}
	want := map[string]string{
		"author_in":  "string_list",
		"author":     "string",
		"label_name": "string",
		"repo_in":    "string_list",
		"is_draft":   "bool",
		"has_label":  "string",
	}
	got := map[string]string{}
	for _, f := range s.Fields {
		got[f.Name] = f.Type
	}
	if len(got) != len(want) {
		t.Errorf("field count mismatch: got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q: got type %q, want %q", k, got[k], v)
		}
	}
}

// TestIssueLabelEventFields pins the predicate fields of the Jira and Linear
// label events. Every one is a string or an identity list, so the predicate
// editor renders them with the field types it already has; the editor picks
// the identity kind of assignee_in from the event's source.
func TestIssueLabelEventFields(t *testing.T) {
	jira := map[string]string{
		"assignee_in": "string_list",
		"project":     "string",
		"label_name":  "string",
		"has_label":   "string",
	}
	linear := map[string]string{
		"assignee_in":       "string_list",
		"linear_team_id_in": "string_list",
		"label_name":        "string",
		"label_group":       "string",
		"has_label":         "string",
	}
	for eventType, want := range map[string]map[string]string{
		domain.EventJiraIssueLabelAdded:     jira,
		domain.EventJiraIssueLabelRemoved:   jira,
		domain.EventLinearIssueLabelAdded:   linear,
		domain.EventLinearIssueLabelRemoved: linear,
	} {
		s, ok := Get(eventType)
		if !ok {
			t.Errorf("%s is not registered", eventType)
			continue
		}
		got := map[string]string{}
		for _, f := range s.Fields {
			got[f.Name] = f.Type
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s fields = %v, want %v", eventType, got, want)
		}
		if s.Ownership != OwnershipOwned {
			t.Errorf("%s ownership = %v, want owned, like every Jira and Linear issue event but available", eventType, s.Ownership)
		}
	}
}

// TestMatcherAppliesPredicate runs the type-erased matcher for a couple of
// realistic scenarios. Exercises the full round-trip: JSON in → decode →
// Matches() → bool out.
func TestMatcherAppliesPredicate(t *testing.T) {
	s, _ := Get(domain.EventGitHubPRCICheckFailed)

	meta := GitHubPRCICheckFailedMetadata{
		Author:       "aidan",
		CheckName:    "test",
		Repo:         "sky-ai-eng/triage-factory",
		RepositoryID: "repo-tf",
		Labels:       []string{"wip", "self-review"},
	}
	metaJSON, _ := json.Marshal(meta)

	cases := []struct {
		name      string
		predicate string
		want      bool
	}{
		{"empty predicate matches all", "", true},
		{"author_in matches when author is in list", `{"author_in":["aidan"]}`, true},
		{"author_in matches case-insensitively", `{"author_in":["AIDAN"]}`, true},
		{"author_in rejects when author not in list", `{"author_in":["someone-else"]}`, false},
		{"empty author_in is no-filter (matches)", `{"author_in":[]}`, true},
		{"author exact-match hits", `{"author":"aidan"}`, true},
		{"author exact-match misses", `{"author":"renovate[bot]"}`, false},
		{"has_label hits when label present", `{"has_label":"self-review"}`, true},
		{"has_label misses when absent", `{"has_label":"urgent"}`, false},
		{"multi-field AND: all pass", `{"author_in":["aidan"],"check_name":"test"}`, true},
		{"multi-field AND: one fails", `{"author_in":["aidan"],"check_name":"build"}`, false},
		{"repo_in hits the event's repository id", `{"repo_in":["repo-other","repo-tf"]}`, true},
		{"repo_in misses another repository id", `{"repo_in":["repo-other"]}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Match(tc.predicate, string(metaJSON))
			if err != nil {
				t.Fatalf("Match error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// TestMatcherRefusesUnknownPredicateField pins that a stored predicate naming a
// field its type does not have errors instead of decoding to match-all: a rule
// written as {"repo": ...} must not widen to every repository.
func TestMatcherRefusesUnknownPredicateField(t *testing.T) {
	s, _ := Get(domain.EventGitHubPRCICheckFailed)
	metaJSON, _ := json.Marshal(GitHubPRCICheckFailedMetadata{Repo: "sky-ai-eng/other"})
	matched, err := s.Match(`{"repo":"sky-ai-eng/triage-factory"}`, string(metaJSON))
	if err == nil {
		t.Fatalf("Match = %v, nil; want an unknown-field error", matched)
	}
	if matched {
		t.Error("Match reported a match alongside its error")
	}
}

// TestRegister_PanicsOnNonGithubRequestedParty pins the boot-time guard: only
// github's native review-request resolver supports OwnershipRequestedParty,
// so a schema for a different source declaring it must fail loudly at
// registration rather than silently downgrading at dispatch time.
func TestRegister_PanicsOnNonGithubRequestedParty(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for a non-github event type declaring OwnershipRequestedParty")
		}
	}()
	Register(EventSchema{
		EventType: "fake:requested_party_bad",
		Ownership: OwnershipRequestedParty,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
}

// TestValidatePredicateJSON covers the happy path, unknown-field rejection,
// and the empty / {} / null → match-all normalization.
func TestValidatePredicateJSON(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		input     string
		wantOut   string
		wantErr   bool
	}{
		{"empty → match-all", domain.EventGitHubPRNewCommits, "", "", false},
		{"{} → match-all", domain.EventGitHubPRNewCommits, "{}", "", false},
		{"null → match-all", domain.EventGitHubPRNewCommits, "null", "", false},
		{"canonical round-trip", domain.EventGitHubPRNewCommits, `{"author_in":["aidan"]}`, `{"author_in":["aidan"]}`, false},
		{"unknown field rejected", domain.EventGitHubPRNewCommits, `{"bogus_field":true}`, "", true},
		{"deprecated author_is_self rejected", domain.EventGitHubPRNewCommits, `{"author_is_self":true}`, "", true},
		{"wrong type rejected", domain.EventGitHubPRNewCommits, `{"author_in":"nope"}`, "", true},
		{"unknown event type rejected", "github:pr:does_not_exist", `{"author_in":["aidan"]}`, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidatePredicateJSON(tc.eventType, tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantOut {
				t.Errorf("got %q, want %q", got, tc.wantOut)
			}
		})
	}
}
