// Package snapshotcapture implements the internal
// `triagefactory snapshot-capture <root> [<owner>/<repo>/<slug>...]`
// subcommand: it captures a run tree's non-recoverable state — every named
// checkout's git delta and the _tfac scratch — into a stream on descriptor
// sandbox.CaptureStreamFD, then writes the small worktree.CaptureManifest to
// stdout as JSON. Undocumented in --help, like `hook`.
//
// It runs in exactly one place: the capture jail the cap-broker launches for
// each snapshot (sandbox.LaunchCaptureJail), with the run tree mounted
// read-only and no network. That is the point of it. The capture's git honors
// the clean/smudge/textconv/diff drivers and the attributes the checkout's
// config and .gitattributes name — both of which the agent wrote — so it has
// to run somewhere that executing them costs nothing. The jail holds no
// credential and no other run's tree, and the capture cannot write the tree it
// reads. This package is the unprivileged body that just captures and emits.
package snapshotcapture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Handle dispatches `triagefactory snapshot-capture <root> [<checkout>...]`.
func Handle(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: triagefactory snapshot-capture <root> [<owner>/<repo>/<slug>...]")
		os.Exit(2)
	}
	stream := os.NewFile(uintptr(sandbox.CaptureStreamFD), sandbox.CaptureStreamFile)
	if stream == nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture: the capture stream descriptor is missing")
		os.Exit(1)
	}
	defer func() { _ = stream.Close() }()
	if err := Run(context.Background(), args[0], args[1:], stream, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-capture:", err)
		os.Exit(1)
	}
}

// Run captures root's checkouts and scratch, framing the large members onto
// stream and writing the manifest as JSON to w. The testable core of Handle,
// separated from argv parsing, descriptors and os.Exit so the capture body can
// be exercised without a jail.
func Run(ctx context.Context, root string, checkouts []string, stream io.Writer, w io.Writer) error {
	sw := sandbox.NewCaptureStreamWriter(stream)
	members := &memberWriters{sw: sw}
	var man worktree.CaptureManifest

	for _, rel := range checkouts {
		path := filepath.Join(root, filepath.FromSlash(rel))
		bundle := members.lazy(worktree.CaptureBundleMember(rel))
		patch := members.lazy(worktree.CapturePatchMember(rel))
		delta, hasBundle, hasPatch, err := worktree.CaptureWorkspaceGitToWriters(ctx, path, bundle, patch)
		if err != nil {
			return fmt.Errorf("capture %s: %w", rel, err)
		}
		if err := members.finish(); err != nil {
			return err
		}
		man.Checkouts = append(man.Checkouts, worktree.CheckoutCapture{Rel: rel, Delta: delta, HasBundle: hasBundle, HasPatch: hasPatch})
	}

	scratchFiles := 0
	digest, omitted, unreadable, err := worktree.ScratchDigest(ctx, root, func(rel, path string, fi os.FileInfo) error {
		name := worktree.CaptureScratchPrefix + rel
		if len(name) > sandbox.CaptureMemberNameMax || strings.ContainsRune(name, 0) {
			return fmt.Errorf("%w: name of %d bytes", worktree.ErrScratchSkipped, len(name))
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		m, err := sw.Member(name)
		if err != nil {
			return err
		}
		// At most the size the walk saw: a file that grew since is captured as
		// it was at the stat, and one that shrank is captured as far as it
		// goes. The framing carries no size ahead of the bytes, so a member
		// that ends early is a shorter file, never a torn stream.
		if _, err := io.Copy(m, io.LimitReader(f, fi.Size())); err != nil {
			_ = m.Close()
			return fmt.Errorf("copy scratch %s: %w", rel, err)
		}
		if err := m.Close(); err != nil {
			return err
		}
		scratchFiles++
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk scratch: %w", err)
	}
	man.ScratchDigest, man.ScratchFiles, man.CILogsOmitted, man.Unreadable = digest, scratchFiles, omitted, unreadable

	if err := sw.Close(); err != nil {
		return fmt.Errorf("flush capture stream: %w", err)
	}
	if err := json.NewEncoder(w).Encode(man); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}

// memberWriters opens a stream member on a writer's first write, so a bundle
// or patch git has nothing to put in leaves no member behind and the manifest's
// HasBundle and HasPatch stay the single statement of what is there. Members
// are strictly sequential in the framing, so opening one closes the last.
type memberWriters struct {
	sw  *sandbox.CaptureStreamWriter
	cur io.WriteCloser
}

func (m *memberWriters) lazy(name string) io.Writer { return &lazyMember{m: m, name: name} }

func (m *memberWriters) finish() error {
	if m.cur == nil {
		return nil
	}
	err := m.cur.Close()
	m.cur = nil
	return err
}

type lazyMember struct {
	m    *memberWriters
	name string
	open bool
}

func (l *lazyMember) Write(p []byte) (int, error) {
	if !l.open {
		if err := l.m.finish(); err != nil {
			return 0, err
		}
		w, err := l.m.sw.Member(l.name)
		if err != nil {
			return 0, err
		}
		l.m.cur, l.open = w, true
	}
	return l.m.cur.Write(p)
}
