package server

import (
	"net/http"
	"slices"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ratelimits"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// rateLimitsResponse is the org's current rate-limit readings. Not a paged
// list: its cardinality is the org's credentials times the buckets each
// upstream meters, fixed by the integrations connected rather than by data
// that grows, so there is nothing to page. An object rather than a bare
// array, like every other read here, so a sibling field never changes its
// shape.
type rateLimitsResponse struct {
	Readings []rateLimitReadingView `json:"readings" doc:"The latest reading of each rate-limit budget of each of the organization's credentials, leaving out a reading past its reset time, or with no reset time and taken more than an hour ago."`
}

var opOrgRateLimits = httpx.NewJSONOp[httpx.NoBody, rateLimitsResponse](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/integrations/rate-limits",
	ID:          "list_rate_limits",
	Summary:     "List the organization's rate-limit readings",
	Description: "Returns, as one unpaged list, the latest reading of each rate-limit budget the organization's GitHub, Jira, Linear, and Slack credentials are metered against, taken from the responses to Triage Factory's own requests. It reads stored readings and calls none of those services.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// rateLimitReadingView is one bucket's latest reading as a client renders it.
// The nullable fields are nil when the reading did not state them, which is a
// different answer from zero (domain.RateLimitReading).
type rateLimitReadingView struct {
	Upstream   string `json:"upstream" doc:"The service the budget is on, such as github, jira, linear, or slack."`
	Credential string `json:"credential" doc:"The id of the credential the budget is metered against, such as pat or installation:123; never a token, a login, or a user id."`
	// CredentialName is what a person calls the credential — the GitHub
	// account an installation is on, the Slack workspace a bot token is in —
	// and nil when the id is the credential's kind ("pat",
	// "cloud_api_token") or its upstream cannot name it.
	CredentialName *string    `json:"credential_name" doc:"What a person calls the credential, such as the GitHub account an App installation is on or the Slack workspace a bot token is in; null when credential is the credential's kind or the name cannot be read."`
	Bucket         string     `json:"bucket" doc:"The budget the service meters separately for the credential, such as core or graphql on GitHub, or a Slack method."`
	Remaining      *int       `json:"remaining" doc:"How many requests the budget had left, or null when the service did not say."`
	Limit          *int       `json:"limit" doc:"The budget's size, or null when the service did not say."`
	Used           *int       `json:"used" doc:"How many requests Triage Factory counted itself making against the budget in its window, on a service that states no budget, such as Slack; null on a reading the service stated."`
	NearLimit      *bool      `json:"near_limit" doc:"Whether the service said the budget is close to its limit, on a reading that states no count, such as Jira Cloud's hourly quota; null on every other reading."`
	ResetAt        *time.Time `json:"reset_at" doc:"When the budget refills, as an RFC 3339 timestamp, or null when the service did not say."`
	ObservedAt     time.Time  `json:"observed_at" doc:"When the reading was taken, as an RFC 3339 timestamp."`
}

// handleOrgRateLimits serves how much of each rate-limit budget the org's
// credentials have left, as the processes making its requests last saw it.
//
// Member-readable: every member renders an integration's settings card, and
// the numbers name nothing a member cannot already see there. A reading past
// its reset is left out, because by then the bucket has refilled. A credential
// whose name cannot be read is served by id rather than failing the route
// (ratelimits.Names).
//
// GET /api/orgs/{org_id}/integrations/rate-limits
func (s *Server) handleOrgRateLimits(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var readings []domain.RateLimitReading
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		readings, err = tx.RateLimitReadings.List(r.Context(), orgID, timeNow())
		return err
	}); err != nil {
		internalError(w, "rate-limits", err)
		return
	}
	var upstreams []upstream.Name
	for _, rd := range readings {
		if name := upstream.Name(rd.Upstream); !slices.Contains(upstreams, name) {
			upstreams = append(upstreams, name)
		}
	}
	names := ratelimits.Names(r.Context(), orgID, upstreams, func(fn func(db.TxStores) error) error {
		return s.tx.WithReadTx(r.Context(), orgID, userID, fn)
	})

	out := rateLimitsResponse{Readings: make([]rateLimitReadingView, 0, len(readings))}
	for _, rd := range readings {
		view := rateLimitReadingView{
			Upstream:   rd.Upstream,
			Credential: rd.Credential,
			Bucket:     rd.Bucket,
			Remaining:  rd.Remaining,
			Limit:      rd.Limit,
			Used:       rd.Used,
			NearLimit:  rd.NearLimit,
			ResetAt:    rd.ResetAt,
			ObservedAt: rd.ObservedAt,
		}
		if name, ok := names[upstream.Name(rd.Upstream)][rd.Credential]; ok {
			view.CredentialName = &name
		}
		out.Readings = append(out.Readings, view)
	}
	opOrgRateLimits.Write(w, out)
}
