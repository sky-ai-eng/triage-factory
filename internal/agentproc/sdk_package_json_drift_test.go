package agentproc

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dockerfilePrintfRE captures the sdk-builder stage's package.json printf:
// the single-quoted shell format string, then the two shell variables it
// interpolates, in order. Anchoring on the manifest's name field keeps the
// match off any other printf in the file.
var dockerfilePrintfRE = regexp.MustCompile(`printf '(\{\\n  "name": "triagefactory-sdk-runtime".*?)' "\$([A-Z_]+)" "\$([A-Z_]+)"`)

// TestSDKPackageJSON_MatchesDockerfileTemplate is the drift guard for the
// SDK manifest, which is rendered twice: by sdkPackageJSON for a local
// install, and by docker/Dockerfile's printf when the image bakes the SDK
// at build time (that stage has no Go to call the Go path). The two must
// agree byte-for-byte, because `npm ci` refuses to install whenever the
// manifest disagrees with the embedded lockfile about a dependency or an
// override — so a divergence here breaks either the image build or every
// fresh local install, depending on which side moved.
func TestSDKPackageJSON_MatchesDockerfileTemplate(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	m := dockerfilePrintfRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("no package.json printf found in docker/Dockerfile: the sdk-builder stage changed shape — update this test alongside it")
	}

	// The printf format is shell-escaped; \n is the only escape in play.
	// Substituting the Go constants for the shell variables reproduces
	// exactly what the build stage writes to package.json.
	got := strings.ReplaceAll(m[1], `\n`, "\n")
	for _, sub := range []struct {
		shellVar string
		value    string
	}{
		{m[2], sdkVersion},
		{m[3], honoNodeServerOverride},
	} {
		got = strings.Replace(got, "%s", sub.value, 1)
	}

	if want := string(sdkPackageJSON()); got != want {
		t.Errorf("Dockerfile-rendered package.json differs from sdkPackageJSON()\n--- Dockerfile ---\n%s\n--- sdkPackageJSON ---\n%s", got, want)
	}
}

// TestDockerfilePrintf_InterpolatesPinConstantsInOrder pins the argument
// order the Dockerfile feeds its printf. The rendering test above would
// still pass if the two %s arguments were swapped *and* the Go template
// were swapped to match, leaving the image installing an SDK version of
// "^2.0.5"; asserting the shell variable names catches that directly.
func TestDockerfilePrintf_InterpolatesPinConstantsInOrder(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	m := dockerfilePrintfRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("no package.json printf found in docker/Dockerfile")
	}

	if m[2] != "SDK_VERSION" {
		t.Errorf("first printf argument = $%s, want $SDK_VERSION", m[2])
	}
	if m[3] != "HONO_OVERRIDE" {
		t.Errorf("second printf argument = $%s, want $HONO_OVERRIDE", m[3])
	}
}

// TestDockerfile_ExtractsPinConstantsFromInstallGo asserts the greps the
// sdk-builder stage runs still match the constant declarations in this
// file. The greps are anchored and fail the build when they come up empty,
// but that failure only surfaces in an image build — this catches a rename
// in `go test`, where the person doing the renaming is looking.
func TestDockerfile_ExtractsPinConstantsFromInstallGo(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	installGo, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatalf("read install.go: %v", err)
	}

	// Recover each grep pattern from the Dockerfile and run it against
	// install.go the way the build stage does, so a constant rename that
	// misses one of the two call sites fails here.
	grepRE := regexp.MustCompile(`grep -E '(\^const [a-zA-Z]+ = "\[\^"\]\+")'`)
	patterns := grepRE.FindAllStringSubmatch(string(dockerfile), -1)
	if len(patterns) != 2 {
		t.Fatalf("found %d constant greps in docker/Dockerfile, want 2 (sdkVersion, honoNodeServerOverride)", len(patterns))
	}

	for _, p := range patterns {
		// Translate the shell-quoted ERE into Go's regexp dialect: the
		// pattern is already valid ERE, only multiline anchoring differs.
		re, err := regexp.Compile(`(?m)` + p[1])
		if err != nil {
			t.Fatalf("compile Dockerfile grep pattern %q: %v", p[1], err)
		}
		if !re.Match(installGo) {
			t.Errorf("Dockerfile grep %q matches nothing in install.go: the constant was renamed or reshaped, which fails the image build", p[1])
		}
	}
}
