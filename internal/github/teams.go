package github

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
)

// userTeam is the subset of GET /user/teams we care about. See:
// https://docs.github.com/en/rest/teams/teams#list-teams-for-the-authenticated-user
type userTeam struct {
	Slug         string `json:"slug"`
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"`
}

// OrgTeam is the subset of GET /orgs/{org}/teams we care about — the
// candidate list the github-group mapping editor presents. Names only;
// no membership or nesting. See:
// https://docs.github.com/en/rest/teams/teams#list-teams
type OrgTeam struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
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
func (c *Client) ListMyTeams() ([]string, error) {
	var out []string
	for page := 1; page <= teamsPageCap; page++ {
		path := fmt.Sprintf("/user/teams?per_page=%d&page=%d", teamsPerPage, page)
		data, err := c.Get(path)
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
			log.Printf("[github] WARN: team membership list truncated at %d pages (%d teams max) — team-based review-request matching may miss memberships past this cap", teamsPageCap, teamsPageCap*teamsPerPage)
		}
	}
	return out, nil
}

// ListOrgTeams returns every team in the given GitHub org as {slug, name}
// pairs — the candidate set the github-group mapping editor presents and
// the source of truth the deletion-reconcile floor compares stored
// mappings against. Names only; no membership, no nesting traversal.
//
// Requires the PAT/App token to have read:org (or org-members read) for
// the org. A 404/403 surfaces as an error so callers can skip an org
// they can't see (and crucially NOT prune its mappings on a fetch
// failure). Paginates the REST endpoint's 100-per-page cap, bounded by
// teamsPageCap.
func (c *Client) ListOrgTeams(org string) ([]OrgTeam, error) {
	var out []OrgTeam
	for page := 1; page <= teamsPageCap; page++ {
		path := fmt.Sprintf("/orgs/%s/teams?per_page=%d&page=%d", url.PathEscape(org), teamsPerPage, page)
		data, err := c.Get(path)
		if err != nil {
			return nil, fmt.Errorf("list org %s teams page %d: %w", org, page, err)
		}
		var teams []OrgTeam
		if err := json.Unmarshal(data, &teams); err != nil {
			return nil, fmt.Errorf("parse org %s teams page %d: %w", org, page, err)
		}
		for _, t := range teams {
			if t.Slug == "" {
				continue
			}
			out = append(out, t)
		}
		if len(teams) < teamsPerPage {
			break
		}
		if page == teamsPageCap {
			log.Printf("[github] WARN: org %s team list truncated at %d pages (%d teams max)", org, teamsPageCap, teamsPageCap*teamsPerPage)
		}
	}
	return out, nil
}
