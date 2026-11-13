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

// OpenDependabotAlert is one open alert as ListOpenDependabotAlerts returns it:
// the package it is on, and the alert as a vulnerable package's snapshot
// records it.
type OpenDependabotAlert struct {
	Ecosystem string
	Package   string
	Alert     domain.DependabotAlert
	// UpdatedAt is when the alert's state last changed: when it was created,
	// reopened or reintroduced, for an open alert. Zero when GitHub sent none.
	UpdatedAt time.Time
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
// is never returned. As with DependabotAlertsAccess, GitHub's 403 is counted
// as an answered request rather than a refused credential.
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
		after := nextPageCursor(header.Get("Link"))
		if after == "" {
			break
		}
		newETag = ""
		if pages == maxDependabotAlertPages {
			return nil, "", false, fmt.Errorf("list dependabot alerts of %s/%s: more than %d pages", owner, repo, maxDependabotAlertPages)
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
		Severity string  `json:"severity"`
	} `json:"security_advisory"`
	SecurityVulnerability struct {
		Package             restDependabotPackage `json:"package"`
		Severity            string                `json:"severity"`
		FirstPatchedVersion *struct {
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
		return nil, fmt.Errorf("parse dependabot alerts of %s/%s: %w", owner, repo, err)
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
			Number:       a.Number,
			GHSAID:       a.SecurityAdvisory.GHSAID,
			Severity:     severity,
			ManifestPath: a.Dependency.ManifestPath,
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
		alerts = append(alerts, OpenDependabotAlert{Ecosystem: pkg.Ecosystem, Package: pkg.Name, Alert: alert, UpdatedAt: a.UpdatedAt})
	}
	return alerts, nil
}

// nextPageCursor is the after cursor of the rel="next" link in a Link header,
// "" when there is none. Only the cursor is taken from the link: the next
// request is built on the client's own base URL, so a link naming another
// host or path is never followed.
func nextPageCursor(link string) string {
	for part := range strings.SplitSeq(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			return ""
		}
		return u.Query().Get("after")
	}
	return ""
}
