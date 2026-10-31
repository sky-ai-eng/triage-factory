package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// Event renders what a notify rule raises, one rendering for every event type
// in the catalog: the type's label and the entity's key as the title, the
// entity's title and the rule as the body, and a link to the entity on its
// source. A personal row also links to its rule, which is how the person stops
// being told. No type has copy of its own yet; the entity is where the reader
// acts on any of them.
var Event = notify.Event(notify.EventSpec{
	Render: func(p notify.EventParams, to notify.Recipient) notify.Content {
		subject := p.EntityKey
		if subject == "" {
			subject = "an issue or pull request"
		}
		rule := `From the team rule "` + p.RuleName + `".`
		if !p.TeamRule {
			rule = `From your rule "` + p.RuleName + `".`
		}
		body := rule
		if p.EntityTitle != "" {
			body = p.EntityTitle + "\n" + rule
		}
		actions := []notify.Action{notify.Link(openEntityLabel(p.EntitySource), notify.External(p.EntityURL))}
		if !p.TeamRule && to.Scope == domain.NotificationScopeUser {
			actions = append(actions, notify.Link("Edit your rule", notify.Routes.NotifyRule(p.HandlerID)))
		}
		return notify.Content{Title: catalogLabel(p.EventType) + ": " + subject, Body: body, Actions: actions}
	},
	Example: notify.EventParams{
		EventID:      "00000000-0000-0000-0000-000000000001",
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     "00000000-0000-0000-0000-000000000002",
		HandlerID:    "00000000-0000-0000-0000-000000000003",
		RuleName:     "CI failed on my PR",
		EntitySource: domain.ProviderGitHub,
		EntityKey:    "acme/widgets#18",
		EntityTitle:  "Retry the webhook on a 502",
		EntityURL:    "https://github.com/acme/widgets/pull/18",
	},
})

// catalogLabel is an event type's catalog label, or the type itself for one
// the catalog does not list.
func catalogLabel(eventType string) string {
	for _, et := range domain.AllEventTypes() {
		if et.ID == eventType {
			return et.Label
		}
	}
	return eventType
}

// openEntityLabel names where an entity's link goes.
func openEntityLabel(source string) string {
	switch source {
	case domain.ProviderGitHub:
		return "Open on GitHub"
	case domain.ProviderJira:
		return "Open in Jira"
	case domain.ProviderLinear:
		return "Open in Linear"
	}
	return "Open"
}
