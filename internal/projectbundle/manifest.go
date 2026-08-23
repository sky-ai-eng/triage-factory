package projectbundle

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// FormatVersion is the manifest format version. Bundles reporting a
	// different version are rejected with UnsupportedFormatError.
	FormatVersion = 2

	manifestPath                      = "manifest.yaml"
	knowledgePrefix                   = "knowledge-base/"
	defaultExportFilenameSuffix       = ".tfproject"
	maxManifestBytes            int64 = 4 << 20 // 4MB is ample for the metadata.
)

var (
	// ErrProjectNotFound is returned when the source project row is missing.
	ErrProjectNotFound = errors.New("project not found")
	// ErrManifestMissing is returned when the bundle has no manifest.
	ErrManifestMissing = errors.New("bundle is missing manifest.yaml")
)

// UnsupportedFormatError is returned when manifest.format_version is unknown.
type UnsupportedFormatError struct {
	Got int
}

func (e *UnsupportedFormatError) Error() string {
	return fmt.Sprintf("unsupported bundle format version %d", e.Got)
}

// DuplicateNameError is returned when import would create a project with a
// name that already exists.
type DuplicateNameError struct {
	Name string
}

func (e *DuplicateNameError) Error() string {
	return fmt.Sprintf("project name %q already exists", e.Name)
}

// MissingRepoError captures one pinned repo that failed import preflight.
type MissingRepoError struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

// MissingReposError is returned when one or more pinned repos are unreachable
// to the importer before any DB/filesystem mutation occurs.
type MissingReposError struct {
	Missing []MissingRepoError
}

func (e *MissingReposError) Error() string {
	if len(e.Missing) == 0 {
		return "one or more pinned repos are unreachable"
	}
	repos := make([]string, 0, len(e.Missing))
	for _, m := range e.Missing {
		repos = append(repos, m.Repo)
	}
	return "unreachable pinned repos: " + strings.Join(repos, ", ")
}

// Manifest is the top-level bundle metadata contract.
type Manifest struct {
	FormatVersion int             `yaml:"format_version"`
	ExportedAt    time.Time       `yaml:"exported_at"`
	Project       ManifestProject `yaml:"project"`
	// Warnings records non-fatal gaps in the export (for example a
	// knowledge-base file that existed but was unreadable by the
	// exporting server process) so a bundle that ships without
	// something says so instead of silently omitting it. Informational
	// only — import ignores it.
	Warnings []string `yaml:"warnings,omitempty"`
}

type ManifestProject struct {
	Name             string   `yaml:"name"`
	Description      string   `yaml:"description"`
	PinnedRepos      []string `yaml:"pinned_repos"`
	JiraProjectKey   string   `yaml:"jira_project_key,omitempty"`
	LinearProjectKey string   `yaml:"linear_project_key,omitempty"`
	// SummaryMD is no longer written by the exporter (the column was
	// dropped) but is still accepted at unmarshal time so older
	// bundles that include it import without YAML errors. The value is
	// dropped on import.
	SummaryMD string `yaml:"summary_md,omitempty"`
}

// Validate enforces v1 manifest invariants.
func (m *Manifest) Validate() error {
	if m.FormatVersion != FormatVersion {
		return &UnsupportedFormatError{Got: m.FormatVersion}
	}
	if strings.TrimSpace(m.Project.Name) == "" {
		return errors.New("manifest project.name is required")
	}
	return nil
}

func decodeManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		// Both wrapped: the parse error stays inspectable (it names the line
		// and column the uploader has to fix), and ErrBadBundle classifies it.
		return nil, fmt.Errorf("decode manifest: %w: %w", err, ErrBadBundle)
	}
	if err := m.Validate(); err != nil {
		// A manifest that fails its own invariants is a property of the
		// uploaded file, so it classifies as a client fault. Validate is
		// shared with the export path, where the same failure would be a
		// server-side bug — hence the marking happens here, on decode, not
		// inside Validate.
		return nil, fmt.Errorf("%w: %w", err, ErrBadBundle)
	}
	return &m, nil
}

func encodeManifest(m Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(&m)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return data, nil
}
