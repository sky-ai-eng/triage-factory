package agentproc

import "strconv"

// BuildArgs assembles the argv consumed by wrapper.mjs (which translates
// it into Agent SDK Options). Pulled out of Run so the flag set is
// unit-testable without spawning a subprocess.
//
// The shape historically mirrored the `claude` CLI's flags (the runtime
// used to be `claude -p`); the wrapper preserves those flag names so
// switching between the CLI and the SDK runtime is a one-line change in
// run.go. New flags should be added here so both the initial-run and
// resume paths pick them up uniformly, and mirrored in wrapper.mjs's
// parseArgs switch.
func BuildArgs(opts RunOptions) []string {
	var args []string
	if opts.Interactive {
		// Streaming-input mode: the prompt is fed as a stream of user
		// messages over stdin rather than a one-shot -p value, which is
		// what unlocks the SDK's live controls. The initial message is
		// sent by the caller over stdin once the wrapper signals ready,
		// so Message is deliberately omitted from argv here.
		args = append(args, "--input-format", "stream-json")
		// Opt the wrapper into the canUseTool permission callback. Emitted
		// only when the caller supplied a permission handler; without it
		// the wrapper omits canUseTool entirely and off-allowlist tools
		// auto-deny (byte-identical to the headless allowlist-only path).
		// Interactive-mode only — the one-shot wrapper never wires
		// canUseTool, so the flag would be inert there.
		if opts.PermissionPrompts {
			args = append(args, "--permission-prompts")
		}
	} else {
		args = append(args, "-p", opts.Message)
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
	)
	if opts.AllowedTools != "" {
		args = append(args, "--allowedTools", opts.AllowedTools)
	}
	for _, dir := range opts.AddDirs {
		if dir == "" {
			continue
		}
		args = append(args, "--add-dir", dir)
	}
	if opts.SystemPrompt != "" {
		// --append-system-prompt is additive: it sits after Claude
		// Code's default system prompt rather than replacing it. Delegate
		// sets this for a non-terminal blueprint step to note the step
		// boundary; a terminal (or single-step) run leaves it unset so the
		// envelope (mission text) carries all role-shaping content.
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	return args
}
