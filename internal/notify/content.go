package notify

import (
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Content is what Render produces for one recipient. Title and Body are plain
// text, never markup: they can carry externally authored names.
type Content struct {
	Title   string
	Body    string
	Actions []Action
}

// Action is one of a row's actions; the first is the primary one.
//
// TODO(TFAC-1242): Link and Invoke construct it, and the kinds conformance
// test gains its action checks with them; until then no kind renders one.
type Action struct {
	id, typ, label string
}

func (a Action) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Label string `json:"label"`
	}{a.id, a.typ, a.label})
}

// Example is a kind's Example rendered for one recipient.
type Example struct {
	Recipient Recipient
	Priority  Priority
	Content   Content
}

// Examples renders k's Example for every recipient its audience can produce in
// multi mode, and for local mode's one user.
func Examples(k Kind) []Example {
	c := k.core()
	recipients := appendNew(c.audience.examples(c.example),
		Recipient{Scope: domain.NotificationScopeUser, UserID: runmode.LocalDefaultUserID})
	out := make([]Example, len(recipients))
	for i, to := range recipients {
		out[i] = Example{Recipient: to, Priority: c.priority.at(c.example), Content: c.render(c.example, to)}
	}
	return out
}
