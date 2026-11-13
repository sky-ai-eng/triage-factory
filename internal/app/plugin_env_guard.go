package app

import (
	"errors"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// checkPluginEnvAllowPrefixes runs the operator's TF_PLUGIN_ENV_ALLOW_PREFIXES
// against the installed agent SDK's own CLAUDE_* keys and logs each prefix it
// refuses. A refused prefix is not honored, but it does not stop the boot: the
// plugin env keys under it are dropped at spawn, as they are with no prefix
// configured, and the log names why.
//
// An SDK that is not installed yet is skipped. EnsureSDK runs the same check
// after its install, before any run spawns.
func (a *App) checkPluginEnvAllowPrefixes() {
	sdkDir := paths.SDKDir()
	if _, err := os.Stat(sdkDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			appLog.Debug("agent SDK not installed yet; plugin env prefixes are checked after its install", "sdk_dir", sdkDir)
			return
		}
		appLog.Error("plugin env prefixes not checked: cannot read the agent SDK directory", "sdk_dir", sdkDir, "error", err)
		return
	}
	if err := agentproc.VerifyPluginEnvAllowPrefixes(sdkDir); err != nil {
		appLog.Error("plugin env allow prefixes refused", "error", err)
	}
}
