// Package githubbind holds the two GitHub-side gates of the deployment-App
// bind ceremony: does the person completing an installation have access to it,
// and do they administer the account it was installed on.
//
// It exists as its own package because these are the security decisions of the
// ceremony, and they answer to a single rule that has to be readable in one
// place: EVERY NON-DEFINITIVE OUTCOME REFUSES. A transport failure, a 404, an
// unrecognised role string, a 403 from a permission the App lost — none of them
// is evidence that the caller may bind, so none of them may pass. There is no
// "couldn't determine, proceed" arm anywhere in this file, and a reader should
// be able to confirm that by reading only this file.
//
// The two gates are not redundant, and the second is the one GitHub does not
// prescribe. GitHub's documented fix for the spoofable installation_id stops at
// association — check that the installation is associated with the user who
// authorized. Association is exactly what a read-only contractor has: someone
// with :read on one repository inside Acme's installation sees that whole
// installation in GET /user/installations, and could otherwise bind Acme into a
// workspace they control. The authority gate is the other half.
//
// Note what authority is ABOUT: not "did this person install it" but "is this
// person, right now, an admin of the target account". The installer's identity
// is not available over any reliable channel — the installation object carries
// suspended_by but no installed_by — so asking about the installer is not an
// option even where it would be the better question.
//
// Nothing here writes, persists, or renders. The association read in particular
// is a gate and never a source: it answers yes or no about ONE installation at
// ONE instant, and the facts the ceremony goes on to store come from the App's
// own read of its installation.
package githubbind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// httpClient bounds every call here. A gate that hangs is a gate that refuses
// late, and the caller is a browser waiting on a redirect.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// maxInstallationPages bounds the association walk. The timeout above bounds
// one request; nothing bounded the LOOP, so a host that answers every page with
// another rel="next" — a misconfigured GHES, a proxy rewriting Link headers —
// would keep a browser waiting indefinitely, fifteen seconds at a time.
//
// It is a liveness guard and not a business limit, which is why it is set where
// no real answer can reach it: GitHub serves 100 installations per page, so this
// is fifty thousand installations for ONE user. Exceeding it is not a large
// account, it is a host that is not paginating. Hitting it refuses, like every
// other thing this package cannot determine — a truncated listing must never
// read as "not associated".
const maxInstallationPages = 500

var (
	// ErrNotAssociated is the association gate's definitive no: GitHub
	// answered, and the installation is not one this user can see. On a
	// redirect whose installation_id is an unsigned query parameter, this is
	// also what a spoofed id looks like.
	ErrNotAssociated = errors.New("githubbind: the installation is not associated with this user")

	// ErrNotAdmin is the authority gate's definitive no: GitHub answered, and
	// this user does not administer the account the installation targets.
	// `member` and `billing_manager` both land here — billing_manager is a real
	// value of GitHub's role enum and is not an admin.
	ErrNotAdmin = errors.New("githubbind: the user does not administer the installation's account")

	// ErrUndetermined is every other outcome: GitHub did not answer, answered
	// something this build cannot rank, or refused the question (a 403 from a
	// missing organization members permission is the live example). It is
	// deliberately distinct from the two definitive noes — the caller owes the
	// user different copy — but it refuses just the same, which is the rule
	// this package exists to hold. The cause is wrapped.
	ErrUndetermined = errors.New("githubbind: GitHub gave no definitive answer")
)

// Account is the GitHub account an installation targets, as the App itself
// reports it — never as the association read reports it.
type Account struct {
	// Type is GitHub's verbatim "Organization" or "User". A value that is
	// neither is undetermined: a target this build cannot classify is not one
	// it can prove authority over.
	Type string
	// Login is the account name, used to address the membership read and to
	// name the account in a refusal.
	Login string
	// ID is the account's numeric id — what the User-target arm compares
	// against, because it is the half of an identity a rename does not move.
	ID int64
}

// Actor is the GitHub account that authorized the installation, established by
// exchanging the callback's code for a user access token and asking GitHub who
// it belongs to.
type Actor struct {
	Login string
	ID    int64
}

// Associated is GitHub's own prescribed check: the installation must appear in
// GET /user/installations for the token that authorized it.
//
// It is a membership question, so it short-circuits on the first match and
// never collects the list — the listing is not a source, and a caller holding
// it would be one refactor away from rendering it or offering it as a choice.
//
// Pagination is followed because an installer legitimately associated with more
// than one page of installations must not be refused for arriving on page two.
func Associated(ctx context.Context, baseURL, token string, installationID int64) error {
	apiBase := ghbase.APIBase(baseURL)
	next := apiBase + "/user/installations?per_page=100"
	for pages := 0; next != ""; pages++ {
		if pages >= maxInstallationPages {
			return fmt.Errorf("%w: the installations listing did not end within %d pages",
				ErrUndetermined, maxInstallationPages)
		}
		body, link, err := get(ctx, next, token)
		if err != nil {
			return err
		}
		var page struct {
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("%w: parse user installations: %w", ErrUndetermined, err)
		}
		for _, inst := range page.Installations {
			if inst.ID == installationID {
				return nil
			}
		}
		if next, err = nextPage(apiBase, link); err != nil {
			return err
		}
	}
	return ErrNotAssociated
}

// AssociatedByAccount is the association gate asked the other way round: not
// "is this installation one the user can see" but "which installation on the
// account called login can the user see". It exists for the leg where GitHub
// never says which installation to connect — an account that already has the
// App installed is offered nothing but Configure on GitHub's install page, so
// the admin names the account and the installation has to be found — and it
// finds it among the ones the AUTHORIZING USER can see, never under the App's
// own key. Under the App's key the answer would name installations on accounts
// the user has no relation to, which is a fact about another tenant; here the
// walk can only ever return something GitHub already shows this person.
//
// Same discipline as Associated: the listing is not a source. The walk
// short-circuits on the first account match, returns one id, and the caller
// reads the installation itself under the App to learn anything about it. A
// listing that ends without a match is ErrNotAssociated — the same answer
// whether the account has no installation or the user cannot see it, and the
// caller must keep them indistinguishable. Logins compare case-insensitively
// because GitHub's do.
func AssociatedByAccount(ctx context.Context, baseURL, token, login string) (int64, error) {
	apiBase := ghbase.APIBase(baseURL)
	next := apiBase + "/user/installations?per_page=100"
	for pages := 0; next != ""; pages++ {
		if pages >= maxInstallationPages {
			return 0, fmt.Errorf("%w: the installations listing did not end within %d pages",
				ErrUndetermined, maxInstallationPages)
		}
		body, link, err := get(ctx, next, token)
		if err != nil {
			return 0, err
		}
		var page struct {
			Installations []struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
				} `json:"account"`
			} `json:"installations"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("%w: parse user installations: %w", ErrUndetermined, err)
		}
		for _, inst := range page.Installations {
			if inst.ID > 0 && strings.EqualFold(inst.Account.Login, login) {
				return inst.ID, nil
			}
		}
		if next, err = nextPage(apiBase, link); err != nil {
			return 0, err
		}
	}
	return 0, ErrNotAssociated
}

// Authority answers whether actor administers the account target, right now.
//
// Organization targets ask GitHub: GET /orgs/{org}/memberships/{username} must
// report role `admin`, and report the membership `active`. That endpoint is the
// one on GitHub's list of endpoints an App user access token may call — its
// mirror image, GET /user/memberships/orgs/{org}, is not, and would have failed
// at the first integration. It needs organization `members: read`, which the
// deployment App's preflight refuses to run without.
//
// User targets ask nobody: an account administers itself and nothing else, so
// the gate is that the actor IS the account. Compared by numeric id rather than
// by login, because a login is renameable and a comparison of renameable
// strings is a comparison that can be arranged.
func Authority(ctx context.Context, baseURL, token string, target Account, actor Actor) error {
	switch target.Type {
	case "Organization":
		return orgAdmin(ctx, baseURL, token, target.Login, actor.Login)
	case "User":
		if target.ID == 0 || actor.ID == 0 {
			// One side of the comparison is unknown, so the comparison proves
			// nothing. Refusing is the only honest answer.
			return fmt.Errorf("%w: the installation's account id or the authorizing user's id is unknown", ErrUndetermined)
		}
		if target.ID != actor.ID {
			return ErrNotAdmin
		}
		return nil
	default:
		return fmt.Errorf("%w: unrecognised installation target type %q", ErrUndetermined, target.Type)
	}
}

// orgAdmin reads one organization membership and ranks it.
//
// GitHub's role enum is admin | member | billing_manager. Only the first
// passes; the third is the one worth naming, because "billing manager" reads
// like an administrative role and is not one. A role string outside the enum is
// undetermined rather than a no — this build cannot rank what it has not been
// taught, and it must not report a verdict it did not reach.
//
// The state check is not belt-and-braces. An invited-but-not-accepted member
// comes back `pending`, and a pending admin is not an admin of that
// organization today.
func orgAdmin(ctx context.Context, baseURL, token, org, username string) error {
	if org == "" || username == "" {
		return fmt.Errorf("%w: the installation's account login or the authorizing user's login is unknown", ErrUndetermined)
	}
	endpoint := ghbase.APIBase(baseURL) + "/orgs/" + url.PathEscape(org) + "/memberships/" + url.PathEscape(username)
	body, _, err := get(ctx, endpoint, token)
	if err != nil {
		// Includes the 404 GitHub answers for a non-member, which reads as a
		// definitive "not an admin" and is deliberately NOT translated into
		// one: a 404 here is also what a revoked token, a renamed org and a
		// misrouted request produce. Undetermined refuses too, so nothing is
		// lost by declining to guess which.
		return err
	}
	var membership struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(body, &membership); err != nil {
		return fmt.Errorf("%w: parse organization membership: %w", ErrUndetermined, err)
	}
	switch membership.Role {
	case "admin":
		// The role is right; the membership still has to be live. State gets
		// the same treatment as the role — a value this build can rank decides,
		// and anything else is undetermined. An absent or empty state in
		// particular is not evidence of anything, so reporting it as "you are
		// not an admin" would be a verdict nobody reached.
		switch membership.State {
		case "active":
			return nil
		case "pending":
			return ErrNotAdmin
		default:
			return fmt.Errorf("%w: unrecognised organization membership state %q",
				ErrUndetermined, membership.State)
		}
	case "member", "billing_manager":
		return ErrNotAdmin
	default:
		return fmt.Errorf("%w: unrecognised organization role %q", ErrUndetermined, membership.Role)
	}
}

// get performs one authenticated read and returns the body with the Link
// header. Every non-2xx is ErrUndetermined with the status named — including
// the 403 a missing members permission produces, which is the failure the
// epic's scope-minimization warning is about and the one that must never read
// as a verdict about the user.
//
// The status is in the error and the body is not: a GitHub error body is
// unbounded, externally shaped text, and nothing here needs it to decide.
func get(ctx context.Context, endpoint, token string) (body []byte, link string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: build request: %w", ErrUndetermined, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "triage-factory-githubbind")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrUndetermined, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", fmt.Errorf("%w: read response: %w", ErrUndetermined, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%w: GitHub answered %d", ErrUndetermined, resp.StatusCode)
	}
	return raw, resp.Header.Get("Link"), nil
}

// nextPage returns the rel="next" URL from a Link header, after checking that
// it addresses the same origin as the API base.
//
// The check is what makes following the header safe: every request here carries
// the user's access token, so a Link pointing at another host would hand that
// token to whoever answers. Go's client already strips Authorization across a
// redirect to a different host; this closes the half the stdlib does not see.
// Same discipline as the App-JWT pagination in internal/githubapp, and for the
// same reason — the response body is attacker-shaped input, the request target
// must not be.
//
// A mismatch is an error rather than a quiet stop, because a listing that
// silently truncates would turn "your token is associated with this
// installation" into "it is not". A walk that never ends is refused the same
// way, by the page cap at the top of this file.
func nextPage(apiBase, link string) (string, error) {
	next := nextPageURL(link)
	if next == "" {
		return "", nil
	}
	base, err := url.Parse(apiBase)
	if err != nil {
		return "", fmt.Errorf("%w: parse api base: %w", ErrUndetermined, err)
	}
	target, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("%w: parse next page link: %w", ErrUndetermined, err)
	}
	if target.Scheme != base.Scheme || target.Host != base.Host {
		return "", fmt.Errorf("%w: next page link points at %s://%s, not the configured GitHub",
			ErrUndetermined, target.Scheme, target.Host)
	}
	return next, nil
}

// nextPageURL extracts the rel="next" target from an RFC 5988 Link header.
func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 {
			continue
		}
		raw := strings.TrimSpace(segments[0])
		if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
			continue
		}
		for _, attr := range segments[1:] {
			if strings.EqualFold(strings.TrimSpace(attr), `rel="next"`) {
				return strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">")
			}
		}
	}
	return ""
}
