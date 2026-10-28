package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestDeployBundleMatchesReleaseArchive is the drift guard for the deployment
// bundle, which is listed twice: goreleaser's compose-bundle archive, which
// a release attaches for a manual install, and embed.go's go:embed, which
// `triagefactory deploy` renders from. The two must name the same files, or
// a deployment made with the binary differs from one made from the tarball.
func TestDeployBundleMatchesReleaseArchive(t *testing.T) {
	var embedded []string
	err := fs.WalkDir(deployBundle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			embedded = append(embedded, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded bundle: %v", err)
	}

	var archived []string
	for _, pattern := range composeBundleFiles(t) {
		if dir, ok := strings.CutSuffix(pattern, "/**"); ok {
			err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					archived = append(archived, filepath.ToSlash(path))
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", dir, err)
			}
			continue
		}
		archived = append(archived, pattern)
	}

	slices.Sort(embedded)
	slices.Sort(archived)
	if !slices.Equal(embedded, archived) {
		t.Errorf("embed.go's deployBundle and goreleaser's compose-bundle archive differ; change both together\nembedded: %q\narchived: %q", embedded, archived)
	}
}

// composeBundleFiles reads the files list of the archive with id
// compose-bundle from .github/goreleaser.yaml: the `- ` items under its
// files: key, up to the next key at that indent or shallower.
func composeBundleFiles(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".github", "goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read goreleaser.yaml: %v", err)
	}
	var (
		inArchive, inFiles bool
		filesIndent        int
		files              []string
	)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case strings.HasPrefix(trimmed, "- id:"):
			inArchive = strings.TrimSpace(strings.TrimPrefix(trimmed, "- id:")) == "compose-bundle"
			inFiles = false
		case inArchive && trimmed == "files:":
			inFiles, filesIndent = true, indent
		case inFiles && indent > filesIndent && strings.HasPrefix(trimmed, "- "):
			files = append(files, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		case inFiles:
			inFiles = false
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan goreleaser.yaml: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("found no files list under the compose-bundle archive in .github/goreleaser.yaml")
	}
	return files
}
