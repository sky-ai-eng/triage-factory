package kinds

import (
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// MemoryFailure is one error kind the org's memory generation keeps failing
// with.
type MemoryFailure struct {
	ErrorKind domain.MemoryAttemptErrorKind `json:"error_kind"`
	// TasksWaiting is the org's open tasks waiting on a memory; display only.
	TasksWaiting int `json:"tasks_waiting"`
	// ErrorMessage is the newest such attempt's TF-worded reason; display
	// only.
	ErrorMessage string `json:"error_message"`
}

// MemoryFailuresFor is f's occurrences, one per error kind.
func MemoryFailuresFor(f domain.MemoryFailing) []MemoryFailure {
	out := make([]MemoryFailure, len(f.Kinds))
	for i, k := range f.Kinds {
		out[i] = MemoryFailure{ErrorKind: k.Kind, TasksWaiting: f.TasksWaiting, ErrorMessage: k.ErrorMessage}
	}
	return out
}

// MemoryGenerationFailing is open while the org's memory generation keeps
// failing with an error kind, so every task owing a memory stays blocked. It
// is urgent when no background jobs model is usable, because then no memory
// can be generated at all, and high for a provider that fails, defers or times
// out. Not dismissible: background jobs are required, and there is no waiver
// for a task waiting on its memory. "Retry now" re-sweeps the org's owed
// memories ignoring the attempt backoff; the attempts it makes decide whether
// the condition resolves.
var MemoryGenerationFailing = notify.Condition(notify.ConditionSpec[MemoryFailure]{
	Name: "memory.generation_failing",
	Priority: notify.PriorityOf(func(f MemoryFailure) notify.Priority {
		if f.ErrorKind == domain.MemoryAttemptErrNoModel {
			return notify.Urgent
		}
		return notify.High
	}),
	Dismissible: false,
	Key:         func(f MemoryFailure) string { return string(f.ErrorKind) },
	Audience:    notify.OrgAdmins(),
	Render: func(f MemoryFailure, _ notify.Recipient) notify.Content {
		var title, cause string
		switch f.ErrorKind {
		case domain.MemoryAttemptErrNoModel:
			title = "Task memories can't be generated without a background jobs model"
			cause = "Choose a background jobs model in the organization's settings; waiting memories are " +
				"generated once it is saved."
		case domain.MemoryAttemptErrProviderError:
			title = "Task memories keep failing at the model provider"
			cause = "The background jobs model's provider answers with an error. Check its credential and " +
				"status, or choose another background jobs model."
		case domain.MemoryAttemptErrTimeout:
			title = "Task memories keep timing out"
			cause = "The background jobs model doesn't answer in time. Check the provider's status, or " +
				"choose another background jobs model."
		case domain.MemoryAttemptErrProviderBackoff:
			title = "Task memories keep waiting on the model provider"
			cause = "The background jobs model's provider has been failing or limiting requests, so Triage " +
				"Factory is holding calls to it back. Check its credential and status, or choose another " +
				"background jobs model."
		default:
			title = "Task memories keep failing to generate"
		}
		body := waitingOnMemory(f.TasksWaiting)
		if cause != "" {
			body += " " + cause
		}
		if f.ErrorMessage != "" {
			body += " Last error: " + f.ErrorMessage + "."
		}
		return notify.Content{
			Title: title,
			Body:  body,
			Actions: []notify.Action{
				notify.Link("Open background jobs model", notify.Routes.OrgSettings()),
				notify.Invoke("retry", "Retry now", notify.RequiresOrgAdmin),
			},
		}
	},
	Example: MemoryFailure{
		ErrorKind:    domain.MemoryAttemptErrNoModel,
		TasksWaiting: 3,
		ErrorMessage: "the background jobs model setting is empty",
	},
})

// waitingOnMemory says how many tasks are blocked and why they are.
func waitingOnMemory(n int) string {
	switch n {
	case 0:
		return "When a conversation ends without its agent writing a memory, Triage Factory writes one, " +
			"and nothing new starts on the task until it has."
	case 1:
		return "1 task is waiting for Triage Factory to write the memory of a conversation that ended " +
			"without one, and nothing new starts on it until it has."
	}
	return strconv.Itoa(n) + " tasks are waiting for Triage Factory to write the memory of a conversation " +
		"that ended without one, and nothing new starts on them until it has."
}
