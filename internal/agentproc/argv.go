package agentproc

import "strconv"

// disallowedTools is the --disallowedTools value every SDK run carries. A
// bare tool name here removes the tool from the model's context, which is
// stronger than leaving it off the allowlist: an off-allowlist call is still
// offered to the model and merely denied, and the deny lands as a turn the
// model then spends retrying or stalling on.
//
// AskUserQuestion is removed because no consumer of this wrapper can answer
// it. The one-shot path has nobody on the other end at all, and the
// interactive path routes the call through the same canUseTool callback as a
// permission prompt, so it would surface in the browser as a permission card
// whose "allow" hands the question back with no answers attached. A run that
// needs a decision from a person gets it through the permission prompt or a
// follow-up message, never a question tool.
const disallowedTools = "AskUserQuestion"

// settingSources is the --setting-sources value of an SDK spawn: the
// settings files at the run's cwd, and never the operator's own. Left unset,
// Claude Code loads ~/.claude/settings.json too, and that file's permission
// rules merge with the allowlist, its env overrides the credential TF
// resolved, and its hooks, plugins and instructions join every run.
//
// project stays because a run's staged skills (<runRoot>/.claude/skills) and
// a repository's CLAUDE.md are both discovered through it. Nothing writes a
// run root's settings files, and Claude Code refuses an agent's own writes to
// them. What a run still needs from the operator's settings, the auth surface
// of a "system credentials" setup, is forwarded by operatorAuth instead.
//
// A spawn with no Cwd (the system jobs and the model probe) loads no settings
// file at all. It runs in whatever directory TF was started from, and Claude
// Code reads project settings from the cwd, so started from the operator's
// home, the project settings file is ~/.claude/settings.json itself. Those
// spawns have no project to load anyway.
func settingSources(cwd string) string {
	if cwd == "" {
		return ""
	}
	return "project"
}

// defaultPermissionMode is the posture a run gets when RunOptions leaves
// PermissionMode empty: an off-allowlist tool call goes to the canUseTool
// handler when one is wired, and is denied when none is.
const defaultPermissionMode = "default"

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
	// Always sent. The SDK leaves an omitted mode to Claude Code, which
	// applies a settings defaultMode and, with telemetry off or a
	// third-party provider, starts in auto — where a classifier, not the
	// allowlist, decides whether an unlisted tool runs. Every caller that
	// leaves PermissionMode empty is relying on the allowlist.
	permissionMode := opts.PermissionMode
	if permissionMode == "" {
		permissionMode = defaultPermissionMode
	}
	args = append(args, "--permission-mode", permissionMode)
	args = append(args, "--setting-sources", settingSources(opts.Cwd))
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
	)
	if opts.AllowedTools != "" {
		args = append(args, "--allowedTools", opts.AllowedTools)
	}
	args = append(args, "--disallowedTools", disallowedTools)
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
