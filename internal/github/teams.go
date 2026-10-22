package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// userTeam is the subset of GET /user/teams we care about. The endpoint
// returns the *full* team representation, so name and members_count ride
// along on the same call — no per-team follow-up is needed in the
// PAT/local path. See:
// https://docs.github.com/en/rest/teams/teams#list-teams-for-the-authenticated-user
type userTeam struct {
	ID           int64  `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	MembersCount int    `json:"members_count"`
	Organization struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"organization"`
}

// UserTeam is a GitHub team as the team-mapping editor and the onboarding
// wizard present it. ID and OrgID are GitHub's ids for the team and its
// organization, which a mapping is stored and routed by ("" when GitHub
// returned none). OrgSlug + TeamSlug are its current handle, for display;
// Name is display-only; MemberCount drives the "broad team — noisy queue"
// hint and is best-effort (0 when GitHub didn't return it).
type UserTeam struct {
	ID          string
	OrgID       string
	OrgSlug     string
	TeamSlug    string
	Name        string
	MemberCount int
}

// OrgTeam is the subset of GET /orgs/{org}/teams the deletion reconcile
// reads: GitHub's id for each team, which a mapping is identified by, and its
// current slug and name. No membership or nesting. See:
// https://docs.github.com/en/rest/teams/teams#list-teams
type OrgTeam struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Org is the subset of GET /orgs/{org} the deletion reconcile reads: GitHub's
// id for the organization, which the mappings under it record, and its
// current login.
type Org struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// GetOrg reads the organization GitHub currently calls login. A user account
// or an organization the token cannot see answers 404, which surfaces as an
// error so a caller skips it rather than treating it as an organization with
// no teams.
func (c *Client) GetOrg(ctx context.Context, login string) (Org, error) {
	data, err := c.Get(ctx, "/orgs/"+url.PathEscape(login))
	if err != nil {
		return Org{}, fmt.Errorf("get org %s: %w", login, err)
	}
	var org Org
	if err := json.Unmarshal(data, &org); err != nil {
		return Org{}, fmt.Errorf("parse org %s: %w", login, err)
	}
	if org.ID == 0 {
		return Org{}, fmt.Errorf("get org %s: GitHub returned no id", login)
	}
	return org, nil
}

// githubID renders one of GitHub's numeric ids the way TF stores them, "" for
// the zero value GitHub never issues.
func githubID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// teamsPerPage is GitHub's per-page cap for REST list endpoints.
const teamsPerPage = 100

// teamsPageCap is a sanity ceiling on paginated fetches — 20 pages × 100
// teams = 2,000 teams, well beyond any realistic user's memberships. Bounds
// the worst case if GitHub ever returns a never-ending cursor.
const teamsPageCap = 20

// ListMyTeams returns the set of teams the authenticated user belongs to,
// formatted as "org/slug" strings. This format matches what reviewRequests
// GraphQL emits for team reviewers (see graphql.go), so the tracker can do
// a simple set-membership check.
//
// Requires the PAT to have the read:org scope — ValidateGitHub already
// advises users to grant it, so in practice this call succeeds whenever
// the poller's other queries do.
//
// Paginates through the REST endpoint's 100-per-page cap. Stops at
// teamsPageCap as a defensive ceiling.
func (c *Client) ListMyTeams(ctx context.Context) ([]string, error) {
	var out []string
	for page := 1; page <= teamsPageCap; page++ {
		path := fmt.Sprintf("/user/teams?per_page=%d&page=%d", teamsPerPage, page)
		data, err := c.Get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("list teams page %d: %w", page, err)
		}
		var teams []userTeam
		if err := json.Unmarshal(data, &teams); err != nil {
			return nil, fmt.Errorf("parse teams page %d: %w", page, err)
		}
		for _, t := range teams {
			if t.Slug == "" || t.Organization.Login == "" {
				continue
			}
			out = append(out, t.Organization.Login+"/"+t.Slug)
		}
		if len(teams) < teamsPerPage {
			break
		}
		if page == teamsPageCap {
			githubLog.Warn("team membership list truncated; team-based review-request matching may miss memberships past the cap",
				"page_cap", teamsPageCap, "teams_max", teamsPageCap*teamsPerPage)
		}
	}
	return out, nil
}

// ListMyTeamsDetailed returns the authenticated user's GitHub teams with
// the display name and member count the onboarding mapping step needs.
// It is the richer twin of ListMyTeams (which returns bare
// "org/slug" strings for the tracker's set-membership check): same
// endpoint and pagination, just carrying the extra fields GET /user/teams
// already includes in its full-team response.
//
// This is the PAT/local-mode path — the PAT authenticates *as the user*,
// so /user/teams answers "the teams this person is on" directly. The
// multi-mode path can't reuse it (an App installation token authenticates
// as the app, not a person); see Server.callerGitHubTeams / userTeamsMulti
// for the org-teams + membership reconstruction it uses instead.
//
// Requires the read:org scope, same as ListMyTeams.
func (c *Client) ListMyTeamsDetailed(ctx context.Context) ([]UserTeam, error) {
	var out []UserTeam
	for page := 1; page <= teamsPageCap; page++ {
		path := fmt.Sprintf("/user/teams?per_page=%d&page=%d", teamsPerPage, page)
		data, err := c.Get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("list teams page %d: %w", page, err)
		}
		var teams []userTeam
		if err := json.Unmarshal(data, &teams); err != nil {
			return nil, fmt.Errorf("parse teams page %d: %w", page, err)
		}
		for _, t := range teams {
			if t.Slug == "" || t.Organization.Login == "" {
				continue
			}
			out = append(out, UserTeam{
				ID:          githubID(t.ID),
				OrgID:       githubID(t.Organization.ID),
				OrgSlug:     t.Organization.Login,
				TeamSlug:    t.Slug,
				Name:        t.Name,
				MemberCount: t.MembersCount,
			})
		}
		if len(teams) < teamsPerPage {
			break
		}
		if page == teamsPageCap {
			githubLog.Warn("team membership list truncated",
				"page_cap", teamsPageCap, "teams_max", teamsPageCap*teamsPerPage)
		}
	}
	return out, nil
}

// ListUserTeamsInOrg returns the teams within a single GitHub org that
// userLogin belongs to, with member counts — the multi-mode counterpart
// to ListMyTeamsDetailed. The App installation token authenticates as the
// app, not a person, so GET /user/teams can't answer "this user's teams";
// instead we ask GraphQL's organization.teams connection with its
// userLogins filter, which returns exactly the teams containing that user.
// This is one paginated query per org (O(orgs)), not a membership probe
// per team (O(teams)) — the connection does the filtering server-side.
//
// member counts ride along via members { totalCount } on the same query,
// so the noisy-team hint costs no extra calls. orgLogin should be a real
// org login (a user account has no teams and yields an empty result or a
// GraphQL error, which the caller treats as "skip this owner").
//
// Requires the token to have org-members read for the org. A permission or
// not-found failure surfaces as an error so the caller can skip the org
// rather than assert it has no teams.
func (c *Client) ListUserTeamsInOrg(ctx context.Context, orgLogin, userLogin string) ([]UserTeam, error) {
	const query = `
		query UserTeamsInOrg($org: String!, $login: String!, $cursor: String) {
			organization(login: $org) {
				databaseId
				teams(first: 100, userLogins: [$login], after: $cursor) {
					pageInfo { hasNextPage endCursor }
					nodes {
						databaseId
						slug
						name
						members { totalCount }
					}
				}
			}
			rateLimit { cost }
		}
	`
	var out []UserTeam
	var cursor *string
	for page := 1; page <= teamsPageCap; page++ {
		data, err := c.PostGraphQL(ctx, query, map[string]any{
			"org":    orgLogin,
			"login":  userLogin,
			"cursor": cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("list teams for %s in org %s: %w", userLogin, orgLogin, err)
		}
		var resp struct {
			Data struct {
				Organization *struct {
					DatabaseID int64 `json:"databaseId"`
					Teams      struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							DatabaseID int64  `json:"databaseId"`
							Slug       string `json:"slug"`
							Name       string `json:"name"`
							Members    struct {
								TotalCount int `json:"totalCount"`
							} `json:"members"`
						} `json:"nodes"`
					} `json:"teams"`
				} `json:"organization"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("parse teams for org %s: %w", orgLogin, err)
		}
		// A null organization (owner is a user account, or the token can't
		// see the org) means no teams to contribute — stop, don't error.
		if resp.Data.Organization == nil {
			break
		}
		teams := resp.Data.Organization.Teams
		for _, t := range teams.Nodes {
			if t.Slug == "" {
				continue
			}
			out = append(out, UserTeam{
				ID:          githubID(t.DatabaseID),
				OrgID:       githubID(resp.Data.Organization.DatabaseID),
				OrgSlug:     orgLogin,
				TeamSlug:    t.Slug,
				Name:        t.Name,
				MemberCount: t.Members.TotalCount,
			})
		}
		if !teams.PageInfo.HasNextPage || teams.PageInfo.EndCursor == "" {
			break
		}
		c := teams.PageInfo.EndCursor
		cursor = &c
		if page == teamsPageCap {
			githubLog.Warn("org team list truncated",
				"org", orgLogin, "page_cap", teamsPageCap, "user", userLogin)
		}
	}
	return out, nil
}

// ListOrgTeamsDetailed returns every team in the given GitHub org with its
// display name and member count — the candidate list the github-group
// mapping editor and the onboarding wizard present. It is the count-bearing
// GraphQL twin of ListOrgTeams (which returns bare {slug, name} via REST for
// the deletion-reconcile floor, where counts are dead weight): the same
// organization.teams query ListUserTeamsInOrg uses, minus the userLogins
// filter, so members { totalCount } rides along and the "broad team — noisy
// queue" hint costs no extra round-trip. One paginated query per org.
//
// A null organization (owner is a user account, or the token can't see the
// org) yields an empty result rather than an error — candidates degrade to
// "the owners we could read," never blanking the whole step on one bad org.
// This mirrors ListUserTeamsInOrg and is deliberately the OPPOSITE posture
// from the REST ListOrgTeams the reconcile floor needs, where an unreadable
// org must surface as an error so pruning skips it (see manager.go) rather
// than mistaking it for "this org has no teams, delete everything."
//
// Requires read:org / Members:read for the org, same as ListOrgTeams.
func (c *Client) ListOrgTeamsDetailed(ctx context.Context, orgLogin string) ([]UserTeam, error) {
	const query = `
		query OrgTeams($org: String!, $cursor: String) {
			organization(login: $org) {
				databaseId
				teams(first: 100, after: $cursor) {
					pageInfo { hasNextPage endCursor }
					nodes {
						databaseId
						slug
						name
						members { totalCount }
					}
				}
			}
			rateLimit { cost }
		}
	`
	var out []UserTeam
	var cursor *string
	for page := 1; page <= teamsPageCap; page++ {
		data, err := c.PostGraphQL(ctx, query, map[string]any{
			"org":    orgLogin,
			"cursor": cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("list teams for org %s: %w", orgLogin, err)
		}
		var resp struct {
			Data struct {
				Organization *struct {
					DatabaseID int64 `json:"databaseId"`
					Teams      struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							DatabaseID int64  `json:"databaseId"`
							Slug       string `json:"slug"`
							Name       string `json:"name"`
							Members    struct {
								TotalCount int `json:"totalCount"`
							} `json:"members"`
						} `json:"nodes"`
					} `json:"teams"`
				} `json:"organization"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("parse teams for org %s: %w", orgLogin, err)
		}
		// A null organization (user-account owner, or unreadable org) means
		// no candidates to contribute — stop, don't error. Candidates only.
		if resp.Data.Organization == nil {
			break
		}
		teams := resp.Data.Organization.Teams
		for _, t := range teams.Nodes {
			if t.Slug == "" {
				continue
			}
			out = append(out, UserTeam{
				ID:          githubID(t.DatabaseID),
				OrgID:       githubID(resp.Data.Organization.DatabaseID),
				OrgSlug:     orgLogin,
				TeamSlug:    t.Slug,
				Name:        t.Name,
				MemberCount: t.Members.TotalCount,
			})
		}
		if !teams.PageInfo.HasNextPage || teams.PageInfo.EndCursor == "" {
			break
		}
		cur := teams.PageInfo.EndCursor
		cursor = &cur
		if page == teamsPageCap {
			githubLog.Warn("org team list truncated",
				"org", orgLogin, "page_cap", teamsPageCap, "teams_max", teamsPageCap*teamsPerPage)
		}
	}
	return out, nil
}

// ListOrgTeams returns every team in the given GitHub org with its id, slug
// and name — the source of truth the deletion reconcile compares stored
// mappings against. No membership, no nesting traversal. The candidate-list
// callers (editor + wizard) use ListOrgTeamsDetailed instead, for member
// counts; this bare REST form stays the reconcile's source because it needs
// ids, not counts, and must not depend on the extra Members-read surface the
// count subfield can require.
//
// Requires the PAT/App token to have read:org (or org-members read) for
// the org. A 404/403 surfaces as an error so callers can skip an org
// they can't see (and crucially NOT prune its mappings on a fetch
// failure). Paginates the REST endpoint's 100-per-page cap, bounded by
// teamsPageCap.
func (c *Client) ListOrgTeams(ctx context.Context, org string) ([]OrgTeam, error) {
	var out []OrgTeam
	for page := 1; page <= teamsPageCap; page++ {
		path := fmt.Sprintf("/orgs/%s/teams?per_page=%d&page=%d", url.PathEscape(org), teamsPerPage, page)
		data, err := c.Get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("list org %s teams page %d: %w", org, page, err)
		}
		var teams []OrgTeam
		if err := json.Unmarshal(data, &teams); err != nil {
			return nil, fmt.Errorf("parse org %s teams page %d: %w", org, page, err)
		}
		for _, t := range teams {
			if t.Slug == "" || t.ID == 0 {
				continue
			}
			out = append(out, t)
		}
		if len(teams) < teamsPerPage {
			break
		}
		if page == teamsPageCap {
			githubLog.Warn("org team list truncated",
				"org", org, "page_cap", teamsPageCap, "teams_max", teamsPageCap*teamsPerPage)
		}
	}
	return out, nil
}
