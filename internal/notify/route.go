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

// Trigger is a trigger, open in the prompts editor of the team that owns it.
func (routes) Trigger(triggerID string) Href {
	return Href{route: promptsRoute, query: url.Values{"trigger": {triggerID}}}
}

// route is a page of the SPA. multi is its path under /orgs/{org_id}; local is
// its path in local mode's route table, "" when local mode has no such page.
// Either may end in the query that opens the right part of the page.
type route struct {
	name  string
	multi string
	local string
}

var (
	orgSettingsRoute = newRoute("org_settings", "/org?tab=settings", "/settings")
	promptsRoute     = newRoute("prompts", "/prompts", "/prompts")
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
	// query is what a Routes builder names on its page; every value is
	// required, so one the params left empty refuses the write.
	query    url.Values
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
		page := "/orgs/" + url.PathEscape(orgID) + h.route.multi
		if local {
			if h.route.local == "" {
				return "", fmt.Errorf("route %s has no page in local mode", h.route.name)
			}
			page = h.route.local
		}
		if len(h.query) == 0 {
			return page, nil
		}
		for name, values := range h.query {
			if slices.Contains(values, "") {
				return "", fmt.Errorf("route %s: query %s is empty", h.route.name, name)
			}
		}
		sep := "?"
		if strings.Contains(page, "?") {
			sep = "&"
		}
		return page + sep + h.query.Encode(), nil
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
