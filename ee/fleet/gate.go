package fleet

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// handler serves the operator-gated fleet console. It holds only the admin-pool
// store bundle — every read here is a cross-org system read the fleet console
// is uniquely authorized (by the operator gate) to make.
type handler struct {
	stores db.Stores
}

// staleAfter is how long since an instance's last heartbeat before the console
// flags it stale (dead/fenced). Comfortably above the ~4s heartbeat cadence and
// the reaper's own staleness window, so a healthy pod never flickers stale.
const staleAfter = 30 * time.Second

// gate enforces the composed fleet-console authorization: a signed-in caller
// who is a deployment operator AND whose deployment is licensed for fleet
// administration AND who authenticated with a session rather than an
// org-scoped API token. The first two 404-and-hide (non-disclosure) — an
// unlicensed or non-operator caller must not learn the console exists; the
// third answers 403, for the reason given at its own check. Returns nil when it
// has already written the response; the caller returns immediately on nil.
func (h *handler) gate(w http.ResponseWriter, r *http.Request) *verify.Claims {
	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return nil
	}
	// Deployment entitlement first (cheap, no DB): fleet administration is
	// deployment-scoped, so it resolves through Active(), never For(orgID).
	if !entitlements.Active().Has(entitlements.FeatureFleet) {
		httpx.NotFound(w, "fleet")
		return nil
	}
	ok, err := h.isOperator(r.Context(), claims)
	if err != nil {
		httpx.InternalError(w, "fleet-gate", err)
		return nil
	}
	if !ok {
		httpx.NotFound(w, "fleet")
		return nil
	}
	// An API token cannot reach this console, and the refusal is 403 rather
	// than the 404 above because it is a statement about the CREDENTIAL, not
	// about the console: a token is sealed to one org, and this console's
	// subject is the deployment — there is no org for it to name, so no token
	// is ever in scope for it however privileged its holder. Placed last so
	// only a caller who would otherwise have been admitted learns that much;
	// to everyone else the console still does not exist.
	if httpx.TokenAuthFrom(r.Context()) != nil {
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
			Reason:  httpx.ReasonForbidden,
			Message: "the fleet console is deployment-scoped and cannot be reached with an org-scoped API token; sign in to use it",
		})
		return nil
	}
	return claims
}

// isOperator resolves the deployment-operator flag for the request identity.
// Local mode is a single implicit operator (matching /api/me), though the
// entitlement gate above already darkens the console there (no license). Multi
// mode looks the verified session email up in the org-less operators table.
func (h *handler) isOperator(ctx context.Context, claims *verify.Claims) (bool, error) {
	if runmode.Current() == runmode.ModeLocal {
		return true, nil
	}
	if claims.Email == "" || h.stores.Operators == nil {
		return false, nil
	}
	return h.stores.Operators.IsOperator(ctx, claims.Email)
}

// --- shared numeric helpers ---

// percentileMS returns the p-th percentile (0..100) of a slice of millisecond
// values, or nil when the slice is empty. Nearest-rank (ceil) — the standard
// latency-percentile convention, so p95 of a small sample is the tail value
// operators expect, not an interpolated middle. Sorts a copy (the caller's
// slice is left unmutated). Go-side so it is identical across SQLite/Postgres.
func percentileMS(vals []int, p float64) *int {
	if len(vals) == 0 {
		return nil
	}
	sorted := append([]int(nil), vals...)
	sort.Ints(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	v := sorted[rank-1]
	return &v
}
