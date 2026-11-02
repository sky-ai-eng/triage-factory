package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
)

var (
	// ErrNoSuchAction: the row declares no invoke action by that id, or
	// nothing in this process handles it. The route answers 404.
	ErrNoSuchAction = errors.New("notify: no such action")
	// ErrActionForbidden: the caller lacks the permission the action
	// declares. The route answers 403.
	ErrActionForbidden = errors.New("notify: the caller lacks the action's permission")
	// ErrConditionResolved: the row's condition has resolved, so the actions
	// that fix it no longer apply. The route answers 409.
	ErrConditionResolved = errors.New("notify: the condition has resolved")
)

// ActionCall is what an invoke action's handler is given: the row's org, the
// caller who invoked it, the audience the row is for, and the row's params.
type ActionCall[P any] struct {
	OrgID     string
	UserID    string
	Recipient Recipient
	Params    P
}

// ActionResult is what an invoke action's handler reports of a call that
// succeeded.
type ActionResult struct {
	// Outcome tells the caller what the action did, in a sentence. Required:
	// it is the call's whole answer when the row has since left what the
	// caller may read.
	Outcome string
	// Resolved is set when the action's effect resolved the condition, to
	// why: cleared when it fixed what was wrong, audience_removed when it took
	// the row's audience out of the condition's reach. Empty leaves the row
	// open, as an action whose effect the producer's next evaluation decides
	// does.
	Resolved domain.NotificationResolvedReason
}

// handler is a registered handler with its params type erased.
type handler func(ctx context.Context, n domain.Notification, userID string) (ActionResult, error)

// HandleAction registers fn as what the kind's invoke action id does. Register
// it once at boot, from the code that owns the operation, so the action's
// validation and authorization are that code's; registering id again replaces
// the handler, as a second server built in one process must. When fn reports
// the condition resolved, the row the action was invoked on resolves at once;
// any other audience's row of the occurrence stays until the producer's next
// evaluation.
func (c *ConditionKind[P]) HandleAction(id string, fn func(ctx context.Context, call ActionCall[P]) (ActionResult, error)) {
	k := c.k
	if !actionIDPattern.MatchString(id) {
		refuse(k.name, "action id %q is not lowercase letters, digits and underscores", id)
	}
	if fn == nil {
		refuse(k.name, "action %q needs a handler", id)
	}
	k.handlersMu.Lock()
	defer k.handlersMu.Unlock()
	if k.handlers == nil {
		k.handlers = map[string]handler{}
	}
	k.handlers[id] = func(ctx context.Context, n domain.Notification, userID string) (ActionResult, error) {
		var p P
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return ActionResult{}, fmt.Errorf("decode %s params: %w", k.name, err)
		}
		return fn(ctx, ActionCall[P]{OrgID: n.OrgID, UserID: userID, Recipient: n.Audience, Params: p})
	}
}

func (k *kind) handler(id string) handler {
	k.handlersMu.RLock()
	defer k.handlersMu.RUnlock()
	return k.handlers[id]
}

// handlerFor is the handler for kind name's action id, or nil.
func handlerFor(name, id string) handler {
	k := kindNamed(name)
	if k == nil {
		return nil
	}
	return k.handler(id)
}

// UnhandledActions lists, as "kind/action", every invoke action a registered
// kind's Example renders that has no handler. The server's boot test calls it
// once boot has registered every handler, and expects nothing.
func UnhandledActions() []string {
	var out []string
	for _, kd := range Kinds() {
		k := kd.core()
		for _, ex := range Examples(kd) {
			for _, a := range ex.Content.Actions {
				missing := k.name + "/" + a.id
				if a.typ == invokeAction && k.handler(a.id) == nil && !slices.Contains(out, missing) {
					out = append(out, missing)
				}
			}
		}
	}
	return out
}

// Authorizer answers the questions a Permission asks, under the claims the
// context carries. The server's *authz.Checker satisfies it.
type Authorizer interface {
	UserIsOrgAdmin(ctx context.Context, userID, orgID string) (bool, error)
	UserIsTeamAdmin(ctx context.Context, userID, orgID, teamID string) (bool, error)
}

// Caller is one person reading an org's notifications and running their
// actions. Build one per request: it keeps each permission answer for its
// life, so a page of rows asks each question once. It is not safe for
// concurrent use.
type Caller struct {
	az     Authorizer
	orgID  string
	userID string

	orgAdmin  *bool
	teamAdmin map[string]bool
}

func NewCaller(az Authorizer, orgID, userID string) *Caller {
	return &Caller{az: az, orgID: orgID, userID: userID, teamAdmin: map[string]bool{}}
}

// may reports whether the caller holds p on a row for to. A permission notify
// does not know is held by nobody.
func (c *Caller) may(ctx context.Context, p Permission, to Recipient) (bool, error) {
	switch p {
	case RequiresOrgAdmin:
		if c.orgAdmin == nil {
			ok, err := c.az.UserIsOrgAdmin(ctx, c.userID, c.orgID)
			if err != nil {
				return false, fmt.Errorf("org-admin check: %w", err)
			}
			c.orgAdmin = &ok
		}
		return *c.orgAdmin, nil
	case RequiresTeamAdmin:
		if to.Scope != domain.NotificationScopeTeam || to.TeamID == "" {
			return false, nil
		}
		if ok, seen := c.teamAdmin[to.TeamID]; seen {
			return ok, nil
		}
		ok, err := c.az.UserIsTeamAdmin(ctx, c.userID, c.orgID, to.TeamID)
		if err != nil {
			return false, fmt.Errorf("team-admin check: %w", err)
		}
		c.teamAdmin[to.TeamID] = ok
		return ok, nil
	}
	return false, nil
}

// ActionView is one of a row's actions as a client reads it.
type ActionView struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
	// RunningLabel is set on an invoke action: what to show in Label's place
	// while the call is out.
	RunningLabel string `json:"running_label,omitempty"`
	Href         string `json:"href,omitempty"`
	Confirm      string `json:"confirm,omitempty"`
	// Allowed is set on an invoke action: whether this caller may run it here
	// and now.
	Allowed *bool `json:"allowed,omitempty"`
}

// Actions returns n's actions for the caller. An invoke action is allowed when
// its condition is still open, the caller holds the permission it declares,
// and this process has its handler, which is exactly when RunAction would run
// it.
func (c *Caller) Actions(ctx context.Context, n domain.Notification) ([]ActionView, error) {
	if n.OrgID != c.orgID {
		return nil, fmt.Errorf("notification %s is in org %s, not the caller's %s", n.ID, n.OrgID, c.orgID)
	}
	stored, err := decodeActions(n.Actions)
	if err != nil {
		return nil, err
	}
	out := make([]ActionView, len(stored))
	for i, a := range stored {
		v := ActionView{ID: a.ID, Type: string(a.Type), Label: a.Label, RunningLabel: a.RunningLabel, Href: a.Href, Confirm: a.Confirm}
		if a.Type == invokeAction {
			ok := n.ResolvedAt == nil && handlerFor(n.Kind, a.ID) != nil
			if ok {
				if ok, err = c.may(ctx, a.Requires, n.Audience); err != nil {
					return nil, err
				}
			}
			v.Allowed = &ok
		}
		out[i] = v
	}
	return out, nil
}

// RunAction runs n's invoke action actionID as the caller: it checks the
// permission the action declares, hands the kind's handler the row's params,
// and resolves n when the handler reports the condition resolved. n is a row
// the caller may read, read under their claims, and ctx carries those claims
// to the handler. The handler's error is returned unchanged; a result with no
// outcome, or a resolution reason other than cleared or audience_removed, is
// the handler's bug and an error: a source turned off resolves its rows
// through ClearSource, not through an action.
//
// A failed resolve is logged and counted as a verb's write is, and the result
// is still returned: the effect happened, and the producer's next evaluation
// resolves the row. Call it outside any transaction, as a verb is called.
func (c *Caller) RunAction(ctx context.Context, n domain.Notification, actionID string) (ActionResult, error) {
	if n.OrgID != c.orgID {
		return ActionResult{}, ErrNoSuchAction
	}
	stored, err := decodeActions(n.Actions)
	if err != nil {
		return ActionResult{}, err
	}
	i := slices.IndexFunc(stored, func(a storedAction) bool { return a.Type == invokeAction && a.ID == actionID })
	h := handlerFor(n.Kind, actionID)
	if i < 0 || h == nil {
		return ActionResult{}, ErrNoSuchAction
	}
	ok, err := c.may(ctx, stored[i].Requires, n.Audience)
	if err != nil {
		return ActionResult{}, err
	}
	if !ok {
		return ActionResult{}, ErrActionForbidden
	}
	if n.ResolvedAt != nil {
		return ActionResult{}, ErrConditionResolved
	}
	res, err := h(ctx, n, c.userID)
	if err != nil {
		return ActionResult{}, err
	}
	if strings.TrimSpace(res.Outcome) == "" {
		return ActionResult{}, fmt.Errorf("%s action %q reported no outcome", n.Kind, actionID)
	}
	switch res.Resolved {
	case "":
	case domain.NotificationResolvedCleared, domain.NotificationResolvedAudienceRemoved:
		resolveRow(context.WithoutCancel(ctx), n, res.Resolved)
	default:
		return ActionResult{}, fmt.Errorf("%s action %q resolved for reason %q, which is not one an action reports", n.Kind, actionID, res.Resolved)
	}
	return res, nil
}

// resolveRow resolves n for reason through the installed engine, which says
// why and pings the users it is sent to. Before Install it does nothing.
func resolveRow(ctx context.Context, n domain.Notification, reason domain.NotificationResolvedReason) {
	e, ok := sink.Load().(*engine)
	if !ok {
		return
	}
	e.commit(ctx, n.Kind, n.OrgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
		got, err := tx.ResolveIDSystem(ctx, n.OrgID, n.ID, reason)
		if err != nil || got == nil {
			return nil, err
		}
		return []domain.Notification{*got}, nil
	})
}
