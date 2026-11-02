package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// noLongerApplies is why a row resolved when the condition no longer reaches
// its audience, for a kind with nothing more particular to say: a team
// archived, a role changed, or an action that took the audience out of it.
func noLongerApplies(to notify.Recipient) string {
	switch to.Scope {
	case domain.NotificationScopeTeam:
		return "This no longer applies to this team."
	case domain.NotificationScopeUser:
		return "This no longer applies to you."
	}
	return "This no longer applies to the organization's admins."
}

// sourceOff is why a row resolved when its source was turned off or its
// credential disconnected, so nothing checks the condition any more.
func sourceOff(source string) string {
	return sourceName(source) + " was turned off or disconnected, so Triage Factory stopped checking this."
}

// sourceName is an event source's name as copy spells it.
func sourceName(source string) string {
	switch source {
	case domain.ProviderGitHub:
		return "GitHub"
	case domain.ProviderJira:
		return "Jira"
	case domain.ProviderLinear:
		return "Linear"
	}
	return "Its source"
}
