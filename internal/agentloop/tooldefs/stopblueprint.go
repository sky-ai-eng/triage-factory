package tooldefs

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// StopBlueprintName is the wire name, exported because the engine's tests and
// the delegate path both reason about this tool by name.
const StopBlueprintName = "stop_blueprint"

// stop_blueprint's `type` argument. The values are the stored ConversationOutcome
// vocabulary verbatim rather than a second, friendlier set of words: a
// translation table between what the model says and what the row holds is a
// thing that drifts.
//
// ConversationOutcomeContinue is deliberately absent. It is what an ordinary stop
// means, so offering it here would reintroduce the ambiguity this tool exists
// to remove.
const (
	stopTypeFinish = string(domain.ConversationOutcomeFinish)
	stopTypeAbort  = string(domain.ConversationOutcomeAbort)
)

// StopBlueprint is the one flow-control tool: every deliberate ending other
// than simply finishing goes through this single name, with `type` naming
// which one.
//
// The alternative — a tool per ending — put the destructive ending on the
// zero-effort path. Handing off to the next step needed an explicit call
// while ending the entire workflow needed only silence, so the prompt had to
// spend a paragraph arguing against the easier option. Stopping now means
// `continue`, and this tool is only ever reached to do something other than
// the safe default.
//
// The name says what it acts on, which is what makes its absence obvious: a
// conversation with no blueprint has nothing for it to stop, and does not get
// it. Resolved loop-side, so it never enters the sandbox.
var StopBlueprint = Tool{
	Name: StopBlueprintName,
	Description: "End this run in a way other than simply finishing. You do NOT need this to complete your work normally — " +
		"for that, reply with a message and no tool calls. Reach for this only to abort, or to end an entire " +
		"multi-step workflow early.",
	Params: obj([]string{"type", "reason", "summary"},
		Field{Name: "type", Schema: Schema{
			Type: "string",
			Enum: []string{stopTypeFinish, stopTypeAbort},
			Description: `"finish" ends the whole task successfully, skipping any workflow steps after this one. ` +
				`"abort" stops without completing it and leaves the task open for a human.`,
		}},
		str("reason", "Why you are stopping here, and what a human needs to know or do next."),
		str("summary", "The account of the work: what you did, what you found, and what state things are in. "+
			"This is shown as the run's result."),
	),
	Handler: stopBlueprint,
}

// stopBlueprint acknowledges the call, or tells the model what it was
// missing. All three arguments are enforced here rather than by the schema
// alone because a schema is advisory: a model can and does emit a call
// without one. An unenforced stop with no reason lands a terminal state
// carrying no explanation for the human who inherits it, and one with no
// summary lands a completed run with a blank result — models routinely emit
// tool-call-only messages, so there is no surrounding prose to fall back on.
//
// A correction is an ordinary error result, never a termination — the run
// continues and the model gets to fix its call.
func stopBlueprint(args map[string]any) Outcome {
	reason := stringArg(args, "reason")
	summary := stringArg(args, "summary")

	var outcome domain.ConversationOutcome
	switch stringArg(args, "type") {
	case stopTypeFinish:
		outcome = domain.ConversationOutcomeFinish
	case stopTypeAbort:
		outcome = domain.ConversationOutcomeAbort
	default:
		// A model that reached for a type that does not exist has usually
		// reached for `continue`, and the answer to that is not another tool.
		return Outcome{
			IsError: true,
			Content: `stop_blueprint needs a type of "finish" or "abort". If you meant to finish your part normally ` +
				`and hand off whatever comes next, do not call this at all — reply with a message and no tool calls.`,
		}
	}

	if reason == "" {
		return Outcome{
			IsError: true,
			Content: "stop_blueprint requires a reason. Call it again with a plain statement of why you are stopping here " +
				"and what a human needs to know or do next.",
		}
	}

	if summary == "" {
		return Outcome{
			IsError: true,
			Content: "stop_blueprint requires a summary. Call it again with an account of the work: what you did, " +
				"what you found, and what state things are in — it is shown as the run's result.",
		}
	}

	if outcome == domain.ConversationOutcomeAbort {
		return Outcome{Content: "Stopping this run. Recorded: " + reason, Terminal: outcome, Reason: reason, Summary: summary}
	}
	return Outcome{Content: "Ending the task here. Recorded: " + reason, Terminal: outcome, Reason: reason, Summary: summary}
}

func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}
