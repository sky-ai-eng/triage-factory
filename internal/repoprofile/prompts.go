package repoprofile

import _ "embed"

// The profiler's prompts are system-job text: a toolless LLM call TF makes for
// itself, with no agent, no envelope, and no user-visible surface. They live
// next to the code that issues the call rather than in a shared prompt
// package — nothing else composes with them, and the pairing is what makes a
// change to the expected output shape reviewable alongside the parser.
//
// The %s in each is the marshalled repo batch; the local-mode arm sends the
// combined single message, multi splits system + user.

//go:embed prompts/repo-profile.txt
var repoProfilePrompt string

//go:embed prompts/repo-profile-system.txt
var repoProfileSystemPrompt string

//go:embed prompts/repo-profile-user.txt
var repoProfileUserPrompt string
