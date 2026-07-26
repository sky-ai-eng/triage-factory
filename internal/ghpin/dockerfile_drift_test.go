package ghpin

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// dockerfileARG extracts one `ARG NAME=value` line from the Dockerfile.
func dockerfileARG(t *testing.T, body []byte, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^ARG ` + name + `=(\S+)\s*$`)
	m := re.FindSubmatch(body)
	if m == nil {
		t.Fatalf("docker/Dockerfile has no `ARG %s=` line — the gh pin moved or was renamed; keep it in lockstep with this package", name)
	}
	return string(m[1])
}

// TestDockerfile_MatchesPin is the drift guard for the gh pin, which is spelled
// twice: here (the Go source of truth every runtime consumer reads) and in
// docker/Dockerfile's ARGs (which a Dockerfile can only express as literals —
// there is no Go at image-build time). A divergence would ship two different gh
// binaries under one "pinned" claim: the image would bake one version while a
// local install fetched another, and the ghinjector wire tests — which CI stages
// from the Dockerfile ARGs — would then be pinning a transport contract for a
// binary no local run ever executes.
func TestDockerfile_MatchesPin(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	if got := dockerfileARG(t, body, "GH_VERSION"); got != Version {
		t.Errorf("Dockerfile ARG GH_VERSION = %q, want ghpin.Version %q", got, Version)
	}

	for _, tc := range []struct {
		arg   string
		goos  string
		march string
	}{
		{"GH_SHA256_AMD64", "linux", "amd64"},
		{"GH_SHA256_ARM64", "linux", "arm64"},
	} {
		asset, ok := AssetFor(tc.goos, tc.march)
		if !ok {
			t.Fatalf("no pinned asset for %s/%s — the image bakes it, so it must stay pinned", tc.goos, tc.march)
		}
		if got := dockerfileARG(t, body, tc.arg); got != asset.SHA256 {
			t.Errorf("Dockerfile ARG %s = %q, want the pinned %s/%s checksum %q", tc.arg, got, tc.goos, tc.march, asset.SHA256)
		}
	}
}

// TestAssetFor_UnpinnedPlatform pins the degradation contract: an unsupported
// platform is a miss, not a panic or a zero-valued asset a fetcher would try to
// download.
func TestAssetFor_UnpinnedPlatform(t *testing.T) {
	if a, ok := AssetFor("plan9", "mips"); ok {
		t.Errorf("AssetFor(plan9/mips) = (%+v, true), want a miss", a)
	}
}

// TestAssetURL pins the release-asset URL shape the Dockerfile also builds by
// hand, so a change to one is visible against the other.
func TestAssetURL(t *testing.T) {
	a, ok := AssetFor("linux", "amd64")
	if !ok {
		t.Fatal("no linux/amd64 asset")
	}
	want := DownloadBase + "/v" + Version + "/gh_" + Version + "_linux_amd64.tar.gz"
	if got := a.URL(DownloadBase); got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}
