package pollbench

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The bench's file model: each repository has a seeded tree of directories
// and files, and a pull request's changed paths, like a base move's, are
// drawn from it. A change lands in a home directory drawn by a skewed
// popularity (the directory of rank r weighs 1/√r), with some of its files
// from any other directory, and a few hot files appear in a fixed share of
// changes each, as pkg/features/kube_features.go does in
// kubernetes/kubernetes. How often a move's paths overlap a pull request's
// follows from these: the mono shape's tree puts it at about 1.2% of the
// open pull requests per move, near the 1.1% measured on kubernetes.

// Quantile is one point of a distribution: the value V at cumulative
// probability P.
type Quantile struct {
	P, V float64
}

// KubernetesFileCounts is the changed-file count of an open pull request on
// kubernetes/kubernetes, as measured for the mergeability probing work:
// median 3, p90 17, p99 140, maximum 550.
var KubernetesFileCounts = []Quantile{{0, 1}, {0.5, 3}, {0.9, 17}, {0.99, 140}, {1, 550}}

// MonorepoTree is the mono shape's file tree, which with
// KubernetesFileCounts puts a base move's overlap set near kubernetes's.
var MonorepoTree = TreeConfig{Dirs: 1200, HotFiles: 3, HotFilePct: 0.05}

// baseMoveFileCounts is the file count of one base move: median 4, at most
// 30.
var baseMoveFileCounts = []Quantile{{0, 1}, {0.5, 4}, {0.9, 12}, {1, 30}}

// drawQuantiles draws a value from qs (ascending in P, from 0 to 1) by
// inverting the distribution, interpolating log-linearly between points, so
// a large sample's quantiles land on qs's.
func drawQuantiles(rng *rand.Rand, qs []Quantile) int {
	u := rng.Float64()
	for i := 1; i < len(qs); i++ {
		if u >= qs[i].P && i < len(qs)-1 {
			continue
		}
		lo, hi := qs[i-1], qs[i]
		t := (u - lo.P) / (hi.P - lo.P)
		return int(math.Round(math.Exp(math.Log(lo.V) + t*(math.Log(hi.V)-math.Log(lo.V)))))
	}
	return int(qs[len(qs)-1].V)
}

// TreeConfig shapes a repository's file tree.
type TreeConfig struct {
	// Dirs is the number of directories; each holds 4 to 24 files.
	Dirs int
	// HotFiles is how many hot files the tree has, each appearing in
	// HotFilePct of changes.
	HotFiles   int
	HotFilePct float64
}

// hotFilePaths are the hot files' paths, the first TreeConfig.HotFiles of
// them.
var hotFilePaths = []string{
	"pkg/features/features.go",
	"go.mod",
	"go.sum",
	"api/openapi-spec/swagger.json",
	"pkg/apis/core/types.go",
	"hack/verify-all.sh",
	"CHANGELOG.md",
	"docs/index.md",
}

var treeRoots = []string{"pkg", "cmd", "internal", "staging/src", "test", "api", "plugin", "hack"}

// tree is one repository's files, by directory.
type tree struct {
	dirs  []string
	files [][]string
	// cum is the directories' cumulative popularity, for drawing one.
	cum []float64
	hot []string
	cfg TreeConfig
}

func newTree(rng *rand.Rand, cfg TreeConfig) *tree {
	t := &tree{cfg: cfg, hot: hotFilePaths[:min(cfg.HotFiles, len(hotFilePaths))]}
	weights := make([]float64, cfg.Dirs)
	for i, rank := range rng.Perm(cfg.Dirs) {
		root := treeRoots[i%len(treeRoots)]
		dir := fmt.Sprintf("%s/mod%03d", root, i/len(treeRoots))
		if rng.Intn(2) == 0 {
			dir = fmt.Sprintf("%s/sub%d", dir, rng.Intn(4))
		}
		n := 4 + rng.Intn(21)
		files := make([]string, n)
		for j := range files {
			files[j] = fmt.Sprintf("%s/file%02d.go", dir, j)
		}
		t.dirs = append(t.dirs, dir)
		t.files = append(t.files, files)
		weights[i] = 1 / math.Sqrt(float64(rank+1))
	}
	sum := 0.0
	for _, w := range weights {
		sum += w
		t.cum = append(t.cum, sum)
	}
	return t
}

// clone copies the tree's file lists, so a copy can change as moves land
// while the dataset keeps the tree pull requests were drawn from.
func (t *tree) clone() *tree {
	c := *t
	c.files = make([][]string, len(t.files))
	for i, f := range t.files {
		c.files[i] = append([]string(nil), f...)
	}
	return &c
}

func (t *tree) drawDir(rng *rand.Rand) int {
	return sort.SearchFloat64s(t.cum, rng.Float64()*t.cum[len(t.cum)-1])
}

// draw picks n distinct changed paths: each hot file with HotFilePct, then
// files clustered in one home directory, as a change to one package is, with
// 30% from any directory. Each is modified, added, deleted, renamed or copied
// in proportions near a real repository's. A file is deleted or renamed only
// when it exists in t, so a tree that moves change keeps describing what is
// there; a hot file is only ever modified.
func (t *tree) draw(rng *rand.Rand, n int) []domain.ChangedPath {
	out := make([]domain.ChangedPath, 0, n)
	used := map[string]bool{}
	for _, h := range t.hot {
		if len(out) < n && rng.Float64() < t.cfg.HotFilePct {
			out = append(out, domain.ChangedPath{Path: h, Change: domain.PathModified})
			used[h] = true
		}
	}
	home := t.drawDir(rng)
	for attempts := 0; len(out) < n; attempts++ {
		d := home
		if rng.Float64() < 0.3 || attempts > 4*n {
			d = rng.Intn(len(t.dirs))
		}
		if attempts > 64*n {
			d = attempts % len(t.dirs)
		}
		files := t.files[d]
		switch p := rng.Float64(); {
		case p < 0.09 || len(files) == 0:
			path := fmt.Sprintf("%s/new%06d.go", t.dirs[d], rng.Intn(1_000_000))
			if !used[path] {
				used[path] = true
				out = append(out, domain.ChangedPath{Path: path, Change: domain.PathAdded})
			}
		default:
			f := files[rng.Intn(len(files))]
			if used[f] {
				continue
			}
			used[f] = true
			c := domain.ChangedPath{Path: f, Change: domain.PathModified}
			switch {
			case p < 0.13:
				c.Change = domain.PathDeleted
			case p < 0.145:
				c.Change, c.Previous = domain.PathRenamed, f
				c.Path = fmt.Sprintf("%s/moved%06d.go", t.dirs[t.drawDir(rng)], rng.Intn(1_000_000))
			case p < 0.15:
				c.Change, c.Previous = domain.PathCopied, f
				c.Path = fmt.Sprintf("%s/copy%06d.go", t.dirs[d], rng.Intn(1_000_000))
			}
			if c.Previous != "" {
				if used[c.Path] {
					continue
				}
				used[c.Path] = true
			}
			out = append(out, c)
		}
	}
	return out
}

// apply records a move's changes in t, so later moves draw from the files
// that exist after it.
func (t *tree) apply(changes []domain.ChangedPath) {
	idx := make(map[string]int, len(t.dirs))
	for i, d := range t.dirs {
		idx[d] = i
	}
	remove := func(path string) {
		d, ok := idx[dirOf(path)]
		if !ok {
			return
		}
		files := t.files[d]
		for i, f := range files {
			if f == path {
				files[i] = files[len(files)-1]
				t.files[d] = files[:len(files)-1]
				return
			}
		}
	}
	add := func(path string) {
		if d, ok := idx[dirOf(path)]; ok {
			t.files[d] = append(t.files[d], path)
		}
	}
	for _, c := range changes {
		switch c.Change {
		case domain.PathAdded, domain.PathCopied:
			add(c.Path)
		case domain.PathDeleted:
			remove(c.Path)
		case domain.PathRenamed:
			remove(c.Previous)
			add(c.Path)
		}
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}
