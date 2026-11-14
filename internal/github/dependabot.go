package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// DependabotAlertsAccess asks GitHub for one open Dependabot alert of
// owner/repo and reports which of the three answers it gave:
//
//   - 200: readable.
//   - 403 saying alerts are turned off on the repository (or unavailable on
//     an archived one): disabled.
//   - any other 403 from GitHub, or a 404: no permission. GitHub sends these
//     to an App without the Dependabot alerts permission, a classic token
//     without the security_events scope (which repo includes), a fine-grained
//     token without the Dependabot alerts permission, and a token whose
//     account may not see the repository's alerts. A GitHub Enterprise Server
//     without the endpoint answers 404 too.
//
// Anything else is an error and no answer: a rate limit comes back as
// *ErrRateLimited, and a 401, a 5xx, a transport failure or a 403 that did
// not come from GitHub (its body is not GitHub's JSON, so something in front
// of GitHub sent it) says nothing about this repository.
//
// GitHub's 403 is counted as an answered request rather than a refused
// credential (withForbiddenAsAnswer): the credential may lack this one
// permission and still do everything else TF asks of it.
func (c *Client) DependabotAlertsAccess(ctx context.Context, owner, repo string) (domain.DependabotAlertsAccess, error) {
	_, err := c.Get(withForbiddenAsAnswer(ctx), fmt.Sprintf("/repos/%s/%s/dependabot/alerts?state=open&per_page=1", owner, repo))
	if err == nil {
		return domain.DependabotAlertsReadable, nil
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		return "", err
	}
	switch {
	case he.StatusCode == http.StatusForbidden && he.Class == upstream.Auth:
		if dependabotAlertsTurnedOff(he.Body) {
			return domain.DependabotAlertsDisabled, nil
		}
		return domain.DependabotAlertsNoPermission, nil
	case he.StatusCode == http.StatusNotFound:
		return domain.DependabotAlertsNoPermission, nil
	}
	return "", err
}

type forbiddenIsAnswerKey struct{}

// withForbiddenAsAnswer marks the requests made under ctx as questions that
// GitHub answers with a 403. Its JSON 403 to one of them is counted as
// rejected, the class of every other answered 4xx, instead of auth, which the
// poll cycle's connection state and the fleet alert on failing orgs read as
// GitHub refusing the credential.
func withForbiddenAsAnswer(ctx context.Context) context.Context {
	return context.WithValue(ctx, forbiddenIsAnswerKey{}, true)
}

func forbiddenIsAnswer(ctx context.Context) bool {
	answer, _ := ctx.Value(forbiddenIsAnswerKey{}).(bool)
	return answer
}

// dependabotAlertsTurnedOff reports whether a 403 body is GitHub saying the
// repository has no alerts to read ("Dependabot alerts are disabled for this
// repository.", "Dependabot alerts are not available for archived
// repositories.") rather than refusing the credential. GitHub answers both
// with 403 and only the message tells them apart.
func dependabotAlertsTurnedOff(body string) bool {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return false
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "dependabot alerts are disabled") ||
		strings.Contains(m, "not available for archived repositories")
}

// maxDependabotAlertPages bounds one repository's listing at 100 alerts a
// page. A listing that reaches it fails rather than returning what it read:
// the caller resolves every package the listing leaves out, so a truncated
// listing would resolve packages that still have open alerts.
const maxDependabotAlertPages = 50

// ErrDependabotListingUnusable is a listing of one repository's alerts, or a
// read of their Dependabot updates, that GitHub answered with something TF
// cannot read as the whole answer: more than maxDependabotAlertPages pages, a
// page whose body does not parse, a next page with no cursor, or (for the
// updates) no repository. It says nothing about any other repository.
var ErrDependabotListingUnusable = errors.New("dependabot alert listing unusable")

// OpenDependabotAlert is one open alert as ListOpenDependabotAlerts returns it:
// the package it is on, and the alert as a vulnerable package's snapshot
// records it, with no DependabotUpdate, which the listing does not carry
// (ListDependabotUpdates reads it).
type OpenDependabotAlert struct {
	Ecosystem string
	Package   string
	Alert     domain.DependabotAlert
}

// ListOpenDependabotAlerts lists owner/repo's open Dependabot alerts with
// GET /repos/{owner}/{repo}/dependabot/alerts?state=open, 100 to a page,
// following the Link header's after cursor from page to page.
//
// etag is the ETag of the repository's previous listing, sent as the first
// page's If-None-Match; "" sends none.
//   - 304 → (nil, "", true, nil): the open alerts are unchanged, at no cost to
//     the rate limit.
//   - 200 → (alerts, newETag, false, nil). newETag is the first page's ETag
//     when the listing fit on that page, and "" when it took more: GitHub
//     orders open alerts newest first, so an alert closing past the first
//     page leaves the first page and its ETag unchanged, and a 304 would hide
//     it.
//
// Any failure fails the whole listing, a later page's included: the caller
// treats a package missing from the listing as resolved, so a partial listing
// is never returned. A listing GitHub answered that TF cannot read in full
// fails with ErrDependabotListingUnusable. As with DependabotAlertsAccess,
// GitHub's 403 is counted as an answered request rather than a refused
// credential.
func (c *Client) ListOpenDependabotAlerts(ctx context.Context, owner, repo, etag string) (alerts []OpenDependabotAlert, newETag string, notModified bool, err error) {
	ctx = withForbiddenAsAnswer(ctx)
	path := fmt.Sprintf("/repos/%s/%s/dependabot/alerts?state=open&per_page=100", owner, repo)
	body, header, notModified, err := c.getConditional(ctx, path, etag)
	if err != nil {
		return nil, "", false, err
	}
	if notModified {
		return nil, "", true, nil
	}
	if alerts, err = appendOpenDependabotAlerts(alerts, owner, repo, body); err != nil {
		return nil, "", false, err
	}
	newETag = header.Get("ETag")
	for pages := 1; ; pages++ {
		after, err := nextPageCursor(header.Get("Link"))
		if err != nil {
			return nil, "", false, fmt.Errorf("list dependabot alerts of %s/%s: %w: %w", owner, repo, ErrDependabotListingUnusable, err)
		}
		if after == "" {
			break
		}
		newETag = ""
		if pages == maxDependabotAlertPages {
			return nil, "", false, fmt.Errorf("list dependabot alerts of %s/%s: %w: more than %d pages", owner, repo, ErrDependabotListingUnusable, maxDependabotAlertPages)
		}
		if body, header, _, err = c.getConditional(ctx, path+"&after="+url.QueryEscape(after), ""); err != nil {
			return nil, "", false, err
		}
		if alerts, err = appendOpenDependabotAlerts(alerts, owner, repo, body); err != nil {
			return nil, "", false, err
		}
	}
	return alerts, newETag, false, nil
}

// restDependabotAlert is the part of GitHub's Dependabot alert object TF reads.
type restDependabotAlert struct {
	Number     int       `json:"number"`
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updated_at"`
	Dependency struct {
		Package      restDependabotPackage `json:"package"`
		ManifestPath string                `json:"manifest_path"`
		Scope        *string               `json:"scope"`
	} `json:"dependency"`
	SecurityAdvisory struct {
		GHSAID   string  `json:"ghsa_id"`
		CVEID    *string `json:"cve_id"`
		Summary  string  `json:"summary"`
		Severity string  `json:"severity"`
	} `json:"security_advisory"`
	SecurityVulnerability struct {
		Package                restDependabotPackage `json:"package"`
		Severity               string                `json:"severity"`
		VulnerableVersionRange string                `json:"vulnerable_version_range"`
		FirstPatchedVersion    *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
}

type restDependabotPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
}

// appendOpenDependabotAlerts decodes one page of the listing onto alerts. An
// alert with no number or no package names nothing TF can key, and one not
// open is not what the listing asked for; both are skipped.
func appendOpenDependabotAlerts(alerts []OpenDependabotAlert, owner, repo string, body []byte) ([]OpenDependabotAlert, error) {
	var page []restDependabotAlert
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("parse dependabot alerts of %s/%s: %w: %w", owner, repo, ErrDependabotListingUnusable, err)
	}
	for _, a := range page {
		pkg := a.Dependency.Package
		if pkg.Ecosystem == "" || pkg.Name == "" {
			pkg = a.SecurityVulnerability.Package
		}
		if a.Number == 0 || a.State != "open" || pkg.Ecosystem == "" || pkg.Name == "" {
			continue
		}
		severity := a.SecurityVulnerability.Severity
		if severity == "" {
			severity = a.SecurityAdvisory.Severity
		}
		alert := domain.DependabotAlert{
			Number:                 a.Number,
			GHSAID:                 a.SecurityAdvisory.GHSAID,
			Summary:                a.SecurityAdvisory.Summary,
			Severity:               severity,
			ManifestPath:           a.Dependency.ManifestPath,
			VulnerableVersionRange: a.SecurityVulnerability.VulnerableVersionRange,
			UpdatedAt:              a.UpdatedAt,
		}
		if a.SecurityAdvisory.CVEID != nil {
			alert.CVEID = *a.SecurityAdvisory.CVEID
		}
		if a.Dependency.Scope != nil {
			alert.Scope = *a.Dependency.Scope
		}
		if fp := a.SecurityVulnerability.FirstPatchedVersion; fp != nil {
			alert.FirstPatchedVersion = fp.Identifier
		}
		alerts = append(alerts, OpenDependabotAlert{Ecosystem: pkg.Ecosystem, Package: pkg.Name, Alert: alert})
	}
	return alerts, nil
}

// dependabotUpdatesQuery reads, for one page of a repository's open alerts,
// what Dependabot did about each: the pull request it opened or the error it
// reported. The REST alert object carries neither. The error's Markdown body
// is not asked for: it has no length limit, and its title says what failed.
const dependabotUpdatesQuery = `
	query DependabotUpdates($owner: String!, $name: String!, $after: String) {
		repository(owner: $owner, name: $name) {
			vulnerabilityAlerts(states: [OPEN], first: 100, after: $after) {
				pageInfo { hasNextPage endCursor }
				nodes {
					number
					dependabotUpdate {
						pullRequest { number url }
						error { errorType title }
					}
				}
			}
		}
		rateLimit { cost }
	}
`

// ListDependabotUpdates reads what Dependabot did about each of owner/repo's
// open Dependabot alerts, by alert number, following the connection's cursor
// from page to page (at most maxDependabotAlertPages of 100). GitHub's
// dependabotUpdate maps to a domain.DependabotUpdate:
//
//   - null: DependabotUpdateNone, Dependabot has not tried;
//   - a pull request: DependabotUpdatePullRequest, whatever else it holds;
//   - an error and no pull request: DependabotUpdateError;
//   - neither: DependabotUpdatePending, an attempt has started.
//
// An alert the answer does not name is absent from the map, which says
// nothing about it: GitHub's GraphQL can answer a credential that may not read
// a repository's alerts with no alerts rather than an error, and can lag the
// REST listing by an alert just opened.
//
// Any failure fails the whole read. GitHub answering with no repository (it
// does not exist, or the credential may not see it), a page that does not
// parse, a next page with no cursor, and more pages than the cap fail with
// ErrDependabotListingUnusable, which says nothing about any other repository.
func (c *Client) ListDependabotUpdates(ctx context.Context, owner, repo string) (map[int]domain.DependabotUpdate, error) {
	updates := map[int]domain.DependabotUpdate{}
	vars := map[string]any{"owner": owner, "name": repo}
	for pages := 1; ; pages++ {
		data, err := c.PostGraphQL(ctx, dependabotUpdatesQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("read dependabot updates of %s/%s: %w", owner, repo, err)
		}
		var resp struct {
			Data struct {
				Repository *struct {
					VulnerabilityAlerts struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []*gqlDependabotAlert `json:"nodes"`
					} `json:"vulnerabilityAlerts"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("parse dependabot updates of %s/%s: %w: %w", owner, repo, ErrDependabotListingUnusable, err)
		}
		r := resp.Data.Repository
		if r == nil {
			return nil, fmt.Errorf("read dependabot updates of %s/%s: %w: GitHub answered with no repository", owner, repo, ErrDependabotListingUnusable)
		}
		for _, n := range r.VulnerabilityAlerts.Nodes {
			if n != nil && n.Number != 0 {
				updates[n.Number] = n.update()
			}
		}
		page := r.VulnerabilityAlerts.PageInfo
		if !page.HasNextPage {
			return updates, nil
		}
		if page.EndCursor == "" {
			return nil, fmt.Errorf("read dependabot updates of %s/%s: %w: a next page with no cursor", owner, repo, ErrDependabotListingUnusable)
		}
		if pages == maxDependabotAlertPages {
			return nil, fmt.Errorf("read dependabot updates of %s/%s: %w: more than %d pages", owner, repo, ErrDependabotListingUnusable, maxDependabotAlertPages)
		}
		vars["after"] = page.EndCursor
	}
}

// gqlDependabotAlert is the part of GitHub's GraphQL alert object
// ListDependabotUpdates reads.
type gqlDependabotAlert struct {
	Number           int `json:"number"`
	DependabotUpdate *struct {
		PullRequest *struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		} `json:"pullRequest"`
		Error *struct {
			ErrorType string `json:"errorType"`
			Title     string `json:"title"`
		} `json:"error"`
	} `json:"dependabotUpdate"`
}

func (a gqlDependabotAlert) update() domain.DependabotUpdate {
	u := a.DependabotUpdate
	switch {
	case u == nil:
		return domain.DependabotUpdate{State: domain.DependabotUpdateNone}
	case u.PullRequest != nil:
		return domain.DependabotUpdate{State: domain.DependabotUpdatePullRequest, PullRequestNumber: u.PullRequest.Number, PullRequestURL: u.PullRequest.URL}
	case u.Error != nil:
		return domain.DependabotUpdate{State: domain.DependabotUpdateError, ErrorType: u.Error.ErrorType, ErrorTitle: u.Error.Title}
	}
	return domain.DependabotUpdate{State: domain.DependabotUpdatePending}
}

// nextPageCursor is the after cursor of the rel="next" link in a Link header,
// "" when there is none. Only the cursor is taken from the link: the next
// request is built on the client's own base URL, so a link naming another
// host or path is never followed. A next link that does not parse or carries
// no after cursor is an error rather than the last page, since stopping
// there would return a partial listing as complete.
func nextPageCursor(link string) (string, error) {
	for part := range strings.SplitSeq(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			return "", fmt.Errorf("parse next page link: %w", err)
		}
		after := u.Query().Get("after")
		if after == "" {
			return "", fmt.Errorf("next page link %q has no after cursor", target)
		}
		return after, nil
	}
	return "", nil
}
