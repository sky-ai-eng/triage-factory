// Team knowledge staged into a run's tree: the task team's own knowledge base
// plus every other team's published root, copied in before the agent starts,
// and the manifest that tells it what arrived.
//
// Which knowledge a run gets is a pure function of the task's team id. There is
// no model in the path, nothing to wait on, and no cross-pod call: the team is
// a column the task already carries, and the two roots under it are the whole
// visibility model.

package delegate

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
)

// The knowledge tree the agent reads, inside the run's scratch dir:
//
//	_tfac/knowledge/team/private/…    the task team's own notes
//	_tfac/knowledge/team/shared/…     what the task team has published
//	_tfac/knowledge/org/<team>/…      what every OTHER team has published
//
// The task team's two roots keep their names because the difference is real to
// the agent's readers: what it learns from `shared/` is knowledge the whole org
// can see, and what it learns from `private/` is not. Other teams contribute
// only their published root, so there is no root level under org/ to name —
// the team is the distinction there.
const (
	knowledgeDirName     = "knowledge"
	knowledgeTeamDirName = "team"
	knowledgeOrgDirName  = "org"

	// knowledgeWarnBytes is the soft cap on one run's staged knowledge. Over it
	// the run still gets everything — trimming an org's knowledge is a human
	// decision, not one a copy step makes silently — but the log says so, since
	// past this point the copy is a real cost on every claim and most of it is
	// unlikely to be read.
	knowledgeWarnBytes = 2 * 1024 * 1024

	// knowledgeSummaryMaxLen caps how much of a document's first line rides the
	// manifest. Long enough to say what a file is about, short enough that a
	// hundred of them stay a table of contents rather than a document.
	knowledgeSummaryMaxLen = 110

	// knowledgeManifestMaxLines bounds the rendered manifest. A team that has
	// filed a thousand documents gets a truncated listing plus a line saying
	// how many were elided, rather than a manifest that crowds out the task.
	knowledgeManifestMaxLines = 200
)

// stagedKnowledge is what one launch copied in: the entries, grouped by the
// directory they landed under, in the order the manifest renders them.
type stagedKnowledge struct {
	// groups are the staged sets in render order — the task team first, then
	// each contributing team's published root.
	groups []knowledgeGroup
	files  int
	bytes  int64
}

// knowledgeGroup is one heading in the manifest: a directory under
// _tfac/knowledge/ and the entries staged beneath it.
type knowledgeGroup struct {
	// dir is the group's path relative to _tfac/knowledge/ ("team",
	// "org/platform").
	dir string
	// label says whose knowledge this is, in the words a person would use.
	label   string
	entries []stagedEntry
}

// stagedEntry is one document as the manifest names it: its path relative to
// the group's directory, and the first line of its content.
type stagedEntry struct {
	path    string
	summary string
}

// stageTeamKnowledge copies this run's knowledge into the run tree and returns
// the rendered manifest, or "" when there was nothing to stage.
//
// teamID is the task's team. It resolves the whole answer: that team's KB in
// full, and every other active team's published root. A task with no team (or
// a spawner with no KB seam wired) stages nothing, which is the same tree an
// org with no knowledge produces.
//
// Every launch REBUILDS the tree rather than adding to it: whatever a previous
// launch into this run root staged is cleared first, so the tree is what the
// knowledge base holds now and nothing else.
//
// Advisory throughout, like the prior-memory materializer beside it: a store
// failure, a mkdir failure or a per-file copy failure is logged and skipped.
// An agent running without staged knowledge is still useful; a run that failed
// because someone's runbook was unreadable is not.
//
// The manifest is rendered from what this call actually wrote, never from the
// listing — so it cannot name a document the agent will not find, and because
// the tree is rebuilt it cannot omit one the agent can.
func (s *Spawner) stageTeamKnowledge(ctx context.Context, orgID, teamID, cwd string, owned repoFiles) string {
	kb := s.TeamKB()
	if kb == nil || teamID == "" {
		return ""
	}
	root := filepath.Join(cwd, scratchDirName, knowledgeDirName)
	// A full rebuild, not an overlay. See clearStagedKnowledge for why the
	// difference is load-bearing rather than tidiness.
	clearStagedKnowledge(root, owned)

	staged := stagedKnowledge{}
	s.stageOneKnowledgeSet(ctx, kb, orgID, teamID, kbstore.Roots(), root,
		knowledgeTeamDirName, "your team's knowledge base", &staged, owned)

	for _, other := range s.otherTeamsWithKnowledge(ctx, orgID, teamID) {
		dir := path.Join(knowledgeOrgDirName, other.Slug)
		s.stageOneKnowledgeSet(ctx, kb, orgID, other.ID, []kbstore.Root{kbstore.RootShared}, root,
			dir, "published by "+other.Name, &staged, owned)
	}

	if staged.files == 0 {
		return ""
	}
	if staged.bytes > knowledgeWarnBytes {
		delegateLog.Warn("staged team knowledge is over the soft cap; consider trimming what the org publishes",
			"team", teamID, "files", staged.files, "bytes", staged.bytes, "soft_cap", knowledgeWarnBytes)
	}
	delegateLog.Info("staged team knowledge for run", "team", teamID, "files", staged.files, "bytes", staged.bytes)
	return renderKnowledgeManifest(staged)
}

// clearStagedKnowledge empties the knowledge tree so this launch can rebuild
// it from the store. Three things make that a correctness step rather than
// housekeeping, and the second is the sharp one:
//
//   - A document deleted from the knowledge base would otherwise stay readable
//     in a tree that no longer lists it. The manifest is rendered from what was
//     copied, so a leftover is invisible in the prompt and present on disk —
//     the worst combination, since the block tells the agent to walk the tree.
//   - A document PUBLISHED between two launches would appear under both roots
//     at once: the old copy under `team/private/`, the new one under
//     `team/shared/`. The root a document sits under is a claim about who else
//     can see it, so a tree carrying both teaches the agent something false
//     about material it may go on to quote in a pull request.
//   - Anything the agent itself left here is discarded, which is what the
//     framework block promises. Without the clear that promise held only for
//     the first launch into a run root.
//
// A blueprint's steps share one run tree, so this fires between them; a warm
// step whose tree already belongs to the sandbox identity never reaches here at
// all (the caller skips staging entirely, and the first step's copy stands).
//
// Repo-owned paths are left alone, for the same reason nothing writes them: for
// a GitHub PR run this tree IS the checkout, and removing a tracked file here
// would ride the agent's next commit into its pull request. That is also why
// this is a walk rather than one RemoveAll — the blunt form cannot make that
// distinction, and a repo that tracks a file under our directory is exactly the
// case the rest of this package already bends around.
//
// Best-effort: a path that cannot be removed is logged and left, which is the
// state a launch that skipped this would have had anyway.
func clearStagedKnowledge(root string, owned repoFiles) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if owned.owns(append([]string{knowledgeDirName}, strings.Split(filepath.ToSlash(rel), "/")...)...) {
			return nil
		}
		if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
			delegateLog.Warn("clear staged knowledge file failed; the agent may read a stale copy", "path", p, "error", rmErr)
		}
		return nil
	})
	if err != nil {
		if !os.IsNotExist(err) {
			delegateLog.Warn("clear staged knowledge failed; the agent may read stale copies", "path", root, "error", err)
		}
		return
	}
	// Deepest first, so a directory is only attempted once its children are
	// gone. os.Remove refuses a non-empty one, which is precisely the signal to
	// keep it — a directory holding a repo-owned file stays, and so does every
	// directory above it.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
}

// stageOneKnowledgeSet copies one team's roots into dir (relative to the
// knowledge root) and records what landed. Entries are staged under the same
// relative shape they have in the KB, so a folder a team filed knowledge into
// is a folder the agent walks.
func (s *Spawner) stageOneKnowledgeSet(
	ctx context.Context,
	kb kbstore.KB,
	orgID, teamID string,
	roots []kbstore.Root,
	knowledgeRoot, dir, label string,
	staged *stagedKnowledge,
	owned repoFiles,
) {
	entries, err := kb.List(ctx, orgID, teamID, roots, "")
	if err != nil {
		delegateLog.Warn("list team knowledge failed; this run stages none of it", "team", teamID, "error", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	group := knowledgeGroup{dir: dir, label: label}
	for _, e := range entries {
		// Under the team's own directory the two roots are named; a
		// contributing team's set is all published, so its entries sit
		// directly under the team's directory with no redundant level.
		rel := e.Path
		if len(roots) > 1 {
			rel = path.Join(string(e.Root), e.Path)
		}
		// The repo owns anything git tracks under the scratch dir, and for a
		// GitHub PR run this tree IS the checkout — a write there would ride
		// the agent's next commit into its pull request.
		if owned.owns(append([]string{knowledgeDirName}, strings.Split(path.Join(dir, rel), "/")...)...) {
			continue
		}
		dst := filepath.Join(knowledgeRoot, filepath.FromSlash(dir), filepath.FromSlash(rel))
		n, summary, err := s.copyKnowledgeFile(ctx, kb, orgID, teamID, e.Ref(), dst)
		if err != nil {
			delegateLog.Warn("stage knowledge file failed", "team", teamID, "ref", e.Ref().String(), "error", err)
			continue
		}
		group.entries = append(group.entries, stagedEntry{path: rel, summary: summary})
		staged.files++
		staged.bytes += n
	}
	if len(group.entries) == 0 {
		return
	}
	sort.Slice(group.entries, func(i, j int) bool { return group.entries[i].path < group.entries[j].path })
	staged.groups = append(staged.groups, group)
}

// copyKnowledgeFile streams one KB entry to dst, returning the bytes written
// and the document's first line as its summary. The summary is taken from the
// stream as it passes rather than by re-reading the file, so a large document
// is read once.
func (s *Spawner) copyKnowledgeFile(ctx context.Context, kb kbstore.KB, orgID, teamID string, ref kbstore.Ref, dst string) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, "", fmt.Errorf("create knowledge dir: %w", err)
	}
	src, err := kb.Open(ctx, orgID, teamID, ref)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = src.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, "", fmt.Errorf("create knowledge file: %w", err)
	}
	head := &headCapture{limit: 4 * knowledgeSummaryMaxLen}
	n, copyErr := io.Copy(io.MultiWriter(out, head), src)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		// A failed copy leaves a truncated file behind, and the caller's next
		// move is to log this entry and skip it — which would leave a document
		// the manifest never mentions sitting in a tree the agent is told to
		// walk. Half a runbook read as a whole one is worse than an absent
		// one, so the fragment goes.
		if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
			delegateLog.Warn("remove partially staged knowledge file failed; the agent may read a truncated document",
				"path", dst, "error", rmErr)
		}
		if copyErr != nil {
			return n, "", copyErr
		}
		return n, "", closeErr
	}
	return n, firstLineSummary(head.String()), nil
}

// otherTeamsWithKnowledge lists the org's other active teams, whose published
// roots this run is staged with. Archived teams are excluded: archiving winds a
// team down, and a run's context should reflect the org as it is rather than
// growing with its history. Their knowledge is still readable by a person on
// the Knowledge page — this is about what a run is handed, not about what
// exists.
//
// A team whose slug is not a single usable directory name falls back to its id,
// so the staged tree can never gain a level or escape its root because of what
// somebody named their team.
func (s *Spawner) otherTeamsWithKnowledge(ctx context.Context, orgID, teamID string) []domain.Team {
	if s.teams == nil {
		return nil
	}
	all, err := s.teams.ListActiveForOrgSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("list org teams for knowledge staging failed; this run stages only its own team's knowledge",
			"org", orgID, "error", err)
		return nil
	}
	out := make([]domain.Team, 0, len(all))
	for _, t := range all {
		if t.ID == teamID {
			continue
		}
		if kbstore.ValidatePath(t.Slug) != nil || strings.Contains(t.Slug, "/") {
			t.Slug = t.ID
		}
		if t.Name == "" {
			t.Name = t.Slug
		}
		out = append(out, t)
	}
	return out
}

// renderKnowledgeManifest turns what was staged into the lines that ride the
// run's own context. It is per-run data by construction — which documents an
// org happens to hold today — so it never belongs in a framework block, whose
// text has to be byte-identical across runs to stay a cacheable prefix.
func renderKnowledgeManifest(staged stagedKnowledge) string {
	var b strings.Builder
	b.WriteString("Team knowledge staged for this run, under `_tfac/knowledge/` in your run root. " +
		"It is written by the humans on these teams — architecture notes, conventions, runbooks, gotchas. " +
		"Read what looks relevant to your mission before you start. " +
		"It can have drifted from the code, so where the two disagree the code wins. " +
		"It is reference material for this run only: nothing you write there is kept.\n")

	lines := 0
	elided := 0
	for _, g := range staged.groups {
		if lines >= knowledgeManifestMaxLines {
			elided += len(g.entries)
			continue
		}
		fmt.Fprintf(&b, "\n%s/ — %s\n", path.Join(knowledgeDirName, g.dir), g.label)
		lines++
		for _, e := range g.entries {
			if lines >= knowledgeManifestMaxLines {
				elided++
				continue
			}
			if e.summary == "" {
				fmt.Fprintf(&b, "  %s\n", e.path)
			} else {
				fmt.Fprintf(&b, "  %s — %s\n", e.path, e.summary)
			}
			lines++
		}
	}
	if elided > 0 {
		fmt.Fprintf(&b, "\n(%d more file(s) staged but not listed here — walk `_tfac/%s/` to see them all.)\n",
			elided, knowledgeDirName)
	}
	return strings.TrimRight(b.String(), "\n")
}

// firstLineSummary reduces a document's opening to one line of manifest. It
// takes the first line with anything on it, strips a leading markdown heading
// marker or list bullet, collapses whitespace, and truncates.
//
// Externally-authored in the sense that matters here: written by members of the
// org, not by the internet — but still text that ends up in a prompt, so it is
// flattened to a single line rather than pasted, and it cannot introduce a
// section boundary or a newline of its own.
func firstLineSummary(head string) string {
	for _, raw := range strings.Split(head, "\n") {
		line := strings.TrimSpace(strings.ReplaceAll(raw, "\r", ""))
		line = strings.TrimLeft(line, "#>-*+ \t")
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if !utf8.ValidString(line) {
			// A binary document — an uploaded diagram or screenshot — has no
			// first line to quote. It still gets a manifest row, just without
			// a summary; putting its bytes in the prompt instead would be
			// noise at best and an invalid request body at worst.
			return ""
		}
		return truncateRunes(line, knowledgeSummaryMaxLen)
	}
	return ""
}

// truncateRunes cuts a summary to at most max BYTES without splitting a rune.
//
// The bound is in bytes because that is what bounds the prompt, but the cut has
// to fall on a rune boundary: a document whose first line is not ASCII would
// otherwise contribute a half-encoded character to <run_context>, which is
// mojibake at best and, for anything that validates its input, a whole request
// spoiled by one advisory line about one file.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := 0
	for i := range s {
		if i > max {
			break
		}
		cut = i
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// headCapture keeps the first bytes written through it and discards the rest —
// enough of a document to find its first line without buffering the whole
// thing while it streams to disk.
type headCapture struct {
	limit int
	buf   []byte
	done  bool
}

func (h *headCapture) Write(p []byte) (int, error) {
	if !h.done {
		room := h.limit - len(h.buf)
		if room > 0 {
			if room > len(p) {
				room = len(p)
			}
			h.buf = append(h.buf, p[:room]...)
		}
		// One newline is all firstLineSummary needs; past it there is nothing
		// left to find.
		if len(h.buf) >= h.limit || strings.ContainsRune(string(h.buf), '\n') {
			h.done = true
		}
	}
	return len(p), nil
}

// String returns the captured head with a trailing partial rune dropped. The
// capture stops at a byte count, so the last character can be half-written;
// leaving it in would put an invalid sequence into the summary the same way an
// unaligned truncation would.
//
// Bounded to the three bytes an incomplete sequence can be, rather than
// scanning back for the first valid prefix. Invalid bytes further in belong to
// the DOCUMENT — it is binary, or not UTF-8 — and are not this cut's to
// rewrite; firstLineSummary declines to summarize those rather than trimming
// its way through them.
func (h *headCapture) String() string {
	buf := h.buf
	for i := 0; i < utf8.UTFMax-1 && len(buf) > 0; i++ {
		if r, size := utf8.DecodeLastRune(buf); r != utf8.RuneError || size != 1 {
			break
		}
		buf = buf[:len(buf)-1]
	}
	return string(buf)
}
