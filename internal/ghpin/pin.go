// Package ghpin is the single in-repo source of truth for the `gh` release
// Triage Factory ships to agents: one version, one checksum per platform asset.
//
// Two consumers, one pin. The multi-mode runtime image bakes the linux binary
// into /opt/tf/bin/gh at build time (docker/Dockerfile) and bind-mounts it into
// the jail; local mode fetches the same release into the state root at first use
// (internal/ghbin). A Dockerfile ARG can't read Go, so the version and the two
// linux checksums are spelled in both places and a drift test asserts they
// agree — the same shape as the toolhost and SDK-manifest drift tests.
//
// Bumping: pick a newer tag from https://github.com/cli/cli/releases, refresh
// every SHA256 below from that release's gh_<VER>_checksums.txt, and mirror the
// version + the two linux values into docker/Dockerfile. The drift test fails
// until both sides match, and the ghinjector wire tests re-drive the new binary
// so a transport change in gh's porcelain surfaces as a test failure rather than
// as silently dropped artifacts.
package ghpin

import "fmt"

// Version is the pinned gh release, without the leading "v".
const Version = "2.96.0"

// DownloadBase is the release-asset base URL. Assets live under
// <DownloadBase>/v<Version>/<asset>. Split out so a test can point a fetcher at
// a local server without restating the asset naming.
const DownloadBase = "https://github.com/cli/cli/releases/download"

// Format is an asset's container. gh ships linux as a gzipped tar and macOS as
// a zip; both unpack to <stem>/bin/gh.
type Format string

const (
	FormatTarGz Format = "tar.gz"
	FormatZip   Format = "zip"
)

// Asset is one platform's release artifact: the file name published under the
// release tag, its SHA256 from the release's checksums file, the container
// format, and the archive-relative path of the gh executable inside it.
type Asset struct {
	Name    string
	SHA256  string
	Format  Format
	BinPath string
}

// assets keys off "<GOOS>/<GOARCH>". Only the platforms Triage Factory actually
// runs an agent on are pinned: linux (the runtime image and Linux laptops) and
// darwin (the common local-mode development host). Everything else resolves to
// no asset, which callers degrade on rather than fail — a local run without a
// pinned gh keeps working through the existing exec verbs.
var assets = map[string]Asset{
	"linux/amd64": {
		Name:    "gh_" + Version + "_linux_amd64.tar.gz",
		SHA256:  "83d5c2ccad5498f58bf6368acb1ab32588cf43ab3a4b1c301bf36328b1c8bd60",
		Format:  FormatTarGz,
		BinPath: "gh_" + Version + "_linux_amd64/bin/gh",
	},
	"linux/arm64": {
		Name:    "gh_" + Version + "_linux_arm64.tar.gz",
		SHA256:  "06f86ec7103d41993b76cd78072f43595c34aaa56506d971d9860e67140bf909",
		Format:  FormatTarGz,
		BinPath: "gh_" + Version + "_linux_arm64/bin/gh",
	},
	"darwin/amd64": {
		Name:    "gh_" + Version + "_macOS_amd64.zip",
		SHA256:  "4bd449df9ad639391bc62b8032546f0fe9edcd8526e06682a4f88abd8c5d163c",
		Format:  FormatZip,
		BinPath: "gh_" + Version + "_macOS_amd64/bin/gh",
	},
	"darwin/arm64": {
		Name:    "gh_" + Version + "_macOS_arm64.zip",
		SHA256:  "f23a0c37d963aacc3bed703ccbd59b41c5ca22101fab7f00eb2b7cad23aba463",
		Format:  FormatZip,
		BinPath: "gh_" + Version + "_macOS_arm64/bin/gh",
	},
}

// AssetFor returns the pinned asset for a GOOS/GOARCH pair. ok is false for a
// platform with no pinned asset; callers treat that as "no gh channel here",
// never as an error to fail a run on.
func AssetFor(goos, goarch string) (Asset, bool) {
	a, ok := assets[goos+"/"+goarch]
	return a, ok
}

// URL is the asset's download URL under base (DownloadBase in production).
func (a Asset) URL(base string) string {
	return fmt.Sprintf("%s/v%s/%s", base, Version, a.Name)
}
