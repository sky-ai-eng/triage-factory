package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// localCommitSigning is one local engagement's commit signing: the git config
// that has every commit it makes signed by its signer, the allowed-signers file
// that config names, and what its daemon signs with. nil for a run that does
// not sign, and every method is nil-safe so the caller wires it
// unconditionally.
type localCommitSigning struct {
	signing        agenthost.LocalCommitSigning
	allowedSigners string
	envEntry       string
}

// prepareLocalCommitSigning sets up commit signing for a local engagement whose
// org resolved to signer when it started, or returns nil when the run does
// not sign: the signer is not active, this host's git predates SSH signing, or
// the allowed-signers file or the signing hand-off could not be written. A run
// that does not sign still commits as the signer, unsigned.
func (s *Spawner) prepareLocalCommitSigning(ctx context.Context, orgID, conversationID, claimID string, signer commitsigner.Identity) *localCommitSigning {
	if !signer.Mode.Signs() || runmode.Current() != runmode.ModeLocal {
		return nil
	}
	if git := commitsigner.ProbeLocalGit(ctx); git.UnsignedReason != "" {
		delegateLog.Warn("the org has an active commit signer, but this host's git cannot sign with an SSH key; the run commits as the signer, unsigned",
			"org", orgID, "conversation", conversationID, "git_version", git.Version, "reason", git.UnsignedReason)
		return nil
	}
	_, bootEpoch := s.executorIdentity()
	c := &localCommitSigning{
		signing: agenthost.LocalCommitSigning{
			ClaimID:   claimID,
			BootEpoch: bootEpoch,
			Signer:    signer,
			Budgets:   s.commitSigningBudgets(),
		},
		allowedSigners: paths.AllowedSignersPath(claimID),
	}
	entry, err := c.signing.EnvEntry()
	if err == nil {
		c.envEntry = entry
		err = writeAllowedSigners(c.allowedSigners, githooks.AllowedSigners(signer.Email, signer.PublicKey, signer.PendingPublicKey))
	}
	if err != nil {
		delegateLog.Error("setting up commit signing failed; the run commits as the signer, unsigned",
			"org", orgID, "conversation", conversationID, "error", err)
		return nil
	}
	return c
}

// writeAllowedSigners writes an engagement's allowed-signers file. The
// directory is the operator's alone; the file holds only public keys.
func writeAllowedSigners(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the allowed-signers dir: %w", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write the allowed-signers file: %w", err)
	}
	return nil
}

// configPairs is the git config that signs the run's commits, for its single
// GIT_CONFIG_* block. A local run reads the hooks dir and the allowed-signers
// file at their host paths, in or out of the local sandbox.
func (c *localCommitSigning) configPairs() [][2]string {
	if c == nil {
		return nil
	}
	return githooks.SigningConfigPairs(githooks.HostDir(), c.signing.Signer.PublicKey, c.allowedSigners)
}

// env carries the signing to the exec CLI that serves a run with the local
// sandbox off.
func (c *localCommitSigning) env() []string {
	if c == nil {
		return nil
	}
	return []string{c.envEntry}
}

// daemon is what the local sandbox's daemon signs with.
func (c *localCommitSigning) daemon() *agenthost.LocalCommitSigning {
	if c == nil {
		return nil
	}
	signing := c.signing
	return &signing
}

// allowedSignersPath is the file the local sandbox binds read-only.
func (c *localCommitSigning) allowedSignersPath() string {
	if c == nil {
		return ""
	}
	return c.allowedSigners
}

// Close removes the engagement's allowed-signers file.
func (c *localCommitSigning) Close() {
	if c == nil {
		return
	}
	if err := os.Remove(c.allowedSigners); err != nil && !os.IsNotExist(err) {
		delegateLog.Warn("removing an engagement's allowed-signers file failed", "path", c.allowedSigners, "error", err)
	}
}
