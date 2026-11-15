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
// A step of a running blueprint, and the step an aborted one stopped on,
// which the message's wake reopens, runs on the model its plan froze and no
// effort, whatever the message says: a message naming different ones is
// refused rather than ignored. That model is not held to the team's set
// here, because the claim runs a step on it whatever the set says now
// (modelForClaim): a step's work is not re-decided mid-flight.
//
// Otherwise the resolved model is held to the team's enabled set, which is the
// only place a send checks it, and the effort to the levels the model's
// catalog entry lists: an explicit effort it does not take is refused, and an
// inherited one is dropped, since the person did not send it. Last, a switch
// to a model whose window the conversation already fills past the compaction
// threshold is refused, so the switch can never fail a call mid-turn.
func (s *Spawner) resolveTurnSettings(ctx context.Context, orgID string, conv domain.Conversation, explicit domain.TurnSettings) (domain.TurnSettings, error) {
	br, err := s.blueprintRunForSettings(ctx, orgID, conv)
	if err != nil {
		return domain.TurnSettings{}, err
	}
	if stepSettingsFixed(br, conv) {
		if explicit.Model != "" && explicit.Model != conv.Model {
			return domain.TurnSettings{}, refuseField("model", fmt.Errorf("%w (its model is %s)", ErrStepSettingsFixed, conv.Model))
		}
		if explicit.Effort != "" {
			return domain.TurnSettings{}, refuseField("effort", fmt.Errorf("%w (it runs at the provider's default effort)", ErrStepSettingsFixed))
		}
		return domain.TurnSettings{Model: conv.Model}, nil
	}

	var last *domain.Message
	if explicit.Model == "" || explicit.Effort == "" {
		if last, err = s.newestStampedPersonRow(ctx, orgID, conv.ID); err != nil {
			return domain.TurnSettings{}, err
		}
	}
	got := explicit
	if got.Model == "" {
		if last != nil {
			got.Model = last.Model
		} else {
			model, err := s.claimModelWithoutPersonRow(ctx, orgID, conv)
			if err != nil {
				return domain.TurnSettings{}, refuseField("model", err)
			}
			got.Model = model
		}
	}
	inheritedEffort := false
	if got.Effort == "" && last != nil {
		got.Effort = last.Effort
		inheritedEffort = got.Effort != ""
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

// NextMessageSettings is what a message that names no settings would run on,
// for the composer to open with: resolveTurnSettings with nothing given. A
// message the send would refuse runs on nothing, so the refusal is the
// answer, and so is a failed read, rather than a guess.
func (s *Spawner) NextMessageSettings(ctx context.Context, orgID string, conv domain.Conversation) (domain.TurnSettings, error) {
	return s.resolveTurnSettings(ctx, orgID, conv, domain.TurnSettings{})
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

// blueprintRunForSettings is the conversation's blueprint run, read on the
// admin pool, and nil for a conversation with none. A read that fails is an
// error rather than no blueprint: resolving as for none would let a message
// move a running step off its model, through the person row the claim reads.
func (s *Spawner) blueprintRunForSettings(ctx context.Context, orgID string, conv domain.Conversation) (*domain.BlueprintRun, error) {
	if s.blueprints == nil || conv.BlueprintRunID == "" {
		return nil, nil
	}
	br, err := s.blueprints.GetRunSystem(ctx, orgID, conv.BlueprintRunID)
	if err != nil {
		return nil, fmt.Errorf("read the conversation's blueprint run: %w", err)
	}
	return br, nil
}

// stepSettingsFixed reports whether a message to conv drives a blueprint
// step, which runs on the settings its plan set: a step of a running
// blueprint, or the step an aborted one stopped on, which the message's wake
// reopens to running (wakeParked).
func stepSettingsFixed(br *domain.BlueprintRun, conv domain.Conversation) bool {
	if br == nil {
		return false
	}
	switch br.Status {
	case domain.BlueprintRunStatusRunning:
		return true
	case domain.BlueprintRunStatusAborted:
		return conv.BlueprintStepIndex != nil && *conv.BlueprintStepIndex == br.CurrentStepIndex
	}
	return false
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
