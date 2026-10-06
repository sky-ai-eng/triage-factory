package db

import (
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// LinearRuleJSON is one linear_team_rules row's rule columns rendered for a
// write, one field per column. A canonical is "" when unset; each dialect
// store binds that as SQL NULL, which the ltr_armed_or_unarmed CHECK expects.
type LinearRuleJSON struct {
	Pickup              string
	InProgress          string
	InProgressCanonical string
	Done                string
	DoneCanonical       string
}

// MarshalLinearTeamRules renders r's rule columns, naming the column that
// failed so a bad ref is traceable to the rule it came from.
func MarshalLinearTeamRules(r domain.LinearTeamRules) (LinearRuleJSON, error) {
	var (
		out LinearRuleJSON
		err error
	)
	if out.Pickup, err = domain.MarshalLinearStateRefs(r.PickupMembers); err != nil {
		return LinearRuleJSON{}, fmt.Errorf("marshal pickup_members for %s: %w", r.LinearTeamID, err)
	}
	if out.InProgress, err = domain.MarshalLinearStateRefs(r.InProgressMembers); err != nil {
		return LinearRuleJSON{}, fmt.Errorf("marshal in_progress_members for %s: %w", r.LinearTeamID, err)
	}
	if out.InProgressCanonical, err = domain.MarshalLinearStateRef(r.InProgressCanonical); err != nil {
		return LinearRuleJSON{}, fmt.Errorf("marshal in_progress_canonical for %s: %w", r.LinearTeamID, err)
	}
	if out.Done, err = domain.MarshalLinearStateRefs(r.DoneMembers); err != nil {
		return LinearRuleJSON{}, fmt.Errorf("marshal done_members for %s: %w", r.LinearTeamID, err)
	}
	if out.DoneCanonical, err = domain.MarshalLinearStateRef(r.DoneCanonical); err != nil {
		return LinearRuleJSON{}, fmt.Errorf("marshal done_canonical for %s: %w", r.LinearTeamID, err)
	}
	return out, nil
}

// ScanLinearTeamRules decodes one linear_team_rules row in the order each
// dialect store's linearRuleCols projects it: team_id, linear_team_id,
// linear_team_key, linear_team_name, pickup_members, in_progress_members,
// in_progress_canonical, done_members, done_canonical — the JSON columns as
// text. Both dialects read the same plain types once Postgres casts its jsonb
// to text, so the decode lives here once rather than per dialect.
func ScanLinearTeamRules(scan func(...any) error) (domain.LinearTeamRules, error) {
	var (
		r                                  domain.LinearTeamRules
		pickup, inProgress, done           string
		inProgressCanonical, doneCanonical sql.NullString
	)
	if err := scan(&r.TeamID, &r.LinearTeamID, &r.LinearTeamKey, &r.LinearTeamName,
		&pickup, &inProgress, &inProgressCanonical, &done, &doneCanonical); err != nil {
		return domain.LinearTeamRules{}, err
	}
	var err error
	if r.PickupMembers, err = domain.UnmarshalLinearStateRefs(pickup); err != nil {
		return domain.LinearTeamRules{}, fmt.Errorf("unmarshal pickup_members for %s: %w", r.LinearTeamID, err)
	}
	if r.InProgressMembers, err = domain.UnmarshalLinearStateRefs(inProgress); err != nil {
		return domain.LinearTeamRules{}, fmt.Errorf("unmarshal in_progress_members for %s: %w", r.LinearTeamID, err)
	}
	if r.InProgressCanonical, err = domain.UnmarshalLinearStateRef(inProgressCanonical.String); err != nil {
		return domain.LinearTeamRules{}, fmt.Errorf("unmarshal in_progress_canonical for %s: %w", r.LinearTeamID, err)
	}
	if r.DoneMembers, err = domain.UnmarshalLinearStateRefs(done); err != nil {
		return domain.LinearTeamRules{}, fmt.Errorf("unmarshal done_members for %s: %w", r.LinearTeamID, err)
	}
	if r.DoneCanonical, err = domain.UnmarshalLinearStateRef(doneCanonical.String); err != nil {
		return domain.LinearTeamRules{}, fmt.Errorf("unmarshal done_canonical for %s: %w", r.LinearTeamID, err)
	}
	return r, nil
}

// ScanLinearTeamRulesRows drains rows through ScanLinearTeamRules. An empty
// result is an empty slice, never nil. err is the query's own error, so a
// caller can hand QueryContext's pair straight in.
func ScanLinearTeamRulesRows(rows *sql.Rows, err error) ([]domain.LinearTeamRules, error) {
	if err != nil {
		return nil, fmt.Errorf("read linear_team_rules: %w", err)
	}
	defer rows.Close()
	out := []domain.LinearTeamRules{}
	for rows.Next() {
		r, err := ScanLinearTeamRules(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan linear_team_rules: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read linear_team_rules: %w", err)
	}
	return out, nil
}

// ValidateLinearTeamRulesInput refuses, before ReplaceForTeam writes
// anything, an entry with no LinearTeamID or no LinearTeamKey, and a
// LinearTeamID named twice. Dropping a malformed entry would also drop its id
// from the prune list, so an input of only malformed entries would clear the
// team instead of failing; and of two entries for one Linear team, the upsert
// would keep whichever came last without saying so.
func ValidateLinearTeamRulesInput(rules []domain.LinearTeamRules) error {
	seen := make(map[string]int, len(rules))
	for i, r := range rules {
		if r.LinearTeamID == "" {
			return fmt.Errorf("ReplaceForTeam: rules[%d] has empty LinearTeamID", i)
		}
		if r.LinearTeamKey == "" {
			return fmt.Errorf("ReplaceForTeam: rules[%d] (%s) has empty LinearTeamKey", i, r.LinearTeamID)
		}
		if first, dup := seen[r.LinearTeamID]; dup {
			return fmt.Errorf("ReplaceForTeam: rules[%d] repeats LinearTeamID %s from rules[%d]", i, r.LinearTeamID, first)
		}
		seen[r.LinearTeamID] = i
	}
	return nil
}
