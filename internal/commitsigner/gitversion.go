package commitsigner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The first git release that signs with an SSH key (gpg.format=ssh).
const (
	minSigningGitMajor = 2
	minSigningGitMinor = 34
)

// gitProbeTimeout bounds `git version`, which answers in milliseconds.
const gitProbeTimeout = 5 * time.Second

// Why a local run whose org has an active signer still commits unsigned. They
// are GET commit-identity's unsigned_reason.
const (
	// UnsignedGitTooOld: the git on this host predates SSH signing, so the
	// run commits as the signer, unsigned.
	UnsignedGitTooOld = "git_too_old"
	// UnsignedGitUnavailable: this host's git could not be run to read its
	// version.
	UnsignedGitUnavailable = "git_unavailable"
)

// LocalGit is what a local run's git can do, as ProbeLocalGit found it.
// Version is git's own version string, "" when git could not be run.
type LocalGit struct {
	Version string
	// UnsignedReason is UnsignedGitTooOld or UnsignedGitUnavailable when this
	// git cannot sign commits with an SSH key, and "" when it can.
	UnsignedReason string
}

// ProbeLocalGit runs the git on PATH, the one a local run's agent runs, and
// reports whether it signs with SSH keys. Multi mode never asks: its jail's
// git is the image's, well past the release that added it.
func ProbeLocalGit(ctx context.Context) LocalGit {
	ctx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "version").Output()
	if err != nil {
		signerLog.WarnContext(ctx, "could not run git to read its version; local runs commit unsigned", "error", err)
		return LocalGit{UnsignedReason: UnsignedGitUnavailable}
	}
	return parseGitVersion(strings.TrimSpace(string(out)))
}

// parseGitVersion reads `git version` output: "git version 2.43.0", or with a
// vendor suffix such as "git version 2.39.3 (Apple Git-146)".
func parseGitVersion(out string) LocalGit {
	version := strings.TrimPrefix(out, "git version ")
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
		signerLog.Warn("could not read git's version; local runs commit unsigned", "output", out)
		return LocalGit{Version: version, UnsignedReason: UnsignedGitUnavailable}
	}
	if major < minSigningGitMajor || major == minSigningGitMajor && minor < minSigningGitMinor {
		return LocalGit{Version: version, UnsignedReason: UnsignedGitTooOld}
	}
	return LocalGit{Version: version}
}

// Unsigned is id committing as the same identity without signing: the run
// still authors its commits as the signer, as a run whose signer is not
// active does.
func (id Identity) Unsigned() Identity {
	id.Mode = domain.CommitSigningNone
	id.Host, id.PublicKey, id.PendingPublicKey = "", "", ""
	return id
}
