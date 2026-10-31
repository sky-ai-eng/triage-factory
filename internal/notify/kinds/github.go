package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// GitHubApp is the org's own GitHub App that GitHub no longer accepts.
type GitHubApp struct {
	AppID  string                         `json:"app_id"`
	Slug   string                         `json:"slug"` // display only
	Reason domain.GitHubAppUnusableReason `json:"reason"`
}

// GitHubAppUnusable is open while GitHub refuses the org's active App, which
// stops the org's polling. It is not dismissible because GitHub is required.
// The key is the App id, so a replacement App is a different occurrence.
var GitHubAppUnusable = notify.Condition(notify.ConditionSpec[GitHubApp]{
	Name:        "github.app_unusable",
	Source:      "github",
	Priority:    notify.Always(notify.Urgent),
	Dismissible: false,
	Key:         func(a GitHubApp) string { return a.AppID },
	Audience:    notify.OrgAdmins(),
	Render: func(a GitHubApp, _ notify.Recipient) notify.Content {
		app := "organization's GitHub App"
		if a.Slug != "" {
			app = a.Slug + " App"
		}
		c := notify.Content{
			Title: "GitHub no longer accepts the " + app,
			Body: "Nothing is being polled and agents can't reach GitHub. " +
				"Remove the App in GitHub access settings and connect GitHub again.",
			Actions: []notify.Action{notify.Link("Open GitHub access settings", notify.Routes.OrgSettings())},
		}
		switch a.Reason {
		case domain.GitHubAppMissing:
			c.Title = "The " + app + " no longer exists on GitHub"
			c.Body = "It was deleted on GitHub, outside Triage Factory, so nothing is being polled " +
				"and agents can't reach GitHub. Remove it in GitHub access settings, then connect GitHub again."
		case domain.GitHubAppKeyRejected:
			c.Title = "GitHub no longer accepts the private key of the " + app
			c.Body = "The key was deleted or regenerated on GitHub, outside Triage Factory, so nothing is " +
				"being polled and agents can't reach GitHub. Remove the App in GitHub access settings and " +
				"connect it again with a new private key, or connect GitHub another way."
		}
		return c
	},
	Example: GitHubApp{AppID: "123456", Slug: "acme-triage", Reason: domain.GitHubAppMissing},
})
