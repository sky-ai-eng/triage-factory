package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type streamMember struct{ name, body string }

// stubJailCapture installs a capture jail that answers with manifest and
// streams members into the staging file the way the real one would.
func stubJailCapture(t *testing.T, manifest worktree.CaptureManifest, members []streamMember, truncate int) {
	t.Helper()
	orig := captureViaSandbox
	t.Cleanup(func() { captureViaSandbox = orig })
	captureViaSandbox = func(_ context.Context, p sandbox.CaptureParams) ([]byte, error) {
		var buf bytes.Buffer
		w := sandbox.NewCaptureStreamWriter(&buf)
		for _, m := range members {
			mw, err := w.Member(m.name)
			if err != nil {
				return nil, err
			}
			_, _ = io.WriteString(mw, m.body)
			_ = mw.Close()
		}
		_ = w.Close()
		data := buf.Bytes()
		if truncate > 0 {
			data = data[:len(data)-truncate]
		}
		if err := os.WriteFile(filepath.Join(p.StagingDir, sandbox.CaptureStreamFile), data, 0o600); err != nil {
			return nil, err
		}
		return json.Marshal(manifest)
	}
}

func jailWrite() snapshotWrite {
	return snapshotWrite{
		wtPath: "/tmp/tf-run",
		checkouts: []snapshotCheckout{{
			repoID: "acme/app", slug: "default", rel: "acme/app/default", path: "/tmp/tf-run/acme/app/default",
		}},
	}
}

// goodHead is a well-formed commit id.
const goodHead = "0123456789abcdef0123456789abcdef01234567"

func goodManifest() worktree.CaptureManifest {
	return worktree.CaptureManifest{
		Checkouts: []worktree.CheckoutCapture{{
			Rel: "acme/app/default", Delta: &worktree.GitDelta{Branch: "work", Head: goodHead}, HasBundle: true, HasPatch: true,
		}},
		ScratchDigest: "digest", ScratchFiles: 2, CILogsOmitted: true,
	}
}

func goodMembers() []streamMember {
	return []streamMember{
		{worktree.CaptureBundleMember("acme/app/default"), "bundle-bytes"},
		{worktree.CapturePatchMember("acme/app/default"), "patch-bytes"},
		{worktree.CaptureScratchPrefix + "notes.txt", "scratch note"},
		{worktree.CaptureScratchPrefix + "sub/deep.txt", "deep"},
	}
}

func TestCaptureTreeInJail_UnpacksWhatTheJailStreamed(t *testing.T) {
	stubJailCapture(t, goodManifest(), goodMembers(), 0)
	captured := &capturedSnapshot{}
	defer captured.release()
	if err := captureTreeInJail(context.Background(), jailWrite(), captured); err != nil {
		t.Fatalf("captureTreeInJail: %v", err)
	}
	if len(captured.checkouts) != 1 {
		t.Fatalf("checkouts = %d", len(captured.checkouts))
	}
	st := captured.checkouts[0].state
	for path, want := range map[string]string{st.BundlePath: "bundle-bytes", st.PatchPath: "patch-bytes"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if st.Delta == nil || st.Delta.Head != goodHead {
		t.Errorf("delta = %+v", st.Delta)
	}
	sc := captured.scratch
	if sc == nil || sc.digest != "digest" || !sc.ciLogsOmitted {
		t.Fatalf("scratch = %+v", sc)
	}
	for rel, want := range map[string]string{"notes.txt": "scratch note", "sub/deep.txt": "deep"} {
		if got, err := os.ReadFile(filepath.Join(sc.root, worktree.ScratchDir, rel)); err != nil || string(got) != want {
			t.Errorf("scratch %s = %q, %v", rel, got, err)
		}
	}
}

// TestCaptureTreeInJail_RefusesWhatTheJailMayNotSay: the jail ran git over
// files the agent wrote, so its output is held to exactly what was asked.
func TestCaptureTreeInJail_RefusesWhatTheJailMayNotSay(t *testing.T) {
	cases := []struct {
		name     string
		manifest func(*worktree.CaptureManifest)
		members  func([]streamMember) []streamMember
		truncate int
	}{
		{"member nobody asked for", nil, func(m []streamMember) []streamMember {
			return append(m, streamMember{"checkouts/other/repo/default/bundle", "x"})
		}, 0},
		{"scratch member escaping the scratch", nil, func(m []streamMember) []streamMember {
			return append(m, streamMember{worktree.CaptureScratchPrefix + "../escape", "x"})
		}, 0},
		{"absolute scratch member", nil, func(m []streamMember) []streamMember {
			return append(m, streamMember{worktree.CaptureScratchPrefix + "/etc/passwd", "x"})
		}, 0},
		{"unclean scratch member", nil, func(m []streamMember) []streamMember {
			return append(m, streamMember{worktree.CaptureScratchPrefix + "a/../b", "x"})
		}, 0},
		{"scratch member named twice", nil, func(m []streamMember) []streamMember {
			return append(m, streamMember{worktree.CaptureScratchPrefix + "notes.txt", "again"})
		}, 0},
		{"scratch file under another file", nil, func(m []streamMember) []streamMember {
			return append(m, streamMember{worktree.CaptureScratchPrefix + "notes.txt/inner", "x"})
		}, 0},
		{"announced member missing", nil, func(m []streamMember) []streamMember { return m[1:] }, 0},
		{"manifest names another checkout", func(m *worktree.CaptureManifest) { m.Checkouts[0].Rel = "acme/app/other" }, nil, 0},
		{"manifest names too many checkouts", func(m *worktree.CaptureManifest) {
			m.Checkouts = append(m.Checkouts, m.Checkouts[0])
		}, nil, 0},
		{"members announced for no git state", func(m *worktree.CaptureManifest) { m.Checkouts[0].Delta = nil }, nil, 0},
		{"buffered members on the manifest", func(m *worktree.CaptureManifest) { m.Checkouts[0].Delta.Patch = []byte("x") }, nil, 0},
		{"stream cut short", nil, nil, 3},
		{"stream cut at a scratch member boundary", nil, func(m []streamMember) []streamMember { return m[:len(m)-1] }, 0},
		{"manifest announces more scratch files", func(m *worktree.CaptureManifest) { m.ScratchFiles = 3 }, nil, 0},
		{"manifest announces fewer scratch files", func(m *worktree.CaptureManifest) { m.ScratchFiles = 1 }, nil, 0},
		{"HEAD that is not an object id", func(m *worktree.CaptureManifest) { m.Checkouts[0].Delta.Head = "abc" }, nil, 0},
		{"HEAD that is an option", func(m *worktree.CaptureManifest) { m.Checkouts[0].Delta.Head = "--upload-pack=x" }, nil, 0},
		{"branch that is an option", func(m *worktree.CaptureManifest) { m.Checkouts[0].Delta.Branch = "-fx" }, nil, 0},
		{"branch git would not accept", func(m *worktree.CaptureManifest) { m.Checkouts[0].Delta.Branch = "a..b" }, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			man := goodManifest()
			if tc.manifest != nil {
				tc.manifest(&man)
			}
			members := goodMembers()
			if tc.members != nil {
				members = tc.members(members)
			}
			stubJailCapture(t, man, members, tc.truncate)
			captured := &capturedSnapshot{}
			defer captured.release()
			if err := captureTreeInJail(context.Background(), jailWrite(), captured); err == nil {
				t.Fatal("accepted the jail's output")
			}
		})
	}
}

func TestValidScratchRel(t *testing.T) {
	for _, ok := range []string{"a", "a/b", "dir/file.txt", ".hidden/x"} {
		if !validScratchRel(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "/abs", "a/../b", "a//b", "a/", "x\x00y"} {
		if validScratchRel(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestCaptureTreeInJail_FailureReleasesStaging: a capture that fails leaves no
// staging behind once the snapshot releases it.
func TestCaptureTreeInJail_FailureReleasesStaging(t *testing.T) {
	var staged string
	orig := captureViaSandbox
	t.Cleanup(func() { captureViaSandbox = orig })
	captureViaSandbox = func(_ context.Context, p sandbox.CaptureParams) ([]byte, error) {
		staged = p.StagingDir
		return nil, io.ErrUnexpectedEOF
	}
	captured := &capturedSnapshot{}
	if err := captureTreeInJail(context.Background(), jailWrite(), captured); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("err = %v", err)
	}
	captured.release()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staging %s survived the release (%v)", staged, err)
	}
}
