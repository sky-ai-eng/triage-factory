package pollbench

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
)

// baseBranch is the branch every pull request in the dataset targets.
const baseBranch = "main"

// world is what the fake serves that moves over a run: each repository's
// base-branch history, and each pull request's mergeability as GitHub has
// computed it. The server's mu guards it.
type world struct {
	ds    *Dataset
	repos []*repoWorld
	prs   [][]*prWorld
	rng   *rand.Rand
	// pushes counts head pushes, which date each push after the last.
	pushes int
	stats  WorldStats
}

// WorldStats is what the world's moves did over a run to the pull requests
// TF tracks (Repo.Listed), for the report and the sanity checks.
type WorldStats struct {
	// Moves is how many base moves landed, BaseOpenPRs the tracked pull
	// requests on their bases summed over them (what probing every pull
	// request on every move would read), and OverlapPRs the tracked pull
	// requests whose paths each move overlapped, summed.
	Moves       int
	BaseOpenPRs int
	OverlapPRs  int
	// HeadPushes is how many tracked pull requests' heads were pushed.
	HeadPushes int
	// Conflicted is the node ids of the tracked pull requests the moves
	// made CONFLICTING.
	Conflicted []string
}

// repoWorld is one repository's base branch: its commits and its tip.
type repoWorld struct {
	idx     int
	tip     string
	commits map[string]*commit
	// listGen moves the open-PR listing's ETag: a merged pull request or a
	// push changes the listing.
	listGen int
	// tree is the repository's files as the moves have left them, which the
	// next move draws from.
	tree *tree
}

// commit is one commit of a base branch: its parent, empty for the first,
// and what it changed against that parent.
type commit struct {
	sha, parent string
	changes     []domain.ChangedPath
}

// prWorld is one pull request's mergeability as GitHub holds it. GitHub
// computes an answer lazily: the answer it holds is for the head and base
// tip it was computed at, and a read once either has moved returns UNKNOWN
// and computes a new one, which is one test merge.
type prWorld struct {
	// truth is what a test merge would find now.
	truth string
	// answer is the last computed answer, at computedHead and computedTip.
	answer                    string
	computedHead, computedTip string
	// Why the answer may be stale, for attributing the next test merge:
	// the head was pushed or an overlapping move landed since it was
	// computed, or the last read answered UNKNOWN.
	pushed, overlapped, lastUnknown bool
}

func newWorld(ds *Dataset) *world {
	w := &world{ds: ds, rng: rand.New(rand.NewSource(ds.Cfg.Seed ^ worldSeedSalt))}
	for ri, repo := range ds.Repos {
		root := &commit{sha: commitSHA(ri, 0)}
		w.repos = append(w.repos, &repoWorld{
			idx:     ri,
			tip:     root.sha,
			commits: map[string]*commit{root.sha: root},
			tree:    repo.tree.clone(),
		})
		prs := make([]*prWorld, len(repo.PRs))
		for pi, pr := range repo.PRs {
			// Every pull request starts with an answer computed at its head
			// and the base's first tip: before TF starts, GitHub has
			// answered for every pull request someone looked at.
			prs[pi] = &prWorld{truth: pr.Mergeable, answer: pr.Mergeable, computedHead: pr.HeadSHA, computedTip: root.sha}
		}
		w.prs = append(w.prs, prs)
	}
	return w
}

// worldSeedSalt separates the world's draws (what each move changes, which
// overlapping pull requests it conflicts) from the dataset's.
const worldSeedSalt = 0x776f726c64

func commitSHA(repoIdx, seq int) string {
	return fmt.Sprintf("c%07x%032x", repoIdx, seq)
}

// MoveBase lands one commit on repository ri's base branch, the merge of a
// pull request outside the dataset: its changed files are drawn from the
// repository's tree (median 4, at most 30), the listing changes as a merge
// changes it, and each tracked pull request whose paths the move overlaps,
// under the tracker's own rule, becomes CONFLICTING with
// ServerConfig.ConflictOnOverlap. The move's paths are the ones the compare
// from the previous tip answers, so the branch watch reads the same move the
// overlap was decided on. A pull request past the listing's cap is never
// read, so its answer is left alone.
func (s *Server) MoveBase(ri int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.world
	rw := w.repos[ri]
	changes := rw.tree.draw(w.rng, drawQuantiles(w.rng, baseMoveFileCounts))
	rw.tree.apply(changes)
	c := &commit{sha: commitSHA(ri, len(rw.commits)), parent: rw.tip, changes: changes}
	rw.commits[c.sha] = c
	rw.tip = c.sha
	rw.listGen++

	movePaths := combine([]*commit{c})
	prs := s.ds.Repos[ri].Listed()
	w.stats.Moves++
	w.stats.BaseOpenPRs += len(prs)
	for pi, pr := range prs {
		if !tracker.Overlaps(pr.Paths, movePaths) {
			continue
		}
		st := w.prs[ri][pi]
		st.overlapped = true
		w.stats.OverlapPRs++
		if st.truth != domain.MergeableConflicting && w.rng.Float64() < s.cfg.ConflictOnOverlap {
			st.truth = domain.MergeableConflicting
			w.stats.Conflicted = append(w.stats.Conflicted, pr.NodeID)
		}
	}
}

// PushHead moves pull request pi of repository ri to a new head, with a
// later updated_at, and changes the repository's listing. Its changed paths
// stay as they were.
func (s *Server) PushHead(ri, pi int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.world
	w.pushes++
	pr := &s.ds.Repos[ri].PRs[pi]
	pr.HeadSHA = fmt.Sprintf("a%07x%032x", ri, w.pushes)
	pr.UpdatedAt = pushEpoch.Add(time.Duration(w.pushes) * time.Second).Format(time.RFC3339)
	w.prs[ri][pi].pushed = true
	w.repos[ri].listGen++
	if pi < len(s.ds.Repos[ri].Listed()) {
		w.stats.HeadPushes++
	}
}

// pushEpoch dates the first head push, after every generated timestamp.
var pushEpoch = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

// Stats reports what the world's moves have done so far.
func (s *Server) Stats() WorldStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.world.stats
	st.Conflicted = append([]string(nil), st.Conflicted...)
	return st
}

// readMergeability answers one PRMergeability read of pull request pi in
// repository ri, as GitHub does: the stored answer while it was computed at
// the current head and base tip, and otherwise UNKNOWN, after running one
// test merge whose answer the next read returns. c counts the test merge by
// what made the answer stale.
func (w *world) readMergeability(ri, pi int, c *Counters) string {
	st := w.prs[ri][pi]
	head, tip := w.ds.Repos[ri].PRs[pi].HeadSHA, w.repos[ri].tip
	if st.computedHead == head && st.computedTip == tip {
		st.lastUnknown = false
		return st.answer
	}
	c.TestMerges++
	switch {
	case st.pushed:
		c.TestMergesHeadPushed++
	case st.overlapped:
		c.TestMergesOverlap++
	case st.lastUnknown:
		c.TestMergesReread++
	default:
		c.TestMergesUnaccounted++
	}
	st.answer, st.computedHead, st.computedTip = st.truth, head, tip
	st.pushed, st.overlapped, st.lastUnknown = false, false, true
	return domain.MergeableUnknown
}

// comparison is one compare's answer, before GitHub's caps are applied.
type comparison struct {
	status   string
	behindBy int
	// commits is merge base..head, oldest first.
	commits []*commit
	// files is what those commits changed together, by path.
	files []domain.ChangedPath
}

// compare compares base...head as GitHub does: from their merge base to
// head. base and head are commit shas or the base branch's name. found is
// false when either is not a commit the repository holds.
func (rw *repoWorld) compare(base, head string) (cmp comparison, found bool) {
	resolve := func(ref string) *commit {
		if ref == baseBranch {
			ref = rw.tip
		}
		return rw.commits[ref]
	}
	b, h := resolve(base), resolve(head)
	if b == nil || h == nil {
		return comparison{}, false
	}
	// First-parent histories: every commit has at most one parent here.
	onBase := map[string]int{}
	for c, depth := b, 0; c != nil; c, depth = rw.commits[c.parent], depth+1 {
		onBase[c.sha] = depth
	}
	var ahead []*commit
	c := h
	for ; c != nil; c = rw.commits[c.parent] {
		if _, ok := onBase[c.sha]; ok {
			break
		}
		ahead = append(ahead, c)
	}
	if c == nil {
		return comparison{}, false
	}
	for i, j := 0, len(ahead)-1; i < j; i, j = i+1, j-1 {
		ahead[i], ahead[j] = ahead[j], ahead[i]
	}
	cmp = comparison{behindBy: onBase[c.sha], commits: ahead, files: combine(ahead)}
	switch {
	case len(ahead) == 0 && cmp.behindBy == 0:
		cmp.status = "identical"
	case cmp.behindBy == 0:
		cmp.status = "ahead"
	case len(ahead) == 0:
		cmp.status = "behind"
	default:
		cmp.status = "diverged"
	}
	return cmp, true
}

// combine is what a run of commits changed taken together: a file renamed
// and then modified is one rename, one added and then deleted is nothing, and
// a copy of a file that existed before the run keeps that file as its source.
func combine(commits []*commit) []domain.ChangedPath {
	type entry struct {
		origin string // the path before the run; empty for a file the run added
		// copiedFrom is, for a file the run added as a copy, the path its
		// source had before the run; empty when the source was added in the
		// run too.
		copiedFrom string
	}
	live := map[string]entry{}
	removed := map[string]bool{}
	for _, c := range commits {
		for _, ch := range c.changes {
			switch ch.Change {
			case domain.PathAdded, domain.PathCopied:
				switch {
				case removed[ch.Path]:
					delete(removed, ch.Path)
					live[ch.Path] = entry{origin: ch.Path}
				case ch.Change == domain.PathCopied:
					source, ok := live[ch.Previous]
					if !ok {
						source = entry{origin: ch.Previous}
					}
					live[ch.Path] = entry{copiedFrom: source.origin}
				default:
					live[ch.Path] = entry{}
				}
			case domain.PathDeleted:
				e, ok := live[ch.Path]
				delete(live, ch.Path)
				switch {
				case !ok:
					removed[ch.Path] = true
				case e.origin != "":
					removed[e.origin] = true
				}
			case domain.PathRenamed:
				e, ok := live[ch.Previous]
				if !ok {
					e = entry{origin: ch.Previous}
				}
				delete(live, ch.Previous)
				live[ch.Path] = e
			default:
				if _, ok := live[ch.Path]; !ok {
					live[ch.Path] = entry{origin: ch.Path}
				}
			}
		}
	}
	out := make([]domain.ChangedPath, 0, len(live)+len(removed))
	for path, e := range live {
		switch e.origin {
		case "":
			if e.copiedFrom != "" {
				out = append(out, domain.ChangedPath{Path: path, Change: domain.PathCopied, Previous: e.copiedFrom})
				continue
			}
			out = append(out, domain.ChangedPath{Path: path, Change: domain.PathAdded})
		case path:
			out = append(out, domain.ChangedPath{Path: path, Change: domain.PathModified})
		default:
			out = append(out, domain.ChangedPath{Path: path, Change: domain.PathRenamed, Previous: e.origin})
		}
	}
	for path := range removed {
		out = append(out, domain.ChangedPath{Path: path, Change: domain.PathDeleted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
