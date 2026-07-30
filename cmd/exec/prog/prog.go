// Package prog resolves the invocation prefix the agent-facing CLI prints in
// its own usage text, help banners, and "run this for usage" hints.
//
// It exists because that prefix cannot be a constant. The same binary answers
// to three different taught spellings — the `tfac` applet in the native jail,
// an absolute `{{BINARY_PATH}}` in the SDK sandbox, and whatever path the
// operator built in local mode (frequently not on PATH as `triagefactory` at
// all) — so a hardcoded `triagefactory exec` names a command the reader often
// cannot run. An agent that copy-pastes a usage line and gets "command not
// found" spends turns recovering from our own help output.
//
// It is a leaf on purpose: the verb subpackages (gh, jira, workspace, memory)
// cannot import cmd/exec — that's an import cycle through the dispatcher — so
// the prefix lives below all of them and everything reads it from here.
package prog

import (
	"os"
	"path/filepath"
	"sync"
)

// AppletName is the additive argv0 this binary also answers to (a second bind
// mount alongside the canonical name). Under it the `exec` word is implicit:
// `tfac <verb>` already means `<binary> exec <verb>`, so printing `exec` would
// teach a prefix that reads as required. The argv dispatch reads this too, so
// the applet has one spelling across the whole binary.
//
// It is also the one name that collapses to itself rather than echoing the
// invoked path — being short and on PATH is the entire reason the second mount
// exists, so the bare name is invocable wherever the applet is.
const AppletName = "tfac"

// fallbackPrefix is used only when argv0 is empty — a pathological exec that
// passed no argv[0] at all. Naming the canonical binary is the best guess
// available; there is no invoked path to echo back.
const fallbackPrefix = "triagefactory exec"

// prefix is resolved once: argv0 cannot change under a running process, and
// every usage string in the CLI asks for it.
var prefix = sync.OnceValue(func() string { return For(os.Args[0]) })

// Prefix returns the command prefix this process's usage text should print,
// derived from how the process was actually invoked.
func Prefix() string { return prefix() }

// For computes the prefix for an explicit argv0. Exported so the rule is
// testable without manipulating os.Args, and callable by anyone holding an
// argv0 already.
//
// Outside the applet the prefix echoes argv0 **verbatim** rather than its
// basename: argv0 is the one string proven invocable — this process is running
// under it right now — which is exactly what a reader needs to copy-paste. A
// basename would re-introduce the local-mode lie, naming a `triagefactory` that
// may well not be on PATH.
func For(argv0 string) string {
	if argv0 == "" {
		return fallbackPrefix
	}
	if filepath.Base(argv0) == AppletName {
		return AppletName
	}
	return argv0 + " exec"
}
