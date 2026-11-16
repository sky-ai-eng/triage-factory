package agentproc

import (
	"os"
	"path/filepath"
	"strings"
)

// directGHChannelEnv builds the env entries that point the real gh binary at a
// per-run injector on the DIRECT (local, non-sandbox) spawn path — the sibling
// of the jail's channel env, which cell.JailEnv assembles.
//
// The five channel variables are identical in both modes, so the agent-facing
// surface is one surface: GH_HOST names the injector (see
// GHChannelParams.Host), GH_ENTERPRISE_TOKEN carries the per-run
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
//   - GH_CONFIG_DIR points at a per-run directory, because a local agent runs
//     under the user's real HOME and gh would otherwise read their
//     ~/.config/gh, hosts entry and personal credential included. It holds at
//     most one setting, the one that sends gh's requests to the injector's
//     unix socket.
//
// An empty config dir is not enough on its own. gh takes a token from the
// environment first and the OS keyring next, and the keyring lookup does not go
// through the config dir, so a user who ran `gh auth login` would still be
// reachable. GH_TOKEN therefore always carries ghNoCredentialToken, which is
// not a credential: a github.com request made around GH_HOST (an explicit
// --hostname, a GH_REPO naming github.com) presents it and is refused instead of
// reaching the keyring. A channel that could not start (no pinned binary for
// the platform, a failed fetch, no GitHub URL) arrives with only ConfigDir set,
// and its run degrades to the scoped exec verbs; the gh on its PATH is then the
// user's own, so GH_ENTERPRISE_TOKEN carries the same refusing value and nothing
// of the user's is left for it to act with. isGitHubCLIEnvKey is the remaining
// half: newDirectCommand drops those keys from the inherited env.
//
// Empty when there is no channel at all.
func directGHChannelEnv(gc *GHChannelParams) []string {
	if gc == nil {
		return nil
	}
	// A listener with no trust file is no channel on this path: pointing gh at
	// it would fail every TLS handshake instead of degrading cleanly.
	// The refusing tokens are unconditional here: newDirectCommand has already
	// dropped the inherited ones for any non-nil channel, so leaving them unset
	// would hand gh straight to the keyring.
	if !gc.Live() || gc.CertSourcePath == "" {
		env := ghRefusingTokenEnv("GH_TOKEN", "GH_ENTERPRISE_TOKEN")
		env = append(env, "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
		if gc.ConfigDir != "" {
			env = append(env, "GH_CONFIG_DIR="+gc.ConfigDir)
		}
		return env
	}
	env := []string{
		"GH_HOST=" + gc.Host,
		"GH_ENTERPRISE_TOKEN=" + gc.Token,
		"SSL_CERT_FILE=" + gc.CertSourcePath,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
	env = append(env, ghRefusingTokenEnv("GH_TOKEN")...)
	if gc.ConfigDir != "" {
		env = append(env, "GH_CONFIG_DIR="+gc.ConfigDir)
	}
	if gc.BinDir != "" {
		env = append(env, "PATH="+prependPath(gc.BinDir, os.Getenv("PATH")))
	}
	return env
}

// ghNoCredentialToken is what gh presents when TF has no credential to give it.
// It is not a credential anywhere: GitHub rejects it, and its only job is to
// stand in front of the keyring, which gh would read in its absence.
const ghNoCredentialToken = "triagefactory-no-github-credential"

// ghRefusingTokenEnv sets each named gh token variable to ghNoCredentialToken.
func ghRefusingTokenEnv(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+ghNoCredentialToken)
	}
	return out
}

// isGitHubCLIEnvKey reports whether an inherited variable configures gh or
// carries a GitHub token: every GH_ key, and the two token names gh reads
// beside them, which other tools read too. A local run reaches GitHub as the
// org's configured identity, through the gh channel, so none of these from
// the operator's environment may reach the agent, with a channel or without
// one. directGHChannelEnv adds the channel's own after they are dropped.
func isGitHubCLIEnvKey(key string) bool {
	return strings.HasPrefix(key, "GH_") || key == "GITHUB_TOKEN" || key == "GITHUB_ENTERPRISE_TOKEN"
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
		if p == "" || samePathEntry(filepath.Clean(p), dir) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(append([]string{dir}, kept...), string(os.PathListSeparator))
}
