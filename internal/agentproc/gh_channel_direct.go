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
// Three entries exist only here, because only here is there a user to be
// confused with:
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
//   - GH_TOKEN carries ghNoCredentialToken. gh takes a host's token from the
//     environment first, its config dir next, and the OS keyring last, and the
//     keyring lookup needs only the host name, so an empty config dir still
//     leaves a user who ran `gh auth login` reachable. With it, a request to
//     github.com or a ghe.com tenancy that names its host around GH_HOST (an
//     explicit --hostname, a GH_REPO or URL naming it) presents a value GitHub
//     refuses instead of the user's keyring entry. A sandboxed run gets it
//     too: the sandbox masks paths but shares the network namespace, so a
//     session bus on an abstract socket still reaches the keyring. GITHUB_TOKEN
//     stays unset: gh reads it only when GH_TOKEN is unset, and the tools that
//     read it keep their anonymous path.
//
// A channel that could not start (no pinned binary for the platform, a failed
// fetch, no GitHub URL) arrives with only ConfigDir set, and its run degrades to
// the scoped exec verbs. The gh on its PATH is then the user's own, so it gets
// no GH_HOST and no PATH entry, and GH_ENTERPRISE_TOKEN carries the refusing
// value too, covering every host GH_TOKEN does not: nothing of the user's is
// left for that gh to act with.
//
// Empty when there is no channel at all.
func directGHChannelEnv(gc *GHChannelParams) []string {
	if gc == nil {
		return nil
	}
	// A listener with no trust file is no channel on this path: pointing gh at
	// it would fail every TLS handshake instead of degrading cleanly.
	if !gc.Live() || gc.CertSourcePath == "" {
		env := []string{
			"GH_TOKEN=" + ghNoCredentialToken,
			"GH_ENTERPRISE_TOKEN=" + ghNoCredentialToken,
			"GH_PROMPT_DISABLED=1",
			"GH_NO_UPDATE_NOTIFIER=1",
		}
		if gc.ConfigDir != "" {
			env = append(env, "GH_CONFIG_DIR="+gc.ConfigDir)
		}
		return env
	}
	env := []string{
		"GH_HOST=" + gc.Host,
		"GH_TOKEN=" + ghNoCredentialToken,
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

// ghNoCredentialToken is what a local run's gh presents for a host TF gives it
// no credential for. It is a credential nowhere: GitHub answers it with 401 Bad
// credentials, and its only job is to stand in front of the OS keyring, which
// gh reads when the variable is unset. Other readers of GH_TOKEN meet the same
// 401, public resources included, where they would have gone anonymous; the
// alternative is a run that can reach GitHub as the user.
const ghNoCredentialToken = "triagefactory-no-github-credential"

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
		if p == "" || filepath.Clean(p) == dir {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(append([]string{dir}, kept...), string(os.PathListSeparator))
}
