package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Recipient is the audience one row is for, as Render sees it.
type Recipient = domain.NotificationAudience

// Audience is who a kind's occurrences are for, built from the constructors
// below and resolved again on every write, so a team that stops tracking a
// subject stops being told about it.
type Audience struct{ parts []audiencePart }

type audiencePart struct {
	name string
	// param is the params type the part reads; nil when it reads none.
	param   reflect.Type
	resolve func(ctx context.Context, r *resolution, p any) ([]Recipient, error)
	// examples is what the part can resolve to, without a store: the
	// recipients the kinds conformance test renders for.
	examples func(p any) []Recipient
}

// exampleTeamID stands in for the teams a tracking resolver would find.
const exampleTeamID = "00000000-0000-0000-0000-000000000000"

// Audiences combines audiences. A recipient two of them name is written once.
func Audiences(as ...Audience) Audience {
	var out Audience
	for _, a := range as {
		out.parts = append(out.parts, a.parts...)
	}
	return out
}

func OrgMembers() Audience { return orgAudience(Members) }

func OrgAdmins() Audience { return orgAudience(Admins) }

func orgAudience(role Role) Audience {
	to := Recipient{Scope: domain.NotificationScopeOrg, Role: role}
	return Audience{parts: []audiencePart{{
		name:     "org " + string(role),
		resolve:  func(context.Context, *resolution, any) ([]Recipient, error) { return []Recipient{to}, nil },
		examples: func(any) []Recipient { return []Recipient{to} },
	}}}
}

// TeamMembers is the members of the team teamID names.
func TeamMembers[P any](teamID func(P) string) Audience { return teamAudience(teamID, Members) }

// TeamAdmins is the admins of the team teamID names.
func TeamAdmins[P any](teamID func(P) string) Audience { return teamAudience(teamID, Admins) }

func teamAudience[P any](teamID func(P) string, role Role) Audience {
	if teamID == nil {
		panic("notify: a team audience needs a team id function")
	}
	to := func(p any) Recipient {
		return Recipient{Scope: domain.NotificationScopeTeam, Role: role, TeamID: teamID(p.(P))}
	}
	return Audience{parts: []audiencePart{{
		name:  "team " + string(role),
		param: reflect.TypeFor[P](),
		resolve: func(_ context.Context, _ *resolution, p any) ([]Recipient, error) {
			r := to(p)
			if r.TeamID == "" {
				return nil, errors.New("team audience: the params name no team")
			}
			return []Recipient{r}, nil
		},
		examples: func(p any) []Recipient { return []Recipient{to(p)} },
	}}}
}

// Teams is the members or admins of each team teamIDs names, one audience per
// team.
func Teams[P any](teamIDs func(P) []string, role Role) Audience {
	if teamIDs == nil {
		panic("notify: Teams needs a team ids function")
	}
	if role != Members && role != Admins {
		panic(fmt.Sprintf("notify: Teams role %q is not %q or %q", role, Members, Admins))
	}
	to := func(p any) []Recipient {
		var out []Recipient
		for _, id := range teamIDs(p.(P)) {
			out = appendNew(out, Recipient{Scope: domain.NotificationScopeTeam, Role: role, TeamID: id})
		}
		return out
	}
	return Audience{parts: []audiencePart{{
		name:  "teams " + string(role),
		param: reflect.TypeFor[P](),
		resolve: func(_ context.Context, _ *resolution, p any) ([]Recipient, error) {
			rs := to(p)
			if len(rs) == 0 || slices.ContainsFunc(rs, func(r Recipient) bool { return r.TeamID == "" }) {
				return nil, errors.New("teams audience: the params name no team, or an empty one")
			}
			return rs, nil
		},
		examples: to,
	}}}
}

// User is the one user userID names.
func User[P any](userID func(P) string) Audience {
	if userID == nil {
		panic("notify: a user audience needs a user id function")
	}
	to := func(p any) Recipient { return Recipient{Scope: domain.NotificationScopeUser, UserID: userID(p.(P))} }
	return Audience{parts: []audiencePart{{
		name:  "user",
		param: reflect.TypeFor[P](),
		resolve: func(_ context.Context, _ *resolution, p any) ([]Recipient, error) {
			r := to(p)
			if r.UserID == "" {
				return nil, errors.New("user audience: the params name no user")
			}
			return []Recipient{r}, nil
		},
		examples: func(p any) []Recipient { return []Recipient{to(p)} },
	}}}
}

// TODO(TFAC-1246): TeamsTrackingJiraProject, the same resolver over the teams
// whose rules arm a jira_projects row; jira.discovery_failing is its first
// consumer.

// TeamsTrackingLinearTeam is the members or admins of every active team that
// tracks the Linear team linearTeamID names, in the org's current Linear
// workspace.
func TeamsTrackingLinearTeam[P any](linearTeamID func(P) string, role Role) Audience {
	if linearTeamID == nil {
		panic("notify: TeamsTrackingLinearTeam needs a Linear team id function")
	}
	if role != Members && role != Admins {
		panic(fmt.Sprintf("notify: TeamsTrackingLinearTeam role %q is not %q or %q", role, Members, Admins))
	}
	return Audience{parts: []audiencePart{{
		name:  "teams tracking a Linear team",
		param: reflect.TypeFor[P](),
		resolve: func(ctx context.Context, r *resolution, p any) ([]Recipient, error) {
			id := linearTeamID(p.(P))
			if id == "" {
				return nil, errors.New("linear tracking audience: the params name no Linear team")
			}
			workspace, err := r.linearWorkspace(ctx)
			if err != nil || workspace == "" {
				return nil, err
			}
			teams, err := r.b.LinearTeamRules.TrackingTeamIDsSystem(ctx, r.orgID, workspace, id)
			if err != nil {
				return nil, fmt.Errorf("teams tracking Linear team: %w", err)
			}
			out := make([]Recipient, len(teams))
			for i, team := range teams {
				out[i] = Recipient{Scope: domain.NotificationScopeTeam, Role: role, TeamID: team}
			}
			return out, nil
		},
		examples: func(any) []Recipient {
			return []Recipient{{Scope: domain.NotificationScopeTeam, Role: role, TeamID: exampleTeamID}}
		},
	}}}
}

// resolve runs every part, keeping the first of any recipient two parts name.
// Any part's failure fails the whole resolution: a partial audience would
// resolve the rows of the recipients it missed.
func (a Audience) resolve(ctx context.Context, r *resolution, p any) ([]Recipient, error) {
	var out []Recipient
	for _, part := range a.parts {
		got, err := part.resolve(ctx, r, p)
		if err != nil {
			return nil, err
		}
		out = appendNew(out, got...)
	}
	return out, nil
}

func (a Audience) examples(p any) []Recipient {
	var out []Recipient
	for _, part := range a.parts {
		out = appendNew(out, part.examples(p)...)
	}
	return out
}

func appendNew(to []Recipient, rs ...Recipient) []Recipient {
	for _, r := range rs {
		if !slices.Contains(to, r) {
			to = append(to, r)
		}
	}
	return to
}

// resolution is one call's reads of the org — its audiences and the hosts its
// links may name — sharing what every member of a Sync would otherwise read
// again.
type resolution struct {
	b     *Backend
	orgID string

	settings *domain.OrgSettings
}

func (r *resolution) orgSettings(ctx context.Context) (domain.OrgSettings, error) {
	if r.settings == nil {
		s, err := r.b.Orgs.GetSettingsSystem(ctx, r.orgID)
		if err != nil {
			return domain.OrgSettings{}, fmt.Errorf("org settings: %w", err)
		}
		r.settings = &s
	}
	return *r.settings, nil
}

func (r *resolution) linearWorkspace(ctx context.Context) (string, error) {
	s, err := r.orgSettings(ctx)
	if err != nil {
		return "", err
	}
	return domain.EntityScope("linear", s), nil
}

// checkExternal is an external link's host check against the org's configured
// hosts, read only when a link needs it.
func (r *resolution) checkExternal(ctx context.Context) func(*url.URL) error {
	return func(u *url.URL) error {
		s, err := r.orgSettings(ctx)
		if err != nil {
			return err
		}
		return configuredHosts(s)(u)
	}
}
