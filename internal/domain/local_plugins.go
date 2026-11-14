package domain

// LocalPlugin is one entry of a team's local plugin list
// (TeamSettings.LocalPlugins): a Claude Code plugin directory loaded into the
// team's local-mode delegated runs through the Agent SDK's plugins option.
//
// The JSON tags are snake_case, unlike TeamSettings' own untagged fields,
// because one shape serves the stored column, the settings PATCH body and the
// GET item alike.
type LocalPlugin struct {
	// Path is the plugin's directory, absolute, holding
	// .claude-plugin/plugin.json. The spawn resolves symlinks in it and
	// passes, binds and checks the resolved path.
	Path string `json:"path"`

	// Required makes the run refuse every tool call until the plugin has
	// loaded with no load errors, and fail the run when it does not.
	Required bool `json:"required"`

	// Env is set in the run's environment for the plugin's hooks. Values are
	// literal, except that {run_state_dir} expands to a directory private to
	// the run (<runRoot>/_tfac/plugins/<index>/). Not a secret channel: it is
	// stored in plain text, and credential-shaped keys are refused.
	Env map[string]string `json:"env"`

	// GrantTools are tools merged into the run's allowlist for this plugin:
	// "Agent" or "Skill". Under the default permission mode neither tool
	// needs an allow rule today, so both are recorded and passed through but
	// do not change what a run can do.
	GrantTools []string `json:"grant_tools"`
}

// PluginsLoaded is what a run's wrapper reported about the plugins it was
// given, once Claude Code's init named the plugins it loaded. One per claim
// (claims.plugins_loaded), so a resume records its own.
type PluginsLoaded struct {
	// OK is false when a required plugin did not load, or loaded with an
	// error tied to it. The run is then failed with plugin_not_loaded.
	OK bool `json:"ok"`
	// Plugins are the configured plugins that loaded, as Claude Code named
	// them.
	Plugins []LoadedPlugin `json:"plugins,omitempty"`
	// Failed are the configured plugins that did not load cleanly: every
	// required one when OK is false, and any non-required one either way.
	Failed []PluginFailure `json:"failed,omitempty"`
}

// LoadedPlugin is one plugin Claude Code reported loaded.
type LoadedPlugin struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path"`
	// Required echoes the entry's Required setting.
	Required bool `json:"required,omitempty"`
}

// PluginFailure is one configured plugin that did not load cleanly, with why.
type PluginFailure struct {
	Path     string `json:"path"`
	Reason   string `json:"reason"`
	Required bool   `json:"required,omitempty"`
}
