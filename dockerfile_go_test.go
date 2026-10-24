package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDockerfileGoMatchesGoMod is the drift guard for the Go version, which is
// spelled twice: go.mod's `go` line, which CI and every local build read, and
// the golang image tag docker/Dockerfile builds the shipped binary in, which a
// Dockerfile can only spell as a literal. The official Go image sets
// GOTOOLCHAIN=local, so an image older than go.mod fails the image build
// outright, and one newer than it ships a binary built with a Go no CI run used.
func TestDockerfileGoMatchesGoMod(t *testing.T) {
	gomod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	goLine := regexp.MustCompile(`(?m)^go (\S+)\s*$`).FindSubmatch(gomod)
	if goLine == nil {
		t.Fatal("go.mod has no `go` line")
	}
	want := string(goLine[1])

	dockerfile, err := os.ReadFile(filepath.Join("docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read docker/Dockerfile: %v", err)
	}
	// The version is the tag up to its variant suffix: 1.26.10 in
	// golang:1.26.10-alpine. A tag that doesn't open with a version
	// (golang:alpine) is not matched, so it fails the count below.
	froms := regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?golang:([0-9][^-\s@]*)`).FindAllSubmatch(dockerfile, -1)
	if len(froms) != 1 {
		t.Fatalf("docker/Dockerfile has %d FROM lines naming a versioned golang image, want exactly 1", len(froms))
	}
	if got := string(froms[0][1]); got != want {
		t.Errorf("docker/Dockerfile builds with golang %s but go.mod's go line is %s; move both in one change", got, want)
	}
}
