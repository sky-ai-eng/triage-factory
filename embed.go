package main

import (
	"embed"
	"io/fs"
)

//go:embed frontend/dist/*
var frontendFS embed.FS

func frontendDist() (fs.FS, error) {
	return fs.Sub(frontendFS, "frontend/dist")
}

// deployBundle is the multi-mode compose bundle `triagefactory deploy`
// renders: the files goreleaser's compose-bundle archive ships, at their repo
// paths, so the release tarball and the binary carry the same bytes.
// deploy_bundle_test.go holds the two lists together.
//
//go:embed docker-compose.yml .env.example docker/seccomp-profile.json docker/observability
var deployBundle embed.FS
