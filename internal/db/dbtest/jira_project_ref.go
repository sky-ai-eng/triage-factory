package dbtest

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// TestJiraProject is the Jira project the suites create Jira issue entities in
// when a test needs no particular one (db.EntityStore.FindOrCreateJiraSystem
// registers it on the entity's site).
var TestJiraProject = domain.JiraProjectRef{ID: "10000", Key: "SKY", Name: "Sky"}
