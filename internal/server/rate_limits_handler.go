package server

import (
	"net/http"
	"slices"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ratelimits"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// rateLimitsResponse is the org's current rate-limit readings. Not a paged
// list: its cardinality is the org's credentials times the buckets each
// upstream meters, fixed by the integrations connected rather than by data
// that grows, so there is nothing to page. An object rather than a bare
// array, like every other read here, so a sibling field never changes its
// shape.
type rateLimitsResponse struct {
	Readings []rateLimitReadingView `json:"readings"`
}

// rateLimitReadingView is one bucket's latest reading as a client renders it.
// The nullable fields are nil when the reading did not state them, which is a
// different answer from zero (domain.RateLimitReading).
type rateLimitReadingView struct {
	Upstream   string `json:"upstream"`
	Credential string `json:"credential"`
	// CredentialName is what a person calls the credential — the GitHub
	// account an installation is on, the Slack workspace a bot token is in —
	// and nil when the id is the credential's kind ("pat",
	// "cloud_api_token") or its upstream cannot name it.
	CredentialName *string    `json:"credential_name"`
	Bucket         string     `json:"bucket"`
	Remaining      *int       `json:"remaining"`
	Limit          *int       `json:"limit"`
	Used           *int       `json:"used"`
	NearLimit      *bool      `json:"near_limit"`
	ResetAt        *time.Time `json:"reset_at"`
	ObservedAt     time.Time  `json:"observed_at"`
}

// handleOrgRateLimits serves how much of each rate-limit budget the org's
// credentials have left, as the processes making its requests last saw it.
//
// Member-readable: every member renders an integration's settings card, and
// the numbers name nothing a member cannot already see there. A reading past
// its reset is left out, because by then the bucket has refilled.
//
// GET /api/orgs/{org_id}/integrations/rate-limits
func (s *Server) handleOrgRateLimits(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var (
		readings []domain.RateLimitReading
		names    map[upstream.Name]map[string]string
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		readings, err = tx.RateLimitReadings.List(r.Context(), orgID, timeNow())
		if err != nil {
			return err
		}
		var upstreams []upstream.Name
		for _, rd := range readings {
			if name := upstream.Name(rd.Upstream); !slices.Contains(upstreams, name) {
				upstreams = append(upstreams, name)
			}
		}
		names, err = ratelimits.Names(r.Context(), tx, orgID, upstreams)
		return err
	}); err != nil {
		internalError(w, "rate-limits", err)
		return
	}

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
	writeJSON(w, http.StatusOK, out)
}
