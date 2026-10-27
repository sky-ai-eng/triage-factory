package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/envfile"
)

// initOptions are init's flags.
type initOptions struct {
	dir       string
	project   string
	buildFrom string
	inputs    inputFlags
}

func runInit(ctx context.Context, args []string, b Bundle, h *host) int {
	fset := newFlagSet("init", initUsage, h)
	var o initOptions
	fset.StringVar(&o.dir, "dir", ".", "")
	fset.StringVar(&o.project, "project", "", "")
	fset.StringVar(&o.buildFrom, "build-from", "", "")
	fset.StringVar(&o.inputs.publicURL, "public-url", "", "")
	fset.StringVar(&o.inputs.clientID, "github-client-id", "", "")
	fset.StringVar(&o.inputs.clientSecretFile, "github-client-secret-file", "", "")
	if code := parseFlags(fset, args, h); code >= 0 {
		return code
	}
	if err := initDeployment(ctx, h, b, o); err != nil {
		fmt.Fprintf(h.stderr, "deploy init: %v\n", err)
		return 1
	}
	return 0
}

var errPreflight = errors.New("the host failed preflight; nothing was written")

func initDeployment(ctx context.Context, h *host, b Bundle, o initOptions) error {
	dir, err := deploymentDir(o.dir)
	if err != nil {
		return err
	}
	existing, stateErr := readState(dir)
	initialized := stateErr == nil

	h.heading("Preflight")
	checks, held := preflight(ctx, h, !initialized)
	if failed := h.reportAll(checks); failed || !held {
		return errPreflight
	}
	if stateErr != nil && !errors.Is(stateErr, fs.ErrNotExist) {
		return stateErr
	}
	if initialized {
		return reinit(h, b, dir, existing, o)
	}
	return firstInit(ctx, h, b, dir, o)
}

// firstInit renders a new deployment into dir, which must be empty or not
// exist yet. Everything that can fail — the inputs, the image's signature,
// the rendering — is settled before the first write, and a write that fails
// takes the others back out, so a refused init leaves dir as it found it.
func firstInit(ctx context.Context, h *host, b Bundle, dir string, o initOptions) error {
	create, err := checkEmptyDir(dir)
	if err != nil {
		return err
	}
	project := o.project
	if project == "" {
		project = defaultProject
	}
	if !projectName.MatchString(project) {
		return fmt.Errorf("--project %q is not a compose project name (lowercase letters, digits, - and _, starting with a letter or digit)", project)
	}
	var src source
	switch {
	case o.buildFrom != "":
		if src.buildFrom, err = checkoutPath(o.buildFrom); err != nil {
			return err
		}
	case !releaseVersion.MatchString(b.Version):
		return fmt.Errorf("this binary's version is %q, not a release, so no published image matches it; pass --build-from <checkout> to build the image from source", b.Version)
	}

	compose, err := fs.ReadFile(b.Files, composeFile)
	if err != nil {
		return fmt.Errorf("read the embedded %s: %w", composeFile, err)
	}
	if err := checkCompose(compose, b.Version, src.buildFrom == ""); err != nil {
		return err
	}
	envBase, err := fs.ReadFile(b.Files, envExampleFile)
	if err != nil {
		return fmt.Errorf("read the embedded %s: %w", envExampleFile, err)
	}
	values, err := resolveInputs(h, envBase, o.inputs)
	if err != nil {
		return err
	}

	h.heading("Image")
	if src.buildFrom != "" {
		h.report(unverifiedSourceCheck("Source build", src.buildFrom))
	} else {
		ref, err := h.resolveImage(ctx, b.Version)
		if err != nil {
			// Not necessarily a bad signature: the tag may be unpublished,
			// the registry may refuse the pull, or the trust root may be
			// unreachable. The error says which.
			h.report(check{fail, "Pinned", "no verified digest; nothing was written"})
			return err
		}
		src.image = ref
		h.report(check{pass, "Pinned", ref})
		h.report(check{pass, "Signature", "verified: " + signerIdentity(b.Version)})
	}

	files, err := render(b, project, src)
	if err != nil {
		return err
	}
	vars, err := envFill(envBase, values)
	if err != nil {
		return err
	}
	st := deployState{Version: b.Version, Project: project, Executors: 1, Files: map[string]string{}}
	if src.image != "" {
		st.Image = &src.image
	} else {
		st.BuildFrom = &src.buildFrom
	}
	for path, data := range files {
		st.Files[path] = sha256Hex(data)
	}
	if err := writeDeployment(dir, create, files, envfile.Set(envBase, vars), st); err != nil {
		return err
	}

	publicURL := values["TF_PUBLIC_URL"]
	fmt.Fprintf(h.stdout, `
Rendered %s:
  %d bundle files (docker-compose.yml, docker/seccomp-profile.json, docker/observability/)
  %s (mode 0600) with every value boot needs; optional settings are documented in it
  %s, deploy's record of what it rendered

Put changes to the rendered files in compose.override.yml, which compose
merges on its own and deploy never writes.

The GitHub OAuth app's authorization callback URL must be
  %s/auth/v1/callback

Start the stack:
  cd %s && docker compose up -d
`, dir, len(files), envFile, stateFile, publicURL, dir)
	return nil
}

// reinit is init on a deployment it already rendered: it fills in what .env
// lacks and touches nothing else. Re-rendering is an upgrade, not this.
func reinit(h *host, b Bundle, dir string, st deployState, o initOptions) error {
	if st.Version != b.Version {
		// TODO(TFAC-1214): name `deploy upgrade` here as the way to move the
		// deployment to this binary's version once that subcommand exists.
		return fmt.Errorf("%s was rendered by %s and this binary is %s; init does not re-render a deployment. Run the %s binary against it", dir, st.Version, b.Version, st.Version)
	}
	if o.project != "" && o.project != st.Project {
		return fmt.Errorf("--project %s: this deployment's compose project is %s, and init never changes it", o.project, st.Project)
	}
	if o.buildFrom != "" {
		path, err := checkoutPath(o.buildFrom)
		if err != nil {
			return err
		}
		if st.BuildFrom == nil || *st.BuildFrom != path {
			return fmt.Errorf("--build-from %s: this deployment runs %s, and init never changes where its image comes from", path, st.imageRef())
		}
	}
	if st.BuildFrom != nil {
		h.heading("Image")
		h.report(unverifiedSourceCheck("Source build", *st.BuildFrom))
	}

	envPath := filepath.Join(dir, envFile)
	base, err := os.ReadFile(envPath)
	if errors.Is(err, fs.ErrNotExist) {
		if base, err = fs.ReadFile(b.Files, envExampleFile); err != nil {
			return fmt.Errorf("read the embedded %s: %w", envExampleFile, err)
		}
	} else if err != nil {
		return fmt.Errorf("read %s: %w", envPath, err)
	}
	values, err := resolveInputs(h, base, o.inputs)
	if err != nil {
		return err
	}
	vars, err := envFill(base, values)
	if err != nil {
		return err
	}
	if len(vars) == 0 {
		fmt.Fprintf(h.stdout, "\n%s already holds every value boot needs; nothing changed.\n", envPath)
		return nil
	}
	if err := envfile.WriteAtomic(envPath, envfile.Set(base, vars), 0o600); err != nil {
		return err
	}

	env := envValues(base)
	var added, replaced, lossy []string
	newKeypair := false
	for _, v := range vars {
		if env.get(v.Key) != "" {
			replaced = append(replaced, v.Key)
		} else {
			added = append(added, v.Key)
		}
		if lossyIfReplaced[v.Key] {
			lossy = append(lossy, v.Key)
		}
		newKeypair = newKeypair || v.Key == "GOTRUE_JWT_KEYS"
	}
	if len(added) > 0 {
		fmt.Fprintf(h.stdout, "\nAdded to %s: %s\n", envPath, strings.Join(added, ", "))
	}
	if len(replaced) > 0 {
		fmt.Fprintf(h.stdout, "\nReplaced in %s, because the GoTrue key that signed them is new: %s\n", envPath, strings.Join(replaced, ", "))
	}
	if newKeypair && env.get("TF_GOTRUE_SERVICE_ROLE_TOKEN_FILE") != "" {
		fmt.Fprintf(h.stdout, `
warning: TF_GOTRUE_SERVICE_ROLE_TOKEN_FILE names a token signed by the old
GoTrue key, which no longer verifies. Mint one against the new key with
  triagefactory jwk-init --write-env .env
which reuses the new key and writes only TF_GOTRUE_SERVICE_ROLE_TOKEN, and
move that value into the file.
`)
	}
	if len(lossy) > 0 {
		fmt.Fprintf(h.stdout, `
warning: these values were missing and are new: %s
If this deployment has already run, what it made under the old values no
longer opens with the new ones: Postgres keeps the password it was
initialized with, stored credentials and sessions stay encrypted under the
old keys, and sessions signed by the old GoTrue key end. Put the old values
back if you have them.
`, strings.Join(lossy, ", "))
	}
	return nil
}

func unverifiedSourceCheck(name, path string) check {
	return check{warn, name, fmt.Sprintf("%s built from the source checkout at %s; an unverified source build, not a signed release", devImage, path)}
}

// checkEmptyDir accepts a dir that does not exist yet, whose parent does,
// reporting that init must create it, or one that exists and is empty.
// Anything else is refused: init does not adopt a deployment it did not
// render.
func checkEmptyDir(dir string) (create bool, err error) {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		parent := filepath.Dir(dir)
		if info, err := os.Stat(parent); err != nil || !info.IsDir() {
			return false, fmt.Errorf("%s does not exist and neither does its parent %s; init creates the deployment directory but not its parents", dir, parent)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read %s: %w", dir, err)
	case len(entries) > 0:
		return false, fmt.Errorf("%s is not empty and holds no %s; init renders only into an empty directory or one it initialized, and does not adopt a deployment it did not render", dir, stateFile)
	}
	return false, nil
}

// checkoutPath resolves --build-from to the absolute path of a Triage
// Factory source checkout: compose builds from it by that path, from
// wherever compose is run.
func checkoutPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--build-from %q: %w", path, err)
	}
	if _, err := os.Stat(filepath.Join(abs, "docker", "Dockerfile")); err != nil {
		return "", fmt.Errorf("--build-from %s is not a Triage Factory source checkout (it has no docker/Dockerfile)", abs)
	}
	return abs, nil
}

// writeDeployment writes a first init's files. dir was empty or absent, so
// when a write fails everything this call wrote there is its to remove.
func writeDeployment(dir string, create bool, files map[string][]byte, env []byte, st deployState) (err error) {
	if create {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	defer func() {
		if err == nil {
			return
		}
		if create {
			_ = os.RemoveAll(dir)
			return
		}
		// Only the names this call writes: dir was checked empty before the
		// image was resolved, and anything that arrived since is not ours.
		for _, name := range topLevelNames(files) {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}()
	for _, path := range slices.Sorted(maps.Keys(files)) {
		rel := filepath.FromSlash(path)
		if err := mkdirs(dir, filepath.Dir(rel)); err != nil {
			return err
		}
		if err := envfile.WriteAtomic(filepath.Join(dir, rel), files[path], 0o644); err != nil {
			return err
		}
	}
	if err := envfile.WriteAtomic(filepath.Join(dir, envFile), env, 0o600); err != nil {
		return err
	}
	// Last, so a directory holding a state file holds everything with it.
	return writeState(dir, st)
}

// topLevelNames are the entries a first init creates directly in the
// deployment directory: each rendered file's first path segment, .env and the
// state file.
func topLevelNames(files map[string][]byte) []string {
	names := map[string]bool{envFile: true, stateFile: true}
	for path := range files {
		first, _, _ := strings.Cut(path, "/")
		names[first] = true
	}
	return slices.Sorted(maps.Keys(names))
}

// mkdirs creates rel's directories under dir at mode 0755, whatever the
// umask: compose bind-mounts them into containers whose processes run as
// other users, which must be able to read them.
func mkdirs(dir, rel string) error {
	if rel == "." {
		return nil
	}
	p := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", p, err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}

const initUsage = `triagefactory deploy init — render a deployment directory.

USAGE
  triagefactory deploy init [flags]

FLAGS
  --dir DIR                         the deployment directory (default: the
                                    current directory). It must be empty or
                                    not exist yet, or be one init rendered.
  --public-url URL                  TF_PUBLIC_URL: https://host[:port], or
                                    http://localhost for a test stack. No path,
                                    no trailing slash.
  --github-client-id ID             GH_CLIENT_ID of the GitHub OAuth app users
                                    sign in through. Its callback URL is
                                    <public URL>/auth/v1/callback.
  --github-client-secret-file FILE  a file holding GH_CLIENT_SECRET. There is no
                                    flag that takes the secret itself: argv is
                                    visible to every user in ps.
  --project NAME                    the compose project name, which prefixes
                                    every volume (default: triage-factory)
  --build-from CHECKOUT             build the image from a source checkout
                                    instead of pulling the signed release image.
                                    Required for a binary that is not a release.

On a terminal, init asks for whichever of the three values no flag gave;
without one, it fails and names the missing flags.

WHAT IT DOES
  Checks the host (Linux, Docker Engine, Compose 2+, cgroup v2, ports 3000,
  9999 and 3030), resolves ghcr.io/sky-ai-eng/triage-factory:<this binary's
  version> to a digest, verifies that digest's cosign signature in-process against
  Sigstore's public trust root — issuer token.actions.githubusercontent.com,
  identity exactly this repository's docker-publish.yml at this version's tag —
  and only then renders:

    docker-compose.yml           the image pinned as <tag>@sha256:<digest>
    docker/seccomp-profile.json  the executor's seccomp profile
    docker/observability/        Tempo, Prometheus and Grafana configuration
    .env                         mode 0600: every required value, with each
                                 secret generated, and every optional setting
                                 documented as .env.example has it
    .triagefactory-deploy.json   deploy's state: version, image, project and
                                 the sha256 of each rendered file

  Operator changes to the rendered files belong in compose.override.yml,
  which compose merges on its own and deploy never writes.

RE-RUNNING
  On a directory it rendered, init adds only what .env lacks and changes
  nothing else. It never regenerates a value .env has: a new
  TF_SECRET_ENCRYPTION_KEY makes every stored credential unreadable, and a new
  POSTGRES_PASSWORD locks TF out of its database.
`
