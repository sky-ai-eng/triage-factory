package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The refusals a person's message settings can meet before anything is
// written. Each reaches the client as a 409 naming the field it is about
// (SettingsFieldError).
var (
	// ErrStepSettingsFixed is a message to a step of a running blueprint that
	// names a model or effort other than the step's: a step runs on the model
	// its plan froze and the provider's default effort, and its work is not
	// re-decided while the blueprint runs.
	ErrStepSettingsFixed = errors.New("this blueprint step runs on the model and effort its plan set, and a message cannot change them while the blueprint runs")
	// ErrEffortNotSupported is an effort the message's model does not take,
	// by its catalog entry's effort_levels.
	ErrEffortNotSupported = errors.New("the model does not take this effort")
	// ErrModelWindowTooSmall is a switch to a model whose context window the
	// conversation already fills past the compaction threshold, so its first
	// call could not fit with room for a compaction.
	ErrModelWindowTooSmall = errors.New("the model's context window is too small for this conversation")
)

// SettingsFieldError names the message field a settings refusal is about,
// "model" or "effort", so the route can put it on the error it answers with.
type SettingsFieldError struct {
	Field string
	Err   error
}

func (e *SettingsFieldError) Error() string { return e.Err.Error() }
func (e *SettingsFieldError) Unwrap() error { return e.Err }

func refuseField(field string, err error) error {
	return &SettingsFieldError{Field: field, Err: err}
}

// resolveTurnSettings decides the model and effort a person's message runs
// on, which the send stamps on the message's row. explicit holds what the
// message gave, empty for a field it left out.
//
// A field the message gives wins, and one it leaves out takes the newest
// person row's value, so a person's pick holds until they change it. With no
// person row that recorded settings, the model is the claim's
// (claimModelWithoutPersonRow) and the effort is the provider's default.
//
// A step of a running blueprint runs on the model its plan froze and no
// effort, whatever the message says: a message naming different ones is
// refused rather than ignored.
//
// The resolved model is then held to the team's enabled set, which is the
// only place a send checks it, and the effort to the levels the model's
// catalog entry lists: an explicit effort it does not take is refused, and an
// inherited one is dropped, since the person did not send it. Last, a switch
// to a model whose window the conversation already fills past the compaction
// threshold is refused, so the switch can never fail a call mid-turn.
func (s *Spawner) resolveTurnSettings(ctx context.Context, orgID string, conv domain.Conversation, explicit domain.TurnSettings) (domain.TurnSettings, error) {
	br := s.blueprintRunForSettings(ctx, orgID, conv)
	var got domain.TurnSettings
	inheritedEffort := false
	if br != nil && br.Status == domain.BlueprintRunStatusRunning {
		if explicit.Model != "" && explicit.Model != conv.Model {
			return got, refuseField("model", fmt.Errorf("%w (its model is %s)", ErrStepSettingsFixed, conv.Model))
		}
		if explicit.Effort != "" {
			return got, refuseField("effort", fmt.Errorf("%w (it runs at the provider's default effort)", ErrStepSettingsFixed))
		}
		got.Model = conv.Model
	} else {
		var last *domain.Message
		if explicit.Model == "" || explicit.Effort == "" {
			var err error
			if last, err = s.newestStampedPersonRow(ctx, orgID, conv.ID); err != nil {
				return got, err
			}
		}
		got = explicit
		if got.Model == "" {
			if last != nil {
				got.Model = last.Model
			} else {
				model, err := s.claimModelWithoutPersonRow(ctx, orgID, br, conv)
				if err != nil {
					return got, refuseField("model", err)
				}
				got.Model = model
			}
		}
		if got.Effort == "" && last != nil {
			got.Effort = last.Effort
			inheritedEffort = got.Effort != ""
		}
	}

	models, err := s.resolveModel(ctx, orgID, conv.TeamID)
	if err != nil {
		return domain.TurnSettings{}, fmt.Errorf("resolve the team's models: %w", err)
	}
	if err := models.RequireModel(got.Model); err != nil {
		if explicit.Model != "" {
			err = fmt.Errorf("%w: %s is not among this team's enabled models (%s) — pick one of those", domain.ErrModelNotEnabled, got.Model, models.Enabled())
		}
		return domain.TurnSettings{}, refuseField("model", err)
	}

	if got.Effort != "" && !modelTakesEffort(got.Model, got.Effort) {
		if inheritedEffort {
			got.Effort = ""
		} else {
			return domain.TurnSettings{}, refuseField("effort", fmt.Errorf("%w: %s does not take %s effort", ErrEffortNotSupported, got.Model, got.Effort))
		}
	}

	if err := s.windowTakesConversation(ctx, orgID, conv.ID, got.Model); err != nil {
		return domain.TurnSettings{}, refuseField("model", err)
	}
	return got, nil
}

// NextMessageSettings is what a message that names no settings would run on:
// the newest person row's, or else the claim's model and the provider's
// default effort. It is resolveTurnSettings' first step with none of its
// refusals, for the composer to open with, so a failed read answers an
// error rather than a guess.
func (s *Spawner) NextMessageSettings(ctx context.Context, orgID string, conv domain.Conversation) (domain.TurnSettings, error) {
	last, err := s.newestStampedPersonRow(ctx, orgID, conv.ID)
	if err != nil {
		return domain.TurnSettings{}, err
	}
	if last != nil {
		return domain.TurnSettings{Model: last.Model, Effort: last.Effort}, nil
	}
	model, err := s.claimModelWithoutPersonRow(ctx, orgID, s.blueprintRunForSettings(ctx, orgID, conv), conv)
	if err != nil {
		return domain.TurnSettings{}, err
	}
	return domain.TurnSettings{Model: model}, nil
}

// newestStampedPersonRow is the conversation's newest person row when it
// recorded the settings it ran on, and nil otherwise. A row written before
// rows recorded them carries no model, and a message after it resolves as if
// there were none.
func (s *Spawner) newestStampedPersonRow(ctx context.Context, orgID, conversationID string) (*domain.Message, error) {
	if s.conversations == nil {
		return nil, nil
	}
	last, err := s.conversations.NewestPersonMessageSystem(ctx, orgID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read the newest person message: %w", err)
	}
	if last == nil || last.Model == "" {
		return nil, nil
	}
	return last, nil
}

// claimEffort is the effort an engagement on model starts at: the newest
// person message's, when it asked on model. A message on another model, or
// none, starts at the provider's default, as does a read that fails.
func (s *Spawner) claimEffort(ctx context.Context, orgID, conversationID, model string) string {
	last, err := s.newestStampedPersonRow(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the newest person message for the engagement's effort failed; starting at the provider's default",
			"conversation", conversationID, "error", err)
		return ""
	}
	if last == nil || last.Model != model {
		return ""
	}
	return last.Effort
}

// blueprintRunForSettings is the conversation's blueprint run, read as
// blueprintFollowUpBlock reads it: on the admin pool, and nil for a
// conversation with none or a read that fails, which resolves the settings as
// for a conversation with no blueprint.
func (s *Spawner) blueprintRunForSettings(ctx context.Context, orgID string, conv domain.Conversation) *domain.BlueprintRun {
	if s.blueprints == nil || conv.BlueprintRunID == "" {
		return nil
	}
	br, err := s.blueprints.GetRunSystem(ctx, orgID, conv.BlueprintRunID)
	if err != nil {
		delegateLog.Warn("settings: blueprint state lookup failed; resolving as for no blueprint", "conversation", conv.ID, "blueprint_run", conv.BlueprintRunID, "error", err)
		return nil
	}
	return br
}

// windowTakesConversation refuses a switch to model when the conversation's
// newest active assistant row, the last call the next one follows, already
// fills model's window to the compaction threshold. Below it, the first call
// on the new model fits with room for the compaction trip. A conversation
// staying on its model, one with no assistant row, and a model whose window
// is unknown are refused nothing.
func (s *Spawner) windowTakesConversation(ctx context.Context, orgID, conversationID, model string) error {
	if s.conversations == nil {
		return nil
	}
	last, err := s.conversations.NewestAssistantSystem(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("read the newest assistant message: %w", err)
	}
	if last == nil || last.Model == model {
		return nil
	}
	window, ok := inference.ModelWindow(model)
	if !ok {
		return nil
	}
	occupancy := domain.PromptOccupancy(*last)
	if occupancy >= int(agentloop.DefaultCompactionThreshold*float64(window)) {
		return fmt.Errorf("%w: the conversation holds %d tokens and %s's window is %d", ErrModelWindowTooSmall, occupancy, model, window)
	}
	return nil
}

// deploymentUniverse is the catalog this deployment offers.
func deploymentUniverse() modelcatalog.Universe {
	return modelcatalog.UniverseFor(runmode.Current() == runmode.ModeMulti)
}

// modelTakesEffort reports whether model's catalog entry lists effort. A
// model the catalog does not offer takes none.
func modelTakesEffort(model, effort string) bool {
	m, ok := deploymentUniverse().Lookup(model)
	return ok && m.TakesEffort(effort)
}

// sameProvider reports whether a and b are reached through one provider by
// their catalog entries, so one engagement's credentials serve both. A model
// the catalog does not offer matches only itself.
func sameProvider(a, b string) bool {
	if a == b {
		return true
	}
	u := deploymentUniverse()
	ma, okA := u.Lookup(a)
	mb, okB := u.Lookup(b)
	return okA && okB && ma.Provider == mb.Provider
}
