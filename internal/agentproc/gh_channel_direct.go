package agentproc

import (
	"os"
	"path/filepath"
	"strings"
)

// directGHChannelEnv builds the env entries that point the real gh binary at a
// per-run injector on the DIRECT (local, non-sandbox) spawn path — the sibling
// of ghChannelEnv, which serves the jail.
//
// The five channel variables are identical in both modes, so the agent-facing
// surface is one surface: GH_HOST names the injector (gh forces https to it and
// verifies against the trust file), GH_ENTERPRISE_TOKEN carries the per-run
// PLACEHOLDER — never a real credential, in either mode — SSL_CERT_FILE points
// at the trust file, and the two suppressors keep gh non-interactive and off the
// release-check network.
//
// Two entries exist only here, because only here is there a user to be confused
// with:
//
//   - PATH leads with the channel's own bin dir, so `gh` resolves to the
//     TF-owned pinned binary. Without it the shell would find the user's own
//     gh, which is authenticated as the user — the exact identity this channel
//     exists to keep an agent off.
//   - GH_CONFIG_DIR points at an empty per-run directory, because a local agent
//     runs under the user's real HOME and gh would otherwise read their
//     ~/.config/gh, hosts entry and personal credential included.
//
// Empty when the channel is off, which is how a run with no provisioned binary
// degrades to the scoped exec verbs.
func directGHChannelEnv(gc *GHChannelParams) []string {
	if gc == nil || gc.Host == "" || gc.CertSourcePath == "" {
		return nil
	}
	env := []string{
		"GH_HOST=" + gc.Host,
		"GH_ENTERPRISE_TOKEN=" + gc.Token,
		"SSL_CERT_FILE=" + gc.CertSourcePath,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
	if gc.ConfigDir != "" {
		env = append(env, "GH_CONFIG_DIR="+gc.ConfigDir)
	}
	if gc.BinDir != "" {
		env = append(env, "PATH="+prependPath(gc.BinDir, os.Getenv("PATH")))
	}
	return env
}

// prependPath puts dir first in a PATH list, dropping any later copy of it so
// repeated application is stable.
func prependPath(dir, current string) string {
	dir = filepath.Clean(dir)
	if current == "" {
		return dir
	}
	kept := make([]string, 0, strings.Count(current, string(os.PathListSeparator))+1)
	for _, p := range filepath.SplitList(current) {
		if p == "" || filepath.Clean(p) == dir {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(append([]string{dir}, kept...), string(os.PathListSeparator))
}
