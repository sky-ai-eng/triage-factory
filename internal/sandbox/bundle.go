package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// writeBundle constructs the per-run OCI bundle directory on disk:
//
//	$TMPDIR/tf-bundle-<conversationID>-<rand>/
//	├── config.json     (the OCI spec)
//	├── resolv.conf     (1.1.1.1/8.8.8.8 — bind-mounted at /etc/resolv.conf)
//	└── rootfs/         (symlink to the cached alpine minirootfs)
//
// The rootfs is a SYMLINK (not a copy) to the cached extraction at
// ~/.triagefactory/sandbox/rootfs-<sha>/ — saves 5MB of file copies
// per run and keeps the bundle dir tiny. runsc handles symlinks
// fine.
//
// Returns the bundle dir path so the caller can pass it to
// `runsc run --bundle <dir>`. Caller must remove via cleanupBundle
// (called from Sandbox.Close) when the run finishes.
//
// Appends a synthesized resolv.conf mount to spec.Mounts before
// writing config.json — the resolv.conf path inside the bundle dir
// isn't knowable until writeBundle has run, so buildSpec leaves the
// mount slot empty and we fill it here.
func writeBundle(cfg Config, spec *specs.Spec, rootfsPath string) (string, error) {
	// Per-invocation unique bundle dir via MkdirTemp's random suffix.
	// ConversationID isn't guaranteed unique per call (some callers pass fixed
	// TraceIDs like "scorer-batch"), so a ConversationID-only path would let
	// concurrent runs delete each other's bundle. ConversationID stays in the
	// prefix purely for operator-grep-friendly naming.
	bundleDir, err := os.MkdirTemp(os.TempDir(), "tf-bundle-"+sanitizeBundlePrefix(cfg.ConversationID)+"-")
	if err != nil {
		return "", fmt.Errorf("bundle: mkdtemp: %w", err)
	}

	// Symlink the cached rootfs into the bundle.
	rootfsLink := filepath.Join(bundleDir, "rootfs")
	if err := os.Symlink(rootfsPath, rootfsLink); err != nil {
		return "", fmt.Errorf("bundle: symlink rootfs: %w", err)
	}

	// Synthesize resolv.conf and append the bind mount to the spec.
	resolvPath, err := writeResolvConf(bundleDir)
	if err != nil {
		return "", fmt.Errorf("bundle: resolv.conf: %w", err)
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/etc/resolv.conf",
		Type:        "bind",
		Source:      resolvPath,
		Options:     []string{"rbind", "ro"},
	})

	// Write the spec last so it reflects all the mount appends.
	if err := specOnDisk(spec, bundleDir); err != nil {
		return "", fmt.Errorf("bundle: %w", err)
	}
	return bundleDir, nil
}

// cleanupBundle removes the per-run bundle dir. Idempotent.
func cleanupBundle(bundleDir string) error {
	if bundleDir == "" {
		return nil
	}
	return os.RemoveAll(bundleDir)
}

// sanitizeBundlePrefix strips characters that would break MkdirTemp's
// path-safe assumption or make grep noisy. Replaces any non-[a-z0-9-]
// with '-' and clips length so the final dir name stays well under
// any OS path limits.
func sanitizeBundlePrefix(s string) string {
	const maxLen = 24
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+32)
		default:
			b = append(b, '-')
		}
	}
	if len(b) == 0 {
		return "run"
	}
	return string(b)
}
