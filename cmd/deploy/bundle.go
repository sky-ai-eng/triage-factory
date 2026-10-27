package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/envfile"
)

// The deployment directory's layout. deploy renders the bundle's files and
// owns the state file; .env and compose.override.yml are the operator's,
// and deploy writes .env only to add a value it lacks.
const (
	composeFile    = "docker-compose.yml"
	seccompFile    = "docker/seccomp-profile.json"
	envExampleFile = ".env.example"
	envFile        = ".env"
	stateFile      = ".triagefactory-deploy.json"

	// defaultProject is the compose project name docker-compose.yml sets.
	defaultProject = "triage-factory"

	// devImage is what a --build-from deployment builds and runs, the tag
	// compose.dev.yml uses: a source build never sits in the image cache
	// under a release's name.
	devImage = "triagefactory:dev"
)

var (
	// releaseVersion is a release tag. Anything else — "dev", a snapshot, a
	// describe-style version — has no published image.
	releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

	// projectName is what compose accepts as a project name.
	projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

	composeNameLine  = regexp.MustCompile(`(?m)^name: \S+$`)
	composeImageLine = regexp.MustCompile(`(?m)^( +)image: ` + regexp.QuoteMeta(imageRepository) + `:(\S+).*$`)
)

// source is where the control and executor images come from: a release
// image pinned by a verified digest, or a build of a source checkout.
type source struct {
	image     string
	buildFrom string
}

// render returns the files deploy writes into a deployment directory, keyed
// by slash path: every bundle file but .env.example, which init turns into
// .env instead, with docker-compose.yml's project name and images set.
func render(b Bundle, project string, src source) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(b.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == envExampleFile {
			return err
		}
		data, err := fs.ReadFile(b.Files, path)
		if err != nil {
			return err
		}
		files[path] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the embedded bundle: %w", err)
	}
	compose, err := renderCompose(files[composeFile], b.Version, project, src)
	if err != nil {
		return nil, err
	}
	files[composeFile] = compose
	if _, ok := files[seccompFile]; !ok {
		return nil, fmt.Errorf("the embedded bundle has no %s", seccompFile)
	}
	return files, nil
}

// checkCompose checks the embedded docker-compose.yml has the lines
// rendering rewrites — one top-level name: and the control and executor
// image pins — and, when the deployment pulls the release image, that the
// pin is this binary's own version: the embedded file and the binary come
// from one release commit, or the binary is not that release.
func checkCompose(raw []byte, version string, pulled bool) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("this binary carries no deployment bundle (its embedded docker-compose.yml is empty, as in the container image's build); run deploy with a release binary")
	}
	if n := len(composeNameLine.FindAllIndex(raw, -1)); n != 1 {
		return fmt.Errorf("the embedded %s has %d top-level name: lines, want 1", composeFile, n)
	}
	pins := composeImageLine.FindAllSubmatch(raw, -1)
	if len(pins) != 2 {
		return fmt.Errorf("the embedded %s pins %s on %d lines, want 2 (control and executor)", composeFile, imageRepository, len(pins))
	}
	if pulled {
		for _, p := range pins {
			if tag := string(p[2]); tag != version {
				return fmt.Errorf("the embedded %s pins %s:%s, but this binary is %s; it was not built from that release's commit", composeFile, imageRepository, tag, version)
			}
		}
	}
	return nil
}

// renderCompose sets docker-compose.yml's project name, and replaces the two
// image pins (control and executor) with the pinned release image or with a
// build of the checkout, the shape compose.dev.yml layers on in a checkout.
func renderCompose(raw []byte, version, project string, src source) ([]byte, error) {
	if err := checkCompose(raw, version, src.image != ""); err != nil {
		return nil, err
	}
	out := composeNameLine.ReplaceAll(raw, []byte("name: "+project))
	out = composeImageLine.ReplaceAllFunc(out, func(line []byte) []byte {
		indent := string(line[:len(line)-len(bytes.TrimLeft(line, " "))])
		if src.image != "" {
			return []byte(indent + "image: " + src.image)
		}
		return []byte(buildBlock(indent, src.buildFrom))
	})
	return out, nil
}

// buildBlock is a service's build of the checkout at path. The path is a
// double-quoted scalar with any $ doubled, so neither YAML nor compose's
// interpolation reads anything in it.
func buildBlock(indent, path string) string {
	quoted, _ := json.Marshal(strings.ReplaceAll(path, "$", "$$"))
	lines := []string{
		"build:",
		"  context: " + string(quoted),
		"  dockerfile: docker/Dockerfile",
		"  args:",
		`    TF_LICENSE_PUBLIC_KEY: "${TF_LICENSE_PUBLIC_KEY:-}"`,
		"image: " + devImage,
		"pull_policy: build",
	}
	return indent + strings.Join(lines, "\n"+indent)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// deployState is .triagefactory-deploy.json: what init rendered, from what,
// so later commands can tell whether a rendered file was edited, and which
// binary a deployment belongs to.
type deployState struct {
	// Version is the binary version that rendered the deployment.
	Version string `json:"version"`
	// Image is the pinned release image (repository:tag@sha256:…), and
	// BuildFrom the absolute checkout path a source build uses. Exactly one
	// is set.
	Image     *string `json:"image"`
	BuildFrom *string `json:"build_from"`
	// Project is the compose project name, which compose prefixes every
	// volume with; every compose call deploy makes passes it.
	Project string `json:"project"`
	// Executors is how many executor replicas the stack runs.
	Executors int `json:"executors"`
	// Files is the sha256 of each rendered file, keyed by slash path.
	Files map[string]string `json:"files"`
}

// imageRef is the image the control and executor services run.
func (s deployState) imageRef() string {
	if s.Image != nil {
		return *s.Image
	}
	return devImage
}

func (s deployState) validate() error {
	var problems []string
	if s.Version == "" {
		problems = append(problems, "version is empty")
	}
	if (s.Image == nil) == (s.BuildFrom == nil) {
		problems = append(problems, "exactly one of image and build_from must be set")
	}
	if !projectName.MatchString(s.Project) {
		problems = append(problems, fmt.Sprintf("project %q is not a compose project name", s.Project))
	}
	if s.Executors < 1 {
		problems = append(problems, fmt.Sprintf("executors is %d, want at least 1", s.Executors))
	}
	if len(s.Files) == 0 {
		problems = append(problems, "files is empty")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// readState reads dir's state file. A missing file is an error wrapping
// fs.ErrNotExist.
func readState(dir string) (deployState, error) {
	var s deployState
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", stateFile, err)
	}
	if err := s.validate(); err != nil {
		return s, fmt.Errorf("%s: %w", stateFile, err)
	}
	return s, nil
}

func writeState(dir string, s deployState) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return envfile.WriteAtomic(filepath.Join(dir, stateFile), append(raw, '\n'), 0o644)
}

// changedFiles compares dir's rendered files with the hashes init recorded,
// returning the ones whose bytes differ and the ones that are gone.
func changedFiles(dir string, s deployState) (edited, missing []string, err error) {
	for _, path := range slices.Sorted(maps.Keys(s.Files)) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, path)
		case err != nil:
			return nil, nil, err
		case sha256Hex(data) != s.Files[path]:
			edited = append(edited, path)
		}
	}
	return edited, missing, nil
}
