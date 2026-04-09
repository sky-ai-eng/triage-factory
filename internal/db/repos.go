package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// UpsertRepoProfile inserts or updates a repo profile.
// On conflict it updates all metadata fields while preserving the row identity.
// UpsertRepoProfile inserts or updates a repo profile.
// On conflict it updates profiling metadata but preserves user-configured fields (base_branch).
func UpsertRepoProfile(database *sql.DB, p domain.RepoProfile) error {
	_, err := database.Exec(`
		INSERT INTO repo_profiles (id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, profiled_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			description    = excluded.description,
			has_readme     = excluded.has_readme,
			has_claude_md  = excluded.has_claude_md,
			has_agents_md  = excluded.has_agents_md,
			profile_text   = excluded.profile_text,
			clone_url      = excluded.clone_url,
			default_branch = excluded.default_branch,
			profiled_at    = excluded.profiled_at,
			updated_at     = datetime('now')
	`,
		p.ID, p.Owner, p.Repo,
		nullIfEmpty(p.Description),
		p.HasReadme, p.HasClaudeMd, p.HasAgentsMd,
		nullIfEmpty(p.ProfileText),
		nullIfEmpty(p.CloneURL),
		nullIfEmpty(p.DefaultBranch),
		p.ProfiledAt,
	)
	return err
}

// GetAllRepoProfiles returns all repo profiles, including those without profile text.
func GetAllRepoProfiles(database *sql.DB) ([]domain.RepoProfile, error) {
	rows, err := database.Query(`
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch, profiled_at
		FROM repo_profiles
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []domain.RepoProfile
	for rows.Next() {
		var p domain.RepoProfile
		var description, profileText, cloneURL, defaultBranch, baseBranch sql.NullString
		var profiledAt sql.NullTime
		if err := rows.Scan(&p.ID, &p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch, &profiledAt); err != nil {
			return nil, err
		}
		p.Description = description.String
		p.ProfileText = profileText.String
		p.CloneURL = cloneURL.String
		p.DefaultBranch = defaultBranch.String
		p.BaseBranch = baseBranch.String
		if profiledAt.Valid {
			p.ProfiledAt = &profiledAt.Time
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// GetRepoProfilesWithContent returns all repo profiles that have a non-null profile_text.
func GetRepoProfilesWithContent(database *sql.DB) ([]domain.RepoProfile, error) {
	rows, err := database.Query(`
		SELECT id, owner, repo, description, has_readme, has_claude_md, has_agents_md, profile_text, clone_url, default_branch, base_branch
		FROM repo_profiles
		WHERE profile_text IS NOT NULL AND profile_text != ''
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []domain.RepoProfile
	for rows.Next() {
		var p domain.RepoProfile
		var description, profileText, cloneURL, defaultBranch, baseBranch sql.NullString
		if err := rows.Scan(&p.ID, &p.Owner, &p.Repo, &description, &p.HasReadme, &p.HasClaudeMd, &p.HasAgentsMd, &profileText, &cloneURL, &defaultBranch, &baseBranch); err != nil {
			return nil, err
		}
		p.Description = description.String
		p.ProfileText = profileText.String
		p.CloneURL = cloneURL.String
		p.DefaultBranch = defaultBranch.String
		p.BaseBranch = baseBranch.String
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// SetConfiguredRepos syncs the repo_profiles table with the given list of repo names.
// New repos get skeleton rows (no profile text). Removed repos are deleted.
func SetConfiguredRepos(database *sql.DB, repoNames []string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Build set of desired repos
	desired := make(map[string]bool, len(repoNames))
	for _, name := range repoNames {
		desired[name] = true
	}

	// Delete repos no longer selected
	existing, err := getRepoIDs(tx)
	if err != nil {
		return err
	}
	for _, id := range existing {
		if !desired[id] {
			if _, err := tx.Exec(`DELETE FROM repo_profiles WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}

	// Upsert skeleton rows for new repos (preserve existing profile data)
	for _, name := range repoNames {
		parts := splitOwnerRepo(name)
		if parts[0] == "" || parts[1] == "" {
			continue
		}
		_, err := tx.Exec(`
			INSERT INTO repo_profiles (id, owner, repo, updated_at)
			VALUES (?, ?, ?, datetime('now'))
			ON CONFLICT(id) DO UPDATE SET updated_at = datetime('now')
		`, name, parts[0], parts[1])
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetConfiguredRepoNames returns just the IDs (owner/repo) of all configured repos.
func GetConfiguredRepoNames(database *sql.DB) ([]string, error) {
	rows, err := database.Query(`SELECT id FROM repo_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// CountConfiguredRepos returns the number of configured repos.
func CountConfiguredRepos(database *sql.DB) (int, error) {
	var count int
	err := database.QueryRow(`SELECT COUNT(*) FROM repo_profiles`).Scan(&count)
	return count, err
}

func getRepoIDs(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM repo_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func splitOwnerRepo(s string) [2]string {
	for i, c := range s {
		if c == '/' {
			return [2]string{s[:i], s[i+1:]}
		}
	}
	return [2]string{s, ""}
}

// UpdateRepoBaseBranch sets the base branch for a repo. Empty string stores NULL.
func UpdateRepoBaseBranch(database *sql.DB, repoID, baseBranch string) error {
	_, err := database.Exec(`UPDATE repo_profiles SET base_branch = ?, updated_at = datetime('now') WHERE id = ?`,
		nullIfEmpty(baseBranch), repoID)
	return err
}

// UpdateTaskRepoMatch stores the repo match results for a task.
func UpdateTaskRepoMatch(database *sql.DB, taskID string, repos []string, blockedReason string) error {
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		return fmt.Errorf("marshal repos: %w", err)
	}
	_, err = database.Exec(`
		UPDATE tasks SET matched_repos = ?, blocked_reason = ? WHERE id = ?
	`, string(reposJSON), nullIfEmpty(blockedReason), taskID)
	return err
}
