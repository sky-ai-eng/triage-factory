package server

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// Every id list on a Jira or Linear predicate is checked on save, against the
// team's tracked set or a provider catalog; an id list in neither would store
// whatever a caller sent.
func TestPredicateCatalog_EveryIDListIsChecked(t *testing.T) {
	trackedSet := map[string]bool{
		"jira_project_id_in": true, "old_jira_project_id_in": true,
		"linear_team_id_in": true, "old_linear_team_id_in": true,
	}
	for eventType, schema := range events.All() {
		if !strings.HasPrefix(eventType, "jira:") && !strings.HasPrefix(eventType, "linear:") {
			continue
		}
		source, _, _ := strings.Cut(eventType, ":")
		for _, f := range schema.Fields {
			if !strings.HasSuffix(f.Name, "_id_in") || trackedSet[f.Name] {
				continue
			}
			if catalogFor(source, f.Name) == nil {
				t.Errorf("%s.%s is an id list no catalog check covers", eventType, f.Name)
			}
		}
	}
}
