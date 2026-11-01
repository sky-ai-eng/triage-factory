package tracker

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestOverlaps(t *testing.T) {
	mod := func(p string) domain.ChangedPath { return domain.ChangedPath{Path: p, Change: domain.PathModified} }
	add := func(p string) domain.ChangedPath { return domain.ChangedPath{Path: p, Change: domain.PathAdded} }
	del := func(p string) domain.ChangedPath { return domain.ChangedPath{Path: p, Change: domain.PathDeleted} }
	ren := func(from, to string) domain.ChangedPath {
		return domain.ChangedPath{Path: to, Change: domain.PathRenamed, Previous: from}
	}
	cases := []struct {
		name     string
		pr, move []domain.ChangedPath
		want     bool
	}{
		{"an exact path", []domain.ChangedPath{mod("src/a.go")}, []domain.ChangedPath{mod("src/a.go")}, true},
		{"a renamed file's previous path on the pull request's side",
			[]domain.ChangedPath{ren("src/old.go", "src/new.go")}, []domain.ChangedPath{mod("src/old.go")}, true},
		{"a renamed file's previous path on the move's side",
			[]domain.ChangedPath{mod("src/old.go")}, []domain.ChangedPath{ren("src/old.go", "src/new.go")}, true},
		{"the pull request's path a parent directory of the move's",
			[]domain.ChangedPath{add("docs/api")}, []domain.ChangedPath{add("docs/api/index.md")}, true},
		{"the move's path a parent directory of the pull request's",
			[]domain.ChangedPath{mod("docs/api/index.md")}, []domain.ChangedPath{add("docs/api")}, true},
		{"a file the pull request added under a directory the move renamed away",
			[]domain.ChangedPath{add("pkg/widget/extra.go")},
			[]domain.ChangedPath{ren("pkg/widget/widget.go", "pkg/gadget/widget.go")}, true},
		{"a file the pull request added under a directory the move only partly emptied",
			[]domain.ChangedPath{add("pkg/widget/extra.go")},
			[]domain.ChangedPath{del("pkg/widget/old.go")}, true},
		{"a file the pull request modified under a vacated directory is no addition",
			[]domain.ChangedPath{mod("pkg/widget/kept.go")},
			[]domain.ChangedPath{del("pkg/widget/old.go")}, false},
		{"a deletion at the root vacates nothing",
			[]domain.ChangedPath{add("NEWS.md")}, []domain.ChangedPath{del("CHANGELOG.md")}, false},
		{"a rename within its directory vacates nothing",
			[]domain.ChangedPath{add("pkg/widget/extra.go")},
			[]domain.ChangedPath{ren("pkg/widget/a.go", "pkg/widget/b.go")}, false},
		{"a sibling with a shared name prefix", []domain.ChangedPath{mod("src/a.go")}, []domain.ChangedPath{mod("src/a.gox")}, false},
		{"no overlap", []domain.ChangedPath{mod("src/a.go"), add("docs/b.md")}, []domain.ChangedPath{mod("lib/c.go")}, false},
	}
	for _, c := range cases {
		if got := Overlaps(c.pr, c.move); got != c.want {
			t.Errorf("%s: overlaps = %v, want %v", c.name, got, c.want)
		}
	}
}
