package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// Event renders what a notify rule raises, one rendering for every event type
// in the catalog: the type's label and the entity's key as the title, the
// entity's title and the rule as the body, a link to the entity on its
// source, and a link to the rule: the person's own, which is how they stop
// being told, or the team's. The rule link is also what a row keeps when the
// entity's link cannot be shown. No type has copy of its own yet; the entity
// is where the reader acts on any of them.
var Event = notify.Event(notify.EventSpec{
	Render: func(p notify.EventParams, to notify.Recipient) notify.Content {
		subject := p.EntityKey
		if subject == "" {
			subject = "an issue or pull request"
		}
		rule := `From the team rule "` + p.RuleName + `".`
		ruleLink := notify.Link("Open the team rule", notify.Routes.NotifyRule(p.HandlerID))
		if !p.TeamRule && to.Scope == domain.NotificationScopeUser {
			rule = `From your rule "` + p.RuleName + `".`
			ruleLink = notify.Link("Edit your rule", notify.Routes.NotifyRule(p.HandlerID))
		}
		body := rule
		if p.EntityTitle != "" {
			body = p.EntityTitle + "\n" + rule
		}
		label, ok := catalogLabel(p.EventType)
		if !ok {
			label = p.EventType
		}
		return notify.Content{
			Title:   label + ": " + subject,
			Body:    body,
			Actions: []notify.Action{notify.Link(openEntityLabel(p.EntitySource), notify.External(p.EntityURL)), ruleLink},
		}
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

// catalogLabel is an event type's label in the catalog, and whether the
// catalog lists the type.
func catalogLabel(eventType string) (string, bool) {
	for _, et := range domain.AllEventTypes() {
		if et.ID == eventType {
			return et.Label, true
		}
	}
	return "", false
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
