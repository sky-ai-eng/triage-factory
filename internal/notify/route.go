package notify

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Routes builds a Link's address on a page of the SPA:
//
//	notify.Link("Open GitHub settings", notify.Routes.OrgSettings())
//
// The write renders it with the org prefixed, so an API client gets an
// address that works without the SPA's org context.
var Routes routes

type routes struct{}

// OrgSettings is the org's settings: GitHub, Jira and Linear access, polling,
// the background-jobs model. Multi mode has them on /org's Settings tab; local
// mode, which has no /org page, on /settings.
func (routes) OrgSettings() Href { return Href{route: orgSettingsRoute} }

// TeamJiraRules is the Jira status rules of the team teamID names: in multi
// mode its team page, opened on the Jira source; in local mode, whose one team
// is configured on /settings, that page.
func (routes) TeamJiraRules(teamID string) Href {
	return Href{route: teamJiraRulesRoute, teamID: teamID}
}

// TeamLinearRules is the Linear rules of the team teamID names, on the same
// pages as TeamJiraRules.
func (routes) TeamLinearRules(teamID string) Href {
	return Href{route: teamLinearRulesRoute, teamID: teamID}
}

// TeamJira is a team's Jira page: its tracked projects and their status rules.
// Multi mode has it on /team's Jira source, which opens on the team the viewer
// last acted in, because the page takes no team from its address; local mode,
// which has no /team page, has the one team's on /settings.
//
// TODO(TFAC-1268): the link names neither the row's team nor its project, so
// an admin of several teams can land on another team's rules and then has to
// find the project by hand. Needs the page to read both from its URL and an
// Href that carries per-row query parameters.
func (routes) TeamJira() Href { return Href{route: teamJiraRoute} }

// route is a page of the SPA. multi is its path under /orgs/{org_id}; local is
// its path in local mode's route table, "" when local mode has no such page.
// Either may end in the query that opens the right part of the page. A team
// route is a page about one team: multi mode's address names it with a team
// query parameter, and local mode's, with one team, names none.
type route struct {
	name  string
	multi string
	local string
	team  bool
}

var (
	orgSettingsRoute     = newRoute("org_settings", "/org?tab=settings", "/settings")
	teamJiraRoute        = newRoute("team_jira", "/team?source=jira", "/settings")
	teamJiraRulesRoute   = newTeamRoute("team_jira_rules", "/team?source=jira", "/settings")
	teamLinearRulesRoute = newTeamRoute("team_linear_rules", "/team?source=linear", "/settings")
)

// routeTable is every route Routes builds. Renaming a page is an edit here and
// in frontend/src/main.tsx; TestNotifyRoutesAreSPARoutes fails until the two
// agree.
var routeTable []*route

// newRoute declares a route and enters it in routeTable, so no page Routes
// builds is missing from the table the test reads.
func newRoute(name, multi, local string) *route {
	r := &route{name: name, multi: multi, local: local}
	routeTable = append(routeTable, r)
	return r
}

// newTeamRoute declares a team route.
func newTeamRoute(name, multi, local string) *route {
	r := newRoute(name, multi, local)
	r.team = true
	return r
}

// SPARoute is one entry of the route table.
type SPARoute struct{ Name, Multi, Local string }

// SPARoutes returns the route table, for the test that holds it to the SPA's.
func SPARoutes() []SPARoute {
	out := make([]SPARoute, len(routeTable))
	for i, r := range routeTable {
		out[i] = SPARoute{Name: r.name, Multi: r.multi, Local: r.local}
	}
	return out
}

// Href is a Link's address: a page from Routes, or a page outside TF from
// External. A write that renders the zero Href is refused.
type Href struct {
	route *route
	// teamID is the team a team route's page is about.
	teamID   string
	external string
}

// External is an address outside TF. Its host must be one the org has
// configured — its GitHub, its Jira site, or Linear while a workspace is
// connected — or the write that renders it is refused: a row's params can
// carry URLs an upstream wrote, and a person follows a row's link on TF's
// word.
func External(rawURL string) Href { return Href{external: rawURL} }

// linearWebBase is the one host every Linear workspace is served from.
const linearWebBase = "https://linear.app"

// id is the id a link's stored action carries.
func (h Href) id() string {
	if h.route != nil {
		return h.route.name
	}
	return "external"
}

// render is h as a row stores it, for orgID in local or multi mode. checkHost
// decides an external address's host.
func (h Href) render(orgID string, local bool, checkHost func(*url.URL) error) (string, error) {
	switch {
	case h.route != nil:
		if h.route.team && h.teamID == "" {
			return "", fmt.Errorf("route %s names no team", h.route.name)
		}
		if !local {
			href := "/orgs/" + url.PathEscape(orgID) + h.route.multi
			if h.route.team {
				sep := "?"
				if strings.Contains(href, "?") {
					sep = "&"
				}
				href += sep + "team=" + url.QueryEscape(h.teamID)
			}
			return href, nil
		}
		if h.route.local == "" {
			return "", fmt.Errorf("route %s has no page in local mode", h.route.name)
		}
		return h.route.local, nil
	case h.external != "":
		u, err := url.Parse(h.external)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return "", fmt.Errorf("external link %q is not an absolute http(s) URL without credentials", h.external)
		}
		// A browser resolves these before it requests the page, so a path
		// that passes the base-path check could still land outside the base.
		if strings.Contains(u.Path, `\`) || slices.ContainsFunc(strings.Split(u.Path, "/"), func(seg string) bool {
			return seg == "." || seg == ".."
		}) {
			return "", fmt.Errorf("external link %q has a dot segment or a backslash in its path", h.external)
		}
		if err := checkHost(u); err != nil {
			return "", err
		}
		return u.String(), nil
	}
	return "", errors.New("a link needs an address from Routes or External")
}

// configuredHosts returns the check an external address passes when it is on
// one of the bases s configures.
func configuredHosts(s domain.OrgSettings) func(*url.URL) error {
	bases := []string{domain.EntityScope("github", s), domain.EntityScope("jira", s)}
	if s.LinearWorkspaceID != "" {
		bases = append(bases, linearWebBase)
	}
	return func(u *url.URL) error {
		for _, base := range bases {
			if under(u, base) {
				return nil
			}
		}
		return fmt.Errorf("external link host %q is not the org's GitHub, Jira or Linear", u.Host)
	}
}

// under reports whether u is base or a page beneath it: the same scheme and
// host, and base's path as a whole-segment prefix of u's.
func under(u *url.URL, base string) bool {
	b, err := url.Parse(base)
	if base == "" || err != nil || b.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, b.Scheme) || !strings.EqualFold(u.Host, b.Host) {
		return false
	}
	prefix := strings.TrimRight(b.Path, "/")
	return prefix == "" || u.Path == prefix || strings.HasPrefix(u.Path, prefix+"/")
}
