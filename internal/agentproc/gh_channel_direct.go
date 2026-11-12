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
// of the user's is left for it to act with. ghInheritedEnvKeys is the remaining
// half: newDirectCommand drops those keys from the inherited env whenever a
// channel is present.
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

// ghInheritedEnvKeys are the variables gh takes a host, token or config dir
// from. A run with a gh channel inherits none of them from the operator's
// shell: directGHChannelEnv sets the ones the run needs itself.
var ghInheritedEnvKeys = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"GH_HOST",
	"GH_CONFIG_DIR",
}

// dropEnvKeysFold removes every entry whose name matches one of keys ignoring
// case. Windows resolves environment names case-insensitively, so an operator's
// `gh_token` is the GH_TOKEN gh reads there; an exact-case filter would let it
// through.
func dropEnvKeysFold(env, keys []string) []string {
	out := env[:0:0]
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		drop := false
		for _, k := range keys {
			if strings.EqualFold(name, k) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
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
