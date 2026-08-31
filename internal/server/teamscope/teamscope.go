// Package teamscope resolves which team a request is scoped to. Writes carry a
// single-team ownership stamp (ResolveActing); single-team reads target one
// team (ResolveRead). It is a pure-function leaf package — every dependency is
// passed in (the team + user stores), so it depends only on db and the httpx
// kernel and can be imported by any handler subpackage.
//
// Reads and writes are deliberately decoupled: a read is a multi-team view
// scope (the board can show many teams at once), while a write is always a
// single-team ownership stamp. So ResolveActing never consults the read filter,
// and the board's read filter never consults this — they share no primitive.
package teamscope

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ErrRequired is returned by ResolveActing when the caller belongs to two or
// more teams, supplied no explicit pick, and has no usable sticky default — the
// write is ambiguous and the picker (required in the UI whenever ≥2 teams are
// visible) should have supplied team_id. Handlers map it to 400.
var ErrRequired = errors.New("a team is required: you belong to multiple teams, so select one")

// ErrForbidden is returned by ResolveActing when the caller explicitly picks a
// team they are not a member of (in the request org). The UI only ever offers
// the caller's own teams, so this is a malformed/forged selection; handlers map
// it to 400.
var ErrForbidden = errors.New("invalid team selection: you are not a member of the selected team")

// ErrNoTeam is returned by ResolveActing / ResolveActingNoStamp when the
// caller belongs to zero teams in the org (whether because the org itself has
// none, or because the caller just isn't on any of them). An earlier version
// of this function substituted the org's default team here instead of
// erroring — a "never block" posture from before teams carried per-team roles
// or RLS enforcement. That substitution never actually helped the caller it
// was meant to: teams.GetDefaultForOrg just picks the org's oldest
// non-archived team, with no relationship to a teamless caller, so the write
// failed anyway — either at a downstream write gate (RequireTeamWrite), with
// a misleading "view-only access" message since the caller isn't a *viewer*
// of that team, they're not on it at all, or — for any call site that skips
// that gate — as a raw, uncontrolled RLS/db error (TFAC-562 found this
// surfacing as a leaked SQLSTATE 42501 message inside an HTTP 200 on the Jira
// stock POST). Erroring here instead lets every caller answer a clean 4xx via
// WriteIfSelectionError; a caller with some other legitimate no-team story
// (e.g. TFAC-562's private/org-visibility projects) checks team membership
// explicitly before ever calling ResolveActing, rather than relying on this
// function to guess.
var ErrNoTeam = errors.New("you don't belong to a team in this org — join one first")

// IsSelectionError reports whether err is a caller-fault team-selection problem
// (→ 400) versus an internal failure (→ 500). Handlers use it to map the error
// surfaced out of the WithTx closure.
func IsSelectionError(err error) bool {
	return errors.Is(err, ErrRequired) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNoTeam)
}

// WriteIfSelectionError writes a 400 with the error's message and returns true
// when err is a caller-fault team-selection error; returns false otherwise so
// the caller falls through to its normal (500) handling. Lets every
// team-stamping handler map the picker errors with one line after its WithTx
// returns. All three selection errors are faults in the request's team pick,
// so they carry the team_id field attribution.
func WriteIfSelectionError(w http.ResponseWriter, err error) bool {
	if IsSelectionError(err) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: err.Error(),
			Field:   "team_id",
		})
		return true
	}
	return false
}

// ResolveActing returns the team a write should be attributed to for the
// current request — "the acting team" — and records it as the caller's
// last-written team so the write pickers seed there next time. It centralizes
// the team selection that the multi-team UX layers on top of every write site,
// so the resolution order lives in one place.
//
// On success the resolved team is stamped onto users.last_acting_team_id as the
// durable last-used write default. The stamp is best-effort — a preference
// write failure must not fail the actual write — and runs under the caller's
// claims (users_modify RLS gates id = current_user_id()). Call this inside the
// request's WithTx.
func ResolveActing(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	resolved, err := resolveActingID(ctx, teams, users, orgID, userID, picked)
	if err != nil {
		return "", err
	}
	if resolved != "" {
		// Best-effort: remember the team this write landed on as the user's
		// last-used default. Ignore the error — the write itself has already
		// been decided and must not fail on a preference miss (e.g. a missing
		// user row in a thin fixture).
		_ = users.SetLastActingTeam(ctx, userID, resolved)
	}
	return resolved, nil
}

// ResolveActingNoStamp resolves the acting team without recording it as the
// caller's last-written default. It's the read-only sibling of ResolveActing,
// for callers that need to know the acting team *before* deciding whether the
// write may proceed — notably the viewer-role write gate (TFAC-447), which must
// resolve the team to check it but must not stamp a last-acting preference for a
// write it's about to 403. Same resolution order and error contract as
// ResolveActing (ErrRequired / ErrForbidden map to 400 via WriteIfSelectionError).
func ResolveActingNoStamp(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	return resolveActingID(ctx, teams, users, orgID, userID, picked)
}

// resolveActingID is the pure resolution (no side effects), kept separate so
// the contract is unit-testable without the last-used stamp.
//
// Resolution order:
//
//  1. Explicit pick — the team_id the write-time picker supplies. The picker is
//     required in the UI whenever the caller belongs to ≥2 teams, so for a
//     multi-team caller this is the normal path. The pick is validated against
//     the caller's current-org teams; a pick for a team they don't belong to is
//     rejected (ErrForbidden) rather than silently honored.
//
//  2. Last-written default — users.last_acting_team_id, when set and still one
//     of the caller's current-org teams. This is the team the caller's previous
//     write landed on (stamped by ResolveActing); it only applies when no
//     explicit pick arrived (a ≤1-team caller, whose picker is hidden, or a
//     non-UI client). A stale value (team deleted, or in another org) fails the
//     membership re-check and falls through.
//
//  3. Sole team — when the caller has exactly one team, that team. This is the
//     only path a solo (local or a 1-team multi org) caller ever takes, and it is
//     byte-identical to the pre-PR1 hardcoded default: ListForUser orders
//     oldest-first, and a 1-team org's only team is its default team.
//
//  4. Ambiguous (≥2 teams, no pick, no usable default) → ErrRequired. The UI
//     cannot reach this (the picker is required), so it only fires for a
//     malformed non-UI write; failing loudly beats guessing a team that could
//     misattribute shared work.
//
//  5. Zero teams (a member of the org but of no team — happens today via a
//     teamless invite or a team archival, see ZeroTeamState/TFAC-445) →
//     ErrNoTeam. Every write site must decide what "no team" means for its
//     own semantics (require joining a team first, or fall back to some
//     other non-team-scoped story, e.g. TFAC-562's private/org-visibility
//     projects) rather than have this function silently guess by picking an
//     arbitrary team the caller has no relationship to.
func resolveActingID(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	// Unwindowed on purpose (ListOpts zero Limit): this is a membership set,
	// not a page. A windowed read would make "is the caller on this team"
	// depend on where the team sorted, which is how a write silently retargets.
	myTeams, _, err := teams.ListForUser(ctx, orgID, db.Unwindowed)
	if err != nil {
		return "", fmt.Errorf("acting team: list teams: %w", err)
	}
	member := make(map[string]struct{}, len(myTeams))
	for _, t := range myTeams {
		member[t.ID] = struct{}{}
	}

	// 1. Explicit pick.
	if picked != "" {
		if _, ok := member[picked]; ok {
			return picked, nil
		}
		return "", ErrForbidden
	}

	// 2. Last-written default (only when still a member).
	lastActing, err := users.GetLastActingTeam(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("acting team: get last-acting team: %w", err)
	}
	if lastActing != "" {
		if _, ok := member[lastActing]; ok {
			return lastActing, nil
		}
	}

	// 3 + 4. One team → it; ≥2 → ambiguous.
	switch {
	case len(myTeams) == 1:
		return myTeams[0].ID, nil
	case len(myTeams) >= 2:
		return "", ErrRequired
	}

	// 5. No team membership — see the ErrNoTeam doc comment for why this
	// errors rather than substituting an arbitrary team.
	return "", ErrNoTeam
}

// ResolveRead picks the single team a single-team-oriented read should target,
// given an optional explicit pick from the caller. The Jira stock/discovery
// deck is the only consumer: it scopes to one team's Jira projects at a time
// (and the same team owns the claim), so it is single-team by construction —
// unlike the board/queue/factory reads, which are multi-team and use the
// IN-list narrow instead.
//
// Unlike ResolveActing this never errors on a multi-team caller — a read
// defaults rather than blocks. Order: valid pick → last-written default (still
// a member) → first (oldest) team. Returns "" with a nil error only when the
// caller has no teams (the handler degrades to its "not configured" branch). An
// explicit pick the caller isn't a member of is ignored (falls through to the
// default) rather than erroring, since a stale filter value should never 4xx a
// read.
func ResolveRead(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	// Unwindowed for the same reason as resolveActingID: a membership set.
	myTeams, _, err := teams.ListForUser(ctx, orgID, db.Unwindowed)
	if err != nil {
		return "", fmt.Errorf("read team: list teams: %w", err)
	}
	member := make(map[string]struct{}, len(myTeams))
	for _, t := range myTeams {
		member[t.ID] = struct{}{}
	}
	if picked != "" {
		if _, ok := member[picked]; ok {
			return picked, nil
		}
	}
	lastActing, err := users.GetLastActingTeam(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("read team: get last-acting team: %w", err)
	}
	if lastActing != "" {
		if _, ok := member[lastActing]; ok {
			return lastActing, nil
		}
	}
	if len(myTeams) > 0 {
		return myTeams[0].ID, nil
	}
	return "", nil
}

// FilterParam extracts the per-page multi-team read filter from the request —
// the set of non-empty, deduped, valid-UUID ?team_id= values (a repeated query
// param). Returns nil when none are present, which the stores treat as "the
// union of the viewer's teams." This is the read-scope counterpart to ResolveRead:
// the board/queue/factory show many teams at once, so they narrow by an IN-list
// rather than a single target.
//
// Malformed values are dropped, not errored: these params are user-controlled
// and also come from possibly-stale/corrupt localStorage (the device-local read
// filter), so a junk id should degrade to "no narrow for that value" rather than
// 500 the queue/board/factory when the raw string hits a Postgres ::uuid cast. A
// team the caller isn't in is already filtered out downstream by the
// RLS-mirroring predicate, so the only validation needed here is well-formedness.
func FilterParam(r *http.Request) []string {
	raw := r.URL.Query()["team_id"]
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v == "" {
			continue
		}
		if _, err := uuid.Parse(v); err != nil {
			continue // drop malformed ids (stale localStorage, hand-edited URL)
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// SingleParam reads a single ?team_id= value for the single-team read surfaces
// (the prompts page narrowed to one team). Like FilterParam it drops a malformed
// id rather than erroring — the value is device-local view state (possibly stale
// localStorage) and junk should degrade to "unfiltered", not 500 the page.
// Returns "" when absent or malformed; the stores treat "" as no team narrow.
func SingleParam(r *http.Request) string {
	v := r.URL.Query().Get("team_id")
	if v == "" {
		return ""
	}
	if _, err := uuid.Parse(v); err != nil {
		return "" // drop malformed (stale localStorage, hand-edited URL)
	}
	return v
}

// FilterParamStrict is FilterParam under the strict-params contract: a
// malformed ?team_id= is rejected as a 400 INVALID_PARAM instead of silently
// dropped — dropping a corrupt filter *widens* the result set, the exact
// failure the contract forbids. On a malformed value the response is already
// written and ok is false; callers return immediately. The lenient FilterParam
// remains only for handlers not yet converted; converted handlers never go
// back, and the lenient variants are deleted once the sweep finishes.
func FilterParamStrict(w http.ResponseWriter, r *http.Request) (teams []string, ok bool) {
	raw := r.URL.Query()["team_id"]
	if len(raw) == 0 {
		return nil, true
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if _, err := uuid.Parse(v); err != nil {
			writeBadTeamID(w, v)
			return nil, false
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out, true
}

// SingleParamStrict is SingleParam under the same strict-params contract as
// FilterParamStrict: only a genuinely absent param means "no narrow" (""),
// everything else must be one well-formed id. A present-but-empty
// `?team_id=` and a repeated `?team_id=a&team_id=b` are both rejected —
// treating either as absent (or taking the first value) would silently
// retarget a single-team read to the caller's default team, the exact
// fall-through this variant exists to close. Malformed ids are a 400 as in
// FilterParamStrict.
func SingleParamStrict(w http.ResponseWriter, r *http.Request) (team string, ok bool) {
	raw, present := r.URL.Query()["team_id"]
	if !present {
		return "", true
	}
	if len(raw) > 1 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidParam,
			Message: "team_id may be given at most once on this route",
			Field:   "team_id",
		})
		return "", false
	}
	if _, err := uuid.Parse(raw[0]); err != nil {
		writeBadTeamID(w, raw[0])
		return "", false
	}
	return raw[0], true
}

func writeBadTeamID(w http.ResponseWriter, v string) {
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason:  httpx.ReasonInvalidParam,
		Message: fmt.Sprintf("team_id %q is not a valid team id", v),
		Field:   "team_id",
	})
}
