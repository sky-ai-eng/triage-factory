package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// This file holds the dialect-agnostic core of the boot-time shipped-defaults
// sync (ShippedDefaultsStore.SyncShippedIntoTeam): the per-unit decision
// (PlanUnitSync) and the fleet-wide orchestration (SyncShippedDefaultsForAllTeams).
// The SQLite and Postgres impls own only the SQL that fills a TeamUnitState and
// the SQL that executes a UnitSyncPlan, so both dialects make byte-identical
// decisions from the same pure logic.

// UnitSyncAction is the decision PlanUnitSync returns for one shipped blueprint.
type UnitSyncAction int

const (
	// UnitSkip: the unit is user-modified, soft-deleted, or can't be applied
	// without resurrecting a soft-deleted row — leave it entirely alone.
	UnitSkip UnitSyncAction = iota
	// UnitEqual: the team's unit already equals shipped — no writes at all (no
	// updated_at churn).
	UnitEqual
	// UnitInsert: the team has no blueprint row for this shipped slug — insert
	// the whole unit (header + steps + step prompts).
	UnitInsert
	// UnitApply: rewrite only the diverging parts of the unit in one tx.
	UnitApply
)

// TeamPromptRow is the loaded state of one team prompt row, keyed by its
// system_slug. Exists is false when the team has no row for (org, team, slug)
// at all; Deleted marks a soft-deleted occupant of that slot.
type TeamPromptRow struct {
	Exists       bool
	Deleted      bool
	UserModified bool
	ID           string
	Name         string
	Body         string
	Model        string
	AllowedTools string
}

// TeamUnitState is the team-side state of a shipped blueprint's sync unit, read
// by the impl before deciding what to write.
type TeamUnitState struct {
	// BlueprintExists is false when the team has no blueprint row for the
	// shipped slug at all (missing unit → insert). A soft-deleted blueprint row
	// still counts as existing (BlueprintExists=true, BlueprintDeleted=true) so
	// the slot stays occupied and the unit is skipped, never resurrected.
	BlueprintExists   bool
	BlueprintID       string
	BlueprintName     string
	BlueprintModified bool
	BlueprintDeleted  bool

	// The team's current step list, ordered by step_index. Parallel slices:
	// StepSlugs[i] is the step prompt's system_slug ("" for a non-system prompt
	// wired in, itself a divergence), StepBriefs[i] its brief, StepPromptIDs[i]
	// its prompt id, CurrentStep[i] its content + flags.
	StepSlugs     []string
	StepBriefs    []string
	StepPromptIDs []string
	CurrentStep   []TeamPromptRow

	// PromptsBySlug covers every shipped step slug of the unit, probed from
	// (org, team, slug). For slugs that are also current steps this is the same
	// live row. Used to resolve apply targets (update / insert / conflict).
	PromptsBySlug map[string]TeamPromptRow
}

// PromptContentWrite is one step prompt to bring to shipped content: Insert
// mints a fresh row (impl supplies the UUID) when the team lacks the slug;
// otherwise it updates the existing live row identified by PromptID.
type PromptContentWrite struct {
	Slug         string
	PromptID     string
	Insert       bool
	Name         string
	Body         string
	Model        string
	AllowedTools string
}

// UnitSyncPlan is the executable result of PlanUnitSync. For UnitApply /
// UnitInsert the impl runs, in one tx: (insert the header on UnitInsert,) apply
// PromptWrites, then — if ReplaceSteps or UnitInsert — rewrite the step list to
// StepTargets, then soft-delete DropPromptIDs, then — if RenameHeader,
// ReplaceSteps, or UnitInsert — write the header (name + updated_at).
type UnitSyncPlan struct {
	Action UnitSyncAction

	// HeaderName is the shipped blueprint name (set on rename or insert).
	HeaderName string
	// RenameHeader is true when the header name diverged (apply only).
	RenameHeader bool
	// ReplaceSteps is true when the ordered step structure diverged (apply
	// only; UnitInsert always writes steps).
	ReplaceSteps bool
	// StepTargets is the desired ordered step list as shipped slugs; the impl
	// resolves each to a prompt id (existing live row or a PromptWrites insert).
	StepTargets []string
	// PromptWrites inserts/updates only the step prompts that diverge; equal
	// rows are left untouched to avoid updated_at churn.
	PromptWrites []PromptContentWrite
	// DropPromptIDs are current step prompts no longer in the shipped structure
	// — soft-deleted (system-owned by construction within an unmodified unit).
	DropPromptIDs []string

	// SkipReason notes an unresolvable-conflict skip (a shipped step slug
	// occupied only by a soft-deleted or user-modified row) as opposed to the
	// ordinary modified/deleted skip. Empty otherwise.
	SkipReason string
}

// PlanUnitSync decides what SyncShippedIntoTeam must do for one shipped
// blueprint, given the loaded team-side state. Pure and dialect-agnostic.
func PlanUnitSync(shipped domain.SeedBlueprint, shippedPromptBySlug map[string]domain.Prompt, st TeamUnitState) UnitSyncPlan {
	// Deleted/modified checks come first — they short-circuit before any
	// content comparison, and a soft-deleted blueprint is never resurrected.
	if st.BlueprintExists {
		if st.BlueprintDeleted || st.BlueprintModified {
			return UnitSyncPlan{Action: UnitSkip}
		}
		for _, sp := range st.CurrentStep {
			if sp.Deleted || sp.UserModified {
				return UnitSyncPlan{Action: UnitSkip}
			}
		}
	}

	// Resolve the shipped step prompts to writes (insert/update), or bail out
	// on a conflict (a slug occupied by a soft-deleted or user-modified row).
	writes, skip := planPromptWrites(shipped, shippedPromptBySlug, st)
	if skip != "" {
		return UnitSyncPlan{Action: UnitSkip, SkipReason: skip}
	}

	// Missing unit: no blueprint row at all → insert the whole unit.
	if !st.BlueprintExists {
		return UnitSyncPlan{
			Action:       UnitInsert,
			HeaderName:   shipped.Name,
			StepTargets:  shipped.StepPromptSlugs,
			PromptWrites: writes,
		}
	}

	replaceSteps := !stepStructureEqual(st.StepSlugs, st.StepBriefs, shipped.StepPromptSlugs)
	renameHeader := st.BlueprintName != shipped.Name

	// Current step prompts whose slug is no longer in the shipped structure are
	// dropped (soft-deleted). Only relevant when the structure is being rewritten.
	var drop []string
	if replaceSteps {
		shippedSet := make(map[string]bool, len(shipped.StepPromptSlugs))
		for _, s := range shipped.StepPromptSlugs {
			shippedSet[s] = true
		}
		for i, slug := range st.StepSlugs {
			if slug == "" || !shippedSet[slug] {
				drop = append(drop, st.StepPromptIDs[i])
			}
		}
	}

	if len(writes) == 0 && !replaceSteps && !renameHeader && len(drop) == 0 {
		return UnitSyncPlan{Action: UnitEqual}
	}
	return UnitSyncPlan{
		Action:        UnitApply,
		HeaderName:    shipped.Name,
		RenameHeader:  renameHeader,
		ReplaceSteps:  replaceSteps,
		StepTargets:   shipped.StepPromptSlugs,
		PromptWrites:  writes,
		DropPromptIDs: drop,
	}
}

// planPromptWrites resolves each shipped step slug to an insert or update (or
// none when already equal). It returns a non-empty skip reason when a slug is
// occupied by a soft-deleted row (non-resurrection) or a user-modified row
// (never clobber a user edit) — either makes the whole unit un-appliable.
func planPromptWrites(shipped domain.SeedBlueprint, shippedPromptBySlug map[string]domain.Prompt, st TeamUnitState) ([]PromptContentWrite, string) {
	var writes []PromptContentWrite
	for _, slug := range shipped.StepPromptSlugs {
		want := shippedPromptBySlug[slug]
		row := st.PromptsBySlug[slug]
		switch {
		case row.Exists && row.Deleted:
			return nil, fmt.Sprintf("step prompt slug %q is soft-deleted; not resurrecting", slug)
		case row.Exists && row.UserModified:
			return nil, fmt.Sprintf("step prompt slug %q is user-modified", slug)
		case row.Exists:
			if !promptContentEqual(row, want) {
				writes = append(writes, PromptContentWrite{
					Slug: slug, PromptID: row.ID,
					Name: want.Name, Body: want.Body, Model: want.Model, AllowedTools: want.AllowedTools,
				})
			}
		default:
			writes = append(writes, PromptContentWrite{
				Slug: slug, Insert: true,
				Name: want.Name, Body: want.Body, Model: want.Model, AllowedTools: want.AllowedTools,
			})
		}
	}
	return writes, ""
}

// promptContentEqual reports whether a loaded team prompt row already matches
// the shipped content the sync would write (name, body, model, allowed_tools).
func promptContentEqual(row TeamPromptRow, want domain.Prompt) bool {
	return row.Name == want.Name &&
		row.Body == want.Body &&
		row.Model == want.Model &&
		row.AllowedTools == want.AllowedTools
}

// stepStructureEqual reports whether the team's current ordered step structure
// already equals shipped: same length, same slug at each position, and every
// current brief empty (shipped steps carry no brief).
func stepStructureEqual(currentSlugs, currentBriefs, shippedSlugs []string) bool {
	if len(currentSlugs) != len(shippedSlugs) {
		return false
	}
	for i := range shippedSlugs {
		if currentSlugs[i] != shippedSlugs[i] {
			return false
		}
		if currentBriefs[i] != "" {
			return false
		}
	}
	return true
}

// SlugsDiverged reports whether the team's ordered step slugs differ from the
// shipped structure — the grandfather backfill's stamping test (ordered slugs
// only; briefs are not part of the structural signal it grandfathers).
func SlugsDiverged(currentSlugs, shippedSlugs []string) bool {
	if len(currentSlugs) != len(shippedSlugs) {
		return true
	}
	for i := range shippedSlugs {
		if currentSlugs[i] != shippedSlugs[i] {
			return true
		}
	}
	return false
}

// ShippedPromptsBySlug indexes a shipped prompt list by system_slug so the sync
// can resolve a step slug to the content it should carry.
func ShippedPromptsBySlug(prompts []domain.Prompt) map[string]domain.Prompt {
	m := make(map[string]domain.Prompt, len(prompts))
	for _, p := range prompts {
		m[p.SystemSlug] = p
	}
	return m
}

// SyncShippedDefaultsForAllTeams runs the boot-time shipped-defaults sync across
// every active org × team on the admin pool. Per-team failures are collected and
// returned joined but never abort the sweep — a failed sync must never block
// boot, and the next boot retries cheaply. Returns nil when no tenant is
// provisioned (a fresh local install has zero orgs). Honors ctx cancellation so
// a demoted lease holder stops mid-fleet.
func SyncShippedDefaultsForAllTeams(ctx context.Context, stores Stores, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	orgIDs, err := stores.Orgs.ListActiveSystem(ctx)
	if err != nil {
		return fmt.Errorf("shipped defaults sync: list orgs: %w", err)
	}
	var errs []error
	for _, orgID := range orgIDs {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		teams, err := stores.Teams.ListActiveForOrgSystem(ctx, orgID)
		if err != nil {
			errs = append(errs, fmt.Errorf("shipped defaults sync: list teams for org %s: %w", orgID, err))
			continue
		}
		for _, t := range teams {
			if ctx.Err() != nil {
				errs = append(errs, ctx.Err())
				break
			}
			if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, t.ID, shippedPrompts, shippedBlueprints); err != nil {
				errs = append(errs, fmt.Errorf("shipped defaults sync: org %s team %s: %w", orgID, t.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}
