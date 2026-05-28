package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// githubAPIBase derives the REST API base from a user-facing GitHub URL,
// mirroring internal/github.NewClient + the server's same-named helper.
// Empty or the public host maps to api.github.com; a GHES base gets the
// /api/v3 suffix.
func githubAPIBase(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "" || base == "https://github.com" {
		return "https://api.github.com"
	}
	return base + "/api/v3"
}

// DiscoverAppInstallations reads the org's App PEM via SecretStore.GetSystem
// (claims-free, for the system/background backfill caller), mints an
// app-level JWT, and lists the App's installations through
// GET {apiBase}/app/installations. The returned rows carry orgID and are
// ready to hand to GitHubAppsStore.UpsertInstallation — the per-org App
// owns every installation it reports (v1 is per-org Apps only).
//
// Shared by both store backends so the JWT-mint + HTTP-list + secret-read
// logic lives in one place; each backend supplies the DB read of
// (appID, pemRef, baseURL) and the upsert.
func DiscoverAppInstallations(ctx context.Context, secrets SecretStore, orgID, appID, pemRef, baseURL string) ([]domain.OrgGitHubAppInstallation, error) {
	pem, err := secrets.GetSystem(ctx, orgID, pemRef)
	if err != nil {
		return nil, fmt.Errorf("read app pem: %w", err)
	}
	if pem == "" {
		return nil, fmt.Errorf("github app pem secret %q not found for org %s", pemRef, orgID)
	}

	key, err := githubapp.ParsePrivateKey([]byte(pem))
	if err != nil {
		return nil, err
	}
	appID64, err := strconv.ParseInt(appID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse app id %q: %w", appID, err)
	}

	minter, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID64,
		APIBase:    githubAPIBase(baseURL),
	})
	if err != nil {
		return nil, err
	}

	raw, err := minter.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]domain.OrgGitHubAppInstallation, 0, len(raw))
	for _, in := range raw {
		out = append(out, domain.OrgGitHubAppInstallation{
			InstallationID: strconv.FormatInt(in.ID, 10),
			OrgID:          orgID,
			AccountType:    in.AccountType,
			AccountLogin:   in.AccountLogin,
			InstalledAt:    in.CreatedAt,
		})
	}
	return out, nil
}
