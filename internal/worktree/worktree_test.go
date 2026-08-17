package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupPlainCheckout creates a fake worktree with a .git directory (plain
// checkout layout, not linked worktree). Returns the worktree root and the
// path where .git/info/exclude lives so tests can pre-populate or assert
// against it directly.
func setupPlainCheckout(t *testing.T) (wtDir, excludePath string) {
	t.Helper()
	wtDir = t.TempDir()
	gitDir := filepath.Join(wtDir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0755); err != nil {
		t.Fatalf("mkdir .git/info: %v", err)
	}
	return wtDir, filepath.Join(gitDir, "info", "exclude")
}

// setupLinkedWorktree creates a fake linked worktree: .git is a pointer
// file containing "gitdir: <externalPath>" and the external gitdir has
// its own info/ directory. Matches how `git worktree add` sets things up.
func setupLinkedWorktree(t *testing.T) (wtDir, excludePath string) {
	t.Helper()
	root := t.TempDir()
	wtDir = filepath.Join(root, "worktree")
	extGitDir := filepath.Join(root, "bare.git", "worktrees", "wt-42")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir wtDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(extGitDir, "info"), 0755); err != nil {
		t.Fatalf("mkdir external gitdir/info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+extGitDir+"\n"), 0644); err != nil {
		t.Fatalf("write .git pointer: %v", err)
	}
	return wtDir, filepath.Join(extGitDir, "info", "exclude")
}

func TestWriteLocalExcludes_CreatesFileWhenMissing(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	s := string(content)

	if !strings.Contains(s, "_tfac/") {
		t.Errorf("missing _tfac/ pattern: %q", s)
	}
	if !strings.Contains(s, managedExcludeBegin) || !strings.Contains(s, managedExcludeEnd) {
		t.Errorf("missing marker pair: %q", s)
	}
}

// TestWriteLocalExcludes_PreservesExistingContent is the core regression
// test for issue 1: any pre-existing content in .git/info/exclude (user
// patterns, comments, other tool-managed lines) must survive untouched.
// The old unconditional-overwrite implementation would have failed this.
func TestWriteLocalExcludes_PreservesExistingContent(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// Pre-populate with user content — representative of what someone
	// might have added by hand or via another tool.
	userContent := `# git ls-files --others --exclude-from=.git/info/exclude
# Lines that start with '#' are comments.
# For a project mostly in C, the following would be a good set of
# exclude patterns (uncomment them if you want to use them):
# *.[oa]
# *~
node_modules/
*.swp
`
	if err := os.WriteFile(excludePath, []byte(userContent), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	gotStr := string(got)

	// Every line of the original user content must still be present.
	for _, line := range strings.Split(strings.TrimSpace(userContent), "\n") {
		if !strings.Contains(gotStr, line) {
			t.Errorf("user line %q was lost; file now:\n%s", line, gotStr)
		}
	}

	// Our managed pattern must be present too.
	if !strings.Contains(gotStr, "_tfac/") {
		t.Error("missing _tfac/ after append")
	}
}

// TestWriteLocalExcludes_Idempotent verifies that running the function
// twice against the same worktree doesn't duplicate entries. Any agent
// lifecycle that spins up a worktree, configures it, tears it down, and
// re-uses the same wtDir (re-delegation) would otherwise accumulate
// duplicate pattern lines.
func TestWriteLocalExcludes_Idempotent(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after first call: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("second call: %v", err)
	}
	secondContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after second call: %v", err)
	}

	if string(firstContent) != string(secondContent) {
		t.Errorf("file diverged between calls:\nfirst:\n%s\nsecond:\n%s", firstContent, secondContent)
	}

	// No pattern should appear more than once.
	for _, p := range managedExcludePatterns {
		count := strings.Count(string(secondContent), p)
		if count != 1 {
			t.Errorf("pattern %q appears %d times, want 1", p, count)
		}
	}
}

// TestWriteLocalExcludes_PartialExisting covers the case where one of our
// managed patterns is present in unrelated user content (e.g. the user
// added _tfac/ manually before we ran) and the other isn't. The
// managed block is always written as a complete manifest, so the user's
// line stays untouched AND our block appears with both patterns. Git
// dedupes duplicate lines internally, so two occurrences of _tfac/
// are functionally equivalent to one — the important invariant is
// "user content preserved, managed block complete."
func TestWriteLocalExcludes_PartialExisting(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// _tfac/ lives in user content; we still write the managed block.
	if err := os.WriteFile(excludePath, []byte("other-tool-pattern/\n_tfac/\n"), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)

	// User content preserved (both lines still present as whole lines)
	gotLines := strings.Split(s, "\n")
	wantUserLines := []string{"other-tool-pattern/", "_tfac/"}
	for _, want := range wantUserLines {
		found := false
		for _, line := range gotLines {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("user line %q lost; file:\n%s", want, s)
		}
	}

	// Managed block is present and complete — both patterns inside it.
	beginIdx := strings.Index(s, managedExcludeBegin)
	endIdx := strings.Index(s, managedExcludeEnd)
	if beginIdx < 0 || endIdx <= beginIdx {
		t.Fatalf("managed block markers missing or inverted; file:\n%s", s)
	}
	managedSection := s[beginIdx:endIdx]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing pattern %q; section:\n%s", p, managedSection)
		}
	}
}

// TestWriteLocalExcludes_GrowthReusesBlock is the regression guard for the
// "header duplication on pattern set growth" issue: if managedExcludePatterns
// grows from {A} to {A, B}, a later run should expand the existing managed
// block in place rather than appending a second block with its own header.
// Simulated here by writing a complete managed block containing a subset of
// the current managedExcludePatterns, then running writeLocalExcludes and
// verifying the block now contains the full set and the begin/end markers
// each appear exactly once.
func TestWriteLocalExcludes_GrowthReusesBlock(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// Simulate a previous run that only knew about the legacy pattern. Format matches
	// what writeLocalExcludes would produce — begin marker, patterns, end
	// marker — but with a subset of the current managedExcludePatterns.
	stale := "user-pattern/\n\n" +
		managedExcludeBegin + "\n" +
		legacyScratchDir + "/\n" +
		managedExcludeEnd + "\n"
	if err := os.WriteFile(excludePath, []byte(stale), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)

	// Markers must appear exactly once — not duplicated by a new block.
	if n := strings.Count(s, managedExcludeBegin); n != 1 {
		t.Errorf("begin marker appears %d times, want 1; file:\n%s", n, s)
	}
	if n := strings.Count(s, managedExcludeEnd); n != 1 {
		t.Errorf("end marker appears %d times, want 1; file:\n%s", n, s)
	}

	// Managed block should now contain the full set (expanded in place).
	beginIdx := strings.Index(s, managedExcludeBegin)
	endIdx := strings.Index(s, managedExcludeEnd)
	managedSection := s[beginIdx:endIdx]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing %q after growth; section:\n%s", p, managedSection)
		}
	}

	// User content outside the block must survive.
	if !strings.Contains(s, "user-pattern/") {
		t.Errorf("user line lost after growth rewrite; file:\n%s", s)
	}
}

// TestWriteLocalExcludes_RejectsMissingGitdir is the regression guard
// for the "valid prefix but bogus target" case. A .git pointer file that
// starts with "gitdir:" but references a path that doesn't exist (or
// isn't a directory) would previously pass the textual prefix check and
// then silently get its info/ parent created by the write path's
// MkdirAll, writing to an arbitrary location. The fix stats the
// resolved gitdir before returning and rejects if it's missing or not a
// directory.
func TestWriteLocalExcludes_RejectsMissingGitdir(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, wtDir string)
	}{
		{
			"gitdir path does not exist",
			func(t *testing.T, wtDir string) {
				bogus := filepath.Join(t.TempDir(), "does-not-exist")
				if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+bogus+"\n"), 0644); err != nil {
					t.Fatalf("write .git: %v", err)
				}
			},
		},
		{
			"gitdir path is a file, not a directory",
			func(t *testing.T, wtDir string) {
				fileTarget := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(fileTarget, []byte("regular file"), 0644); err != nil {
					t.Fatalf("write target file: %v", err)
				}
				if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+fileTarget+"\n"), 0644); err != nil {
					t.Fatalf("write .git: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtDir := t.TempDir()
			tc.setup(t, wtDir)

			err := writeLocalExcludes(wtDir)
			if err == nil {
				t.Fatal("expected error on bogus gitdir, got nil")
			}
			// Error should make the cause diagnosable — mention either
			// "missing gitdir" or "not a directory".
			msg := err.Error()
			if !strings.Contains(msg, "missing gitdir") && !strings.Contains(msg, "not a directory") {
				t.Errorf("error should mention missing/invalid gitdir, got: %v", err)
			}
		})
	}
}

// TestWriteLocalExcludes_IgnoresExtraLinesInPointer verifies that a .git
// pointer file with content past the first newline still parses
// correctly — git's canonical format is one line, but some tools append
// extra config to the same file, and we should read only the first line.
func TestWriteLocalExcludes_IgnoresExtraLinesInPointer(t *testing.T) {
	root := t.TempDir()
	wtDir := filepath.Join(root, "worktree")
	extGitDir := filepath.Join(root, "bare.git", "worktrees", "wt-xyz")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir wtDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(extGitDir, "info"), 0755); err != nil {
		t.Fatalf("mkdir ext gitdir: %v", err)
	}
	// Pointer with garbage after the first newline — should be ignored.
	pointerContent := "gitdir: " + extGitDir + "\n" +
		"# this line is not part of the pointer\n" +
		"corrupted garbage " + strings.Repeat("X", 200) + "\n"
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(pointerContent), 0644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The managed patterns should land in the correct external info/exclude.
	content, err := os.ReadFile(filepath.Join(extGitDir, "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	s := string(content)
	for _, p := range managedExcludePatterns {
		if !strings.Contains(s, p) {
			t.Errorf("managed pattern %q missing; file:\n%s", p, s)
		}
	}
}

func TestWriteLocalExcludes_LinkedWorktreePointer(t *testing.T) {
	wtDir, excludePath := setupLinkedWorktree(t)

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read external exclude: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "_tfac/") {
		t.Errorf("managed patterns not written through pointer file; got:\n%s", s)
	}
}

// TestWriteLocalExcludes_RejectsMalformedGitPointer is the regression guard
// for a real bug: strings.TrimPrefix is a silent no-op when the prefix
// isn't present, so a corrupted or non-pointer .git file like
// "malicious-path/" would have been interpreted as the literal gitdir
// path, causing us to write info/exclude to an arbitrary location
// relative to the worktree. The fix explicitly requires the trimmed
// content to start with "gitdir:".
func TestWriteLocalExcludes_RejectsMalformedGitPointer(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no gitdir prefix", "some-other-path/"},
		{"plain path that looks like a relative dir", "../../etc"},
		{"random garbage", "kjsdfhkjshdf"},
		{"partial prefix", "gitdi: /some/path"},
		{"case-wrong prefix", "GITDIR: /some/path"},
		{"empty file", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtDir := t.TempDir()
			// Write the malformed pointer file
			if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(tc.content), 0644); err != nil {
				t.Fatalf("write .git: %v", err)
			}

			err := writeLocalExcludes(wtDir)
			if err == nil {
				t.Fatalf("expected error on malformed pointer %q, got nil", tc.content)
			}
			// Error message should be diagnostic — mention it's not a
			// valid pointer rather than some downstream "no such file".
			if !strings.Contains(err.Error(), "not a valid worktree pointer") &&
				!strings.Contains(err.Error(), "empty gitdir") {
				t.Errorf("error should mention invalid pointer, got: %v", err)
			}

			// Crucially: no file should have been created anywhere based
			// on the corrupted content. Check that nothing matching
			// "info/exclude" exists under the tempdir beyond the .git
			// file we wrote ourselves.
			var unexpected []string
			_ = filepath.Walk(wtDir, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				if info.IsDir() {
					return nil
				}
				if path == filepath.Join(wtDir, ".git") {
					return nil
				}
				unexpected = append(unexpected, path)
				return nil
			})
			if len(unexpected) > 0 {
				t.Errorf("malformed pointer caused unexpected writes: %v", unexpected)
			}
		})
	}
}

// TestWriteLocalExcludes_StrayEndMarkerBeforeBlock is the regression
// guard for a subtle mergeManagedBlock bug: strings.Index finds the
// *first* occurrence, so if the end marker string appeared anywhere in
// the file before the actual begin marker (e.g., inside a user comment
// that pastes the marker verbatim, or stale content from a broken
// previous run), the pair check `endIdx > beginIdx` would fail and we'd
// fall to the append path, duplicating the managed block every run.
//
// The fix searches for the end marker only after the begin marker's
// position, so stray earlier occurrences are ignored. This test
// pre-populates a file with a stray end marker before a valid managed
// block, runs writeLocalExcludes twice, and verifies:
//   - The managed block is rewritten in place (not duplicated)
//   - The stray end marker text is preserved (it's user content, not ours)
//   - Idempotent: second run produces the same file
func TestWriteLocalExcludes_StrayEndMarkerBeforeBlock(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// User content that happens to include our end marker string —
	// maybe as a quoted example in a comment, maybe as leftover from a
	// truncated previous managed block that someone hand-edited. The
	// real managed block sits *after* this stray mention.
	stray := "# example of a triagefactory block looks like:\n" +
		"# " + managedExcludeEnd + "\n" +
		"node_modules/\n\n" +
		managedExcludeBegin + "\n" +
		legacyScratchDir + "/\n" +
		managedExcludeEnd + "\n"
	if err := os.WriteFile(excludePath, []byte(stray), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}
	firstStr := string(firstContent)

	// The begin marker should appear exactly once (ours, rewritten in
	// place). The end marker appears twice: once in the stray comment
	// line (preserved user content) and once as the actual block
	// terminator. Any more than that means we appended a duplicate.
	if n := strings.Count(firstStr, managedExcludeBegin); n != 1 {
		t.Errorf("begin marker count = %d, want 1 (stray end mention caused duplicate append?)\nfile:\n%s", n, firstStr)
	}
	if n := strings.Count(firstStr, managedExcludeEnd); n != 2 {
		t.Errorf("end marker count = %d, want 2 (one stray comment line + one real block)\nfile:\n%s", n, firstStr)
	}

	// User content preserved
	if !strings.Contains(firstStr, "node_modules/") {
		t.Errorf("user line 'node_modules/' lost; file:\n%s", firstStr)
	}
	if !strings.Contains(firstStr, "# example of a triagefactory block looks like:") {
		t.Errorf("stray user comment lost; file:\n%s", firstStr)
	}

	// Managed block now contains both patterns (growth in place)
	beginIdx := strings.Index(firstStr, managedExcludeBegin)
	searchFrom := beginIdx + len(managedExcludeBegin)
	relEnd := strings.Index(firstStr[searchFrom:], managedExcludeEnd)
	if relEnd < 0 {
		t.Fatalf("end marker not found after begin; file:\n%s", firstStr)
	}
	managedSection := firstStr[beginIdx : searchFrom+relEnd]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing pattern %q; section:\n%s", p, managedSection)
		}
	}

	// Idempotent: second run produces identical content. If the stray
	// end marker still confused us, we'd append on every run and the
	// files would differ.
	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("second call: %v", err)
	}
	secondContent, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read after second: %v", err)
	}
	if string(secondContent) != firstStr {
		t.Errorf("file diverged between calls:\nfirst:\n%s\n\nsecond:\n%s", firstStr, string(secondContent))
	}
}

// TestWriteLocalExcludes_StrayBeginBeforeBlock is the regression guard
// for the stray-begin / "user content clobber" case. If the file has an
// orphaned begin marker earlier (truncated block whose end was removed,
// stale fragment from a broken previous run), matching the *first*
// begin with the real end marker would treat everything in between as
// "our content" and overwrite it — including legitimate user lines.
//
// Using LastIndex for begin locks onto the most recent occurrence so
// any earlier stray markers and the user content around them stay
// untouched.
func TestWriteLocalExcludes_StrayBeginBeforeBlock(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// File state that would trigger the bug pre-fix:
	//   1. A standalone (orphaned) begin marker with no matching end
	//   2. User content between the orphan and the real block
	//   3. A valid begin+end pair (the real managed block)
	//
	// Before the LastIndex fix, strings.Index(existing, begin) would
	// return the orphan's position, the end search would find the real
	// end, and the replace would wipe out the user content between.
	stale := managedExcludeBegin + "\n" + // orphaned begin, no end
		"node_modules/\n" +
		"*.swp\n" +
		"\n" +
		managedExcludeBegin + "\n" + // real begin
		legacyScratchDir + "/\n" +
		managedExcludeEnd + "\n"
	if err := os.WriteFile(excludePath, []byte(stale), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)

	// User content between the orphan and the real block must survive.
	// This is the core assertion — the old first-begin behavior would
	// have eaten both lines.
	for _, line := range []string{"node_modules/", "*.swp"} {
		foundLine := false
		for _, l := range strings.Split(s, "\n") {
			if l == line {
				foundLine = true
				break
			}
		}
		if !foundLine {
			t.Errorf("user line %q was clobbered; file:\n%s", line, s)
		}
	}

	// The real managed block has been expanded in place. The orphaned
	// begin earlier in the file is left alone — we'd need a bigger
	// cleanup pass to remove orphaned markers, and that's out of scope
	// for this fix.
	beginIdx := strings.LastIndex(s, managedExcludeBegin)
	searchFrom := beginIdx + len(managedExcludeBegin)
	relEnd := strings.Index(s[searchFrom:], managedExcludeEnd)
	if relEnd < 0 {
		t.Fatalf("end marker not found after the last begin; file:\n%s", s)
	}
	managedSection := s[beginIdx : searchFrom+relEnd]
	for _, p := range managedExcludePatterns {
		if !strings.Contains(managedSection, p) {
			t.Errorf("managed block missing %q; section:\n%s", p, managedSection)
		}
	}
}

func TestWriteLocalExcludes_AppendsTrailingNewlineWhenMissing(t *testing.T) {
	wtDir, excludePath := setupPlainCheckout(t)

	// Existing file does not end with a newline.
	if err := os.WriteFile(excludePath, []byte("no-trailing-newline/"), 0644); err != nil {
		t.Fatalf("pre-populate: %v", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)

	// The begin marker must appear on its own line, not mashed onto the
	// end of the unterminated user line.
	if !strings.Contains(s, "\n"+managedExcludeBegin) {
		t.Errorf("begin marker not on its own line; file:\n%s", s)
	}
	// The original pattern must still be findable as a whole line.
	foundLine := false
	for _, line := range strings.Split(s, "\n") {
		if line == "no-trailing-newline/" {
			foundLine = true
			break
		}
	}
	if !foundLine {
		t.Errorf("original unterminated line was corrupted; file:\n%s", s)
	}
}

// --- CleanupWithOptions tests -----------------------------------------

// TestCleanupWithOptions_SkipClaudeProjectCleanup is the non-local-mode
// path: the parked-worktree preserve set can't be determined, so we skip
// ALL ~/.claude/projects deletions. Worktree dirs and bare-repo pruning
// should still run — those leaks compound fast and aren't
// session-state-sensitive.
func TestCleanupWithOptions_SkipClaudeProjectCleanup(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", home)

	// One worktree dir + a corresponding ~/.claude/projects entry.
	wtDir := filepath.Join(tmp, runsDir, "run-skip")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(wtDir)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	encoded := encodeClaudeProjectDir(resolved)
	projectDir := filepath.Join(home, claudeProjectsDir, encoded)
	jsonlPath := filepath.Join(projectDir, "session.jsonl")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte("data"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	CleanupWithOptions(CleanupOptions{SkipClaudeProjectCleanup: true})

	// Worktree dir gone — the leak we care about.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should have been removed even with SkipClaudeProjectCleanup (err=%v)", err)
	}
	// Project dir preserved — we don't know if it belongs to a
	// takeover, so we err on the side of "leave it alone."
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Errorf("session JSONL should have been preserved with SkipClaudeProjectCleanup (err=%v)", err)
	}
}

// TestEncodeClaudeProjectDir_ReplacesDots is the headline regression
// test for the encoding bug. Empirically (Claude Code 2.1.119) every
// '/' AND every '.' in the resolved cwd becomes '-'. Triage Factory's
// own paths almost always contain dots — the takeover destination is
// ~/.triagefactory/takeovers/run-<id> — so a slash-only encoding
// silently misses Claude Code's actual lookup path and resume fails
// with "No conversation found." This test pins down both characters
// so the rule can't quietly regress.
func TestEncodeClaudeProjectDir_ReplacesDots(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/private/tmp/repro-orig", "-private-tmp-repro-orig"},
		{"/private/tmp/dot.in.middle", "-private-tmp-dot-in-middle"},
		{"/private/tmp/v1.2.3-rc", "-private-tmp-v1-2-3-rc"},
		// The case that actually broke in production: leading dot in
		// `.triagefactory` produces a `--` (two dashes in a row) where
		// the slash-after-dot used to live. Slash-only would have
		// produced just `-.triagefactory-...`.
		{"/Users/aidan/.triagefactory/takeovers/run-x", "-Users-aidan--triagefactory-takeovers-run-x"},
		{"/home/user/.triagefactory/takeovers/run-x", "-home-user--triagefactory-takeovers-run-x"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := encodeClaudeProjectDir(tc.in)
			if got != tc.want {
				t.Errorf("encodeClaudeProjectDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRemoveAt_HandlesNonCanonicalPath guards review-comment fix #3:
// callers hold the source worktree path explicitly. If they used
// Remove(conversationID) (which derives the path from the canonical /tmp
// layout), they'd silently target the wrong directory whenever the
// source is elsewhere — leaking the actual source on disk and possibly
// destroying an unrelated conversationID's canonical dir.
//
// RemoveAt takes the path explicitly so this can't happen.
func TestRemoveAt_HandlesNonCanonicalPath(t *testing.T) {
	noncanonicalSrc := filepath.Join(t.TempDir(), "elsewhere", "wt-x")
	if err := os.MkdirAll(noncanonicalSrc, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Drop a marker file inside so we can verify the right dir is gone.
	if err := os.WriteFile(filepath.Join(noncanonicalSrc, "marker"), []byte("x"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if err := RemoveAt(noncanonicalSrc, "run-x"); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}
	if _, err := os.Stat(noncanonicalSrc); !os.IsNotExist(err) {
		t.Errorf("non-canonical src should have been removed (err=%v)", err)
	}
}

// TestRemoveAt_EmptyPath is a no-op — guards against a caller passing
// an empty path (e.g., a no-worktree run that somehow reached this
// code path) from accidentally trying to RemoveAll("").
func TestRemoveAt_EmptyPath(t *testing.T) {
	if err := RemoveAt("", "run-y"); err != nil {
		t.Errorf("RemoveAt with empty path should be a no-op, got: %v", err)
	}
}

// TestCleanupWithOptions_NilPreserveSet is safe (no panic) and behaves
// like the legacy Cleanup() — every orphan's project dir gets nuked.
// Map reads on nil maps return the zero value in Go, so the index
// expression `opts.PreserveClaudeProjectFor[conversationID]` returns false and
// every run is treated as non-preserved.
func TestCleanupWithOptions_NilPreserveSet(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", home)

	runDir := filepath.Join(tmp, runsDir, "run-nil")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CleanupWithOptions panicked on nil PreserveClaudeProjectFor: %v", r)
		}
	}()
	CleanupWithOptions(CleanupOptions{}) // PreserveClaudeProjectFor is nil

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir should have been removed (err=%v)", err)
	}
}
