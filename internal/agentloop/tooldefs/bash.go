package tooldefs

// Bash executes a shell command in the workspace. Inside the jail that
// is the widest capability the agent has, which is why the sandbox exists at
// all rather than the tool surface being trusted to constrain it.
//
// Implemented by the harness in the jail; the loop only forwards the call.
//
// The two summary params are the exception to that forwarding: they are
// display-only, and the jail drops them along with any other argument its
// shallow validation does not recognize. They exist because a shell command
// is the only argument in this registry that is not already legible to a
// person — a path, a pattern, a directory each describe themselves, while
// `go test ./internal/sandbox -run TestSampler_Series -count=20` describes
// nothing. The interface renders the summary where it would otherwise print
// the command, so a run's activity reads as sentences. Two tenses because a
// call in flight and a call that finished are different sentences, and the
// tense is authored rather than derived: nothing here transforms one into
// the other.
var Bash = Tool{
	Name:        "bash",
	Description: "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
	Params: obj([]string{"command"},
		str("command", "Bash command to execute"),
		num("timeout", "Timeout in seconds (optional, no default timeout)"),
		str("description", "What this command is doing, in 1-6 words and present tense: \"Reproducing the flake\", \"Vetting the sandbox package\". Describe the action, never its outcome."),
		str("description_past", "The same summary in past tense, in 1-6 words: \"Ran the sampler test 50x\". You are writing it before the command runs, so describe the action, never a result you cannot know yet."),
	),
}
