package notify

import (
	"fmt"
	"net/url"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Content is what Render produces for one recipient. Title and Body are plain
// text, never markup: they can carry externally authored names. Actions holds
// at most two, built by Link and Invoke; the first is the primary one.
type Content struct {
	Title   string
	Body    string
	Actions []Action
}

// exampleOrgID is the org an Example's links render for.
const exampleOrgID = "00000000-0000-0000-0000-000000000000"

// Example is a kind's Example rendered for one recipient.
type Example struct {
	Recipient Recipient
	// Local is whether Recipient is local mode's one user, whose links render
	// to local mode's pages.
	Local    bool
	Priority Priority
	Content  Content
	// Err is why a write of this rendering would be refused: more than two
	// actions, a notice with none, a link whose route has no page in the
	// recipient's mode, or an action that is malformed or declares a
	// permission its row cannot answer. An external link's host is the org's
	// to allow, so here only its shape is checked. A Render that panics is
	// reported here too, with empty Content.
	Err error
}

// Examples renders k's Example for every recipient its audience can produce in
// multi mode, and for local mode's one user.
func Examples(k Kind) []Example {
	c := k.core()
	var out []Example
	render := func(to Recipient, local bool) {
		ex := Example{Recipient: to, Local: local, Priority: c.priority.at(c.example)}
		defer func() {
			if r := recover(); r != nil {
				ex.Content, ex.Err = Content{}, fmt.Errorf("render panicked: %v", r)
			}
			out = append(out, ex)
		}()
		ex.Content = c.render(c.example, to)
		_, ex.Err = renderActions(c.lifecycle, to, ex.Content.Actions, exampleOrgID, local, func(*url.URL) error { return nil })
	}
	for _, to := range c.audience.examples(c.example) {
		render(to, false)
	}
	render(Recipient{Scope: domain.NotificationScopeUser, UserID: runmode.LocalDefaultUserID}, true)
	return out
}
