package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// maxActions is how many actions a row carries; the first is the primary one.
const maxActions = 2

// Action is one of a row's actions, built by Link or Invoke.
type Action struct {
	typ      actionType
	id       string
	label    string
	href     Href
	requires Permission
	confirm  string
}

type actionType string

const (
	linkAction   actionType = "link"
	invokeAction actionType = "invoke"
)

// Link is an action that opens href. Its id is href's: the route's name, or
// "external".
func Link(label string, href Href) Action {
	return Action{typ: linkAction, id: href.id(), label: label, href: href}
}

// Invoke is an action that runs the handler the kind registers for id with
// HandleAction, for a caller who holds requires. Only a condition carries one:
// a notice's actions are links.
func Invoke(id, label string, requires Permission, opts ...InvokeOption) Action {
	a := Action{typ: invokeAction, id: id, label: label, requires: requires}
	for _, opt := range opts {
		opt(&a)
	}
	return a
}

// InvokeOption adjusts an Invoke action.
type InvokeOption func(*Action)

// Confirm has a client ask the person to confirm, in text, before it runs the
// action.
func Confirm(text string) InvokeOption {
	return func(a *Action) { a.confirm = text }
}

// Permission is what a caller must hold to run an invoke action. The action
// declares it; a row's read computes allowed from it for each viewer, and
// Caller.RunAction checks it before the handler runs, so a client offered the
// button is never answered 403.
type Permission string

const (
	// RequiresOrgAdmin is an admin of the row's org.
	RequiresOrgAdmin Permission = "org_admin"
	// RequiresTeamAdmin is an admin of the team the row is for, so only a team
	// row's action declares it.
	RequiresTeamAdmin Permission = "team_admin"
)

// actionIDPattern keeps an invoke id usable as a path segment of the route
// that runs it.
var actionIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// storedAction is an action as the row's actions column holds it. Requires is
// stored so a read computes allowed from the row alone.
type storedAction struct {
	ID       string     `json:"id"`
	Type     actionType `json:"type"`
	Label    string     `json:"label"`
	Href     string     `json:"href,omitempty"`
	Requires Permission `json:"requires,omitempty"`
	Confirm  string     `json:"confirm,omitempty"`
}

// renderActions checks the actions Render returned for one recipient and
// returns them as the row stores them, or why the write is refused.
func renderActions(lc domain.NotificationLifecycle, to Recipient, as []Action, orgID string, local bool, checkHost func(*url.URL) error) ([]storedAction, error) {
	if len(as) > maxActions {
		return nil, fmt.Errorf("%d actions; a row carries at most %d", len(as), maxActions)
	}
	if lc == domain.NotificationNotice && len(as) == 0 {
		return nil, errors.New("a notice carries an action")
	}
	out := make([]storedAction, 0, len(as))
	for _, a := range as {
		s, err := a.render(lc, to, orgID, local, checkHost)
		if err != nil {
			return nil, err
		}
		for _, prev := range out {
			if prev.ID == s.ID {
				return nil, fmt.Errorf("two actions with id %q", s.ID)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func (a Action) render(lc domain.NotificationLifecycle, to Recipient, orgID string, local bool, checkHost func(*url.URL) error) (storedAction, error) {
	if strings.TrimSpace(a.label) == "" {
		return storedAction{}, fmt.Errorf("action %q has no label", a.id)
	}
	switch a.typ {
	case linkAction:
		href, err := a.href.render(orgID, local, checkHost)
		if err != nil {
			return storedAction{}, fmt.Errorf("link %q: %w", a.label, err)
		}
		return storedAction{ID: a.id, Type: linkAction, Label: a.label, Href: href}, nil
	case invokeAction:
		if lc == domain.NotificationNotice {
			return storedAction{}, fmt.Errorf("invoke %q: a notice's actions are links", a.id)
		}
		if !actionIDPattern.MatchString(a.id) {
			return storedAction{}, fmt.Errorf("invoke id %q is not lowercase letters, digits and underscores", a.id)
		}
		switch a.requires {
		case RequiresOrgAdmin:
		case RequiresTeamAdmin:
			if to.Scope != domain.NotificationScopeTeam {
				return storedAction{}, fmt.Errorf("invoke %q requires a team admin, and a %s row is for no team", a.id, to.Scope)
			}
		default:
			return storedAction{}, fmt.Errorf("invoke %q: permission %q is not one notify checks", a.id, a.requires)
		}
		return storedAction{ID: a.id, Type: invokeAction, Label: a.label, Requires: a.requires, Confirm: a.confirm}, nil
	}
	return storedAction{}, fmt.Errorf("action %q is not built by Link or Invoke", a.label)
}

func decodeActions(raw json.RawMessage) ([]storedAction, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []storedAction
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode stored actions: %w", err)
	}
	return out, nil
}
