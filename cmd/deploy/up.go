package deploy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// healthTimeout is how long up and upgrade wait for the stack to be
	// healthy after compose starts it.
	healthTimeout = 5 * time.Minute
	// pollInterval is how often a wait re-checks what it waits for.
	pollInterval = 5 * time.Second
)

func runUp(ctx context.Context, args []string, b Bundle, h *host) int {
	fset := newFlagSet("up", upUsage, h)
	dir := fset.String("dir", ".", "")
	executors := fset.Int("executors", 0, "")
	if code := parseFlags(fset, args, h); code >= 0 {
		return code
	}
	set := false
	fset.Visit(func(f *flag.Flag) { set = set || f.Name == "executors" })
	if set && *executors < 1 {
		fmt.Fprintf(h.stderr, "deploy up: --executors %d: the stack runs at least 1 executor\n", *executors)
		return 2
	}
	if err := up(ctx, h, b, *dir, *executors); err != nil {
		fmt.Fprintf(h.stderr, "deploy up: %v\n", err)
		return 1
	}
	return 0
}

// up starts dir's deployment and waits for it to be healthy. executors is
// the executor count to run and store, or 0 to run the stored count.
func up(ctx context.Context, h *host, b Bundle, dirFlag string, executors int) error {
	dir, err := deploymentDir(dirFlag)
	if err != nil {
		return err
	}
	h.heading("Preflight")
	checks, held := preflight(ctx, h, false)
	if failed := h.reportAll(checks); failed || !held {
		return errors.New("the host failed preflight; nothing was started")
	}
	st, err := readDeployment(dir)
	if err != nil {
		return err
	}

	h.heading("Image")
	if st.BuildFrom != nil {
		h.report(unverifiedSourceCheck("Source build", *st.BuildFrom))
	} else {
		if st.Version != b.Version {
			h.report(check{fail, "Binary version", fmt.Sprintf("the deployment is %s and this binary is %s", st.Version, b.Version)})
			return fmt.Errorf("this binary is not the deployment's version; %s. Nothing was started", versionFix(b.Version, st.Version))
		}
		// Again, though init verified it: the signature is cheap to check,
		// and this directory may have been copied from another host.
		if err := h.verifyImage(ctx, st.Version, *st.Image); err != nil {
			h.report(check{fail, "Signature", "the pinned digest does not verify; nothing was started"})
			return err
		}
		h.report(check{pass, "Pinned", *st.Image})
		h.report(check{pass, "Signature", "verified: " + signerIdentity(st.Version)})
		if err := pullIfMissing(ctx, h, *st.Image); err != nil {
			return err
		}
	}
	// compose runs the rendered files as they are on disk, so an image pin
	// edited since deploy rendered it is not the image verified above.
	switch edited, missing, err := changedFiles(dir, st); {
	case err != nil:
		h.report(check{warn, "Rendered files", err.Error()})
	case len(edited)+len(missing) > 0:
		h.report(check{warn, "Rendered files", describeChanges(edited, missing) + "; compose runs them as they are now, not as deploy rendered them. Changes belong in compose.override.yml"})
	}

	h.heading("Seccomp")
	if failed := h.reportAll([]check{seccompCheck(ctx, h, dir, st)}); failed {
		return errors.New("the executor's seccomp profile is refused on this host, so the executor would not start; nothing was started")
	}

	if executors != 0 && executors != st.Executors {
		st.Executors = executors
		if err := writeState(dir, st); err != nil {
			return err
		}
	}
	if err := composeUp(ctx, h, dir, st); err != nil {
		return err
	}
	if err := waitHealthy(ctx, h, st); err != nil {
		return err
	}

	publicURL, _ := publicURLOf(dir)
	h.report(publicURLCheck(ctx, h, dir))
	fmt.Fprintf(h.stdout, "\nThe deployment is up at %s with %d executor(s).\nNext: open %s and sign in with GitHub.\n", publicURL, st.Executors, publicURL)
	return nil
}

// readDeployment reads dir's state file, saying what to do when there is none.
func readDeployment(dir string) (deployState, error) {
	st, err := readState(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return st, fmt.Errorf("%s has no %s; run `triagefactory deploy init` there first", dir, stateFile)
	}
	return st, err
}

// pullIfMissing pulls a pinned release image that is not on the host yet, so
// the seccomp check that follows has an image to start.
func pullIfMissing(ctx context.Context, h *host, image string) error {
	if _, _, err := h.docker(ctx, "", "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		return nil
	}
	if err := h.stream(ctx, "", "pull", image); err != nil {
		return fmt.Errorf("docker pull %s: %w", image, err)
	}
	return nil
}

// composeUp starts the stack with its stored executor count. Every compose
// call deploy makes names the project from the state file, so a name chosen
// at init sticks, and passes the count, because compose resets a scale it is
// not given back to the file's single replica.
func composeUp(ctx context.Context, h *host, dir string, st deployState) error {
	h.heading("Start")
	args := []string{"compose", "-p", st.Project, "up", "-d", "--scale", "executor=" + strconv.Itoa(st.Executors)}
	if st.BuildFrom != nil {
		args = append(args, "--build")
	}
	if err := h.stream(ctx, dir, args...); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// waitHealthy polls the stack until every container is running and healthy
// (or finished, for a one-shot), GET /api/health answers 200 and GoTrue
// publishes a signing key, then reports those checks. After healthTimeout it
// reports them as they stand and fails.
func waitHealthy(ctx context.Context, h *host, st deployState) error {
	fmt.Fprintf(h.stdout, "\nWaiting up to %s for the stack to be healthy...\n", healthTimeout)
	deadline := h.now().Add(healthTimeout)
	for {
		cs, _ := healthChecks(ctx, h, st)
		healthy := !slices.ContainsFunc(cs, func(c check) bool { return c.status != pass })
		if healthy || !h.now().Before(deadline) {
			h.heading("Health")
			h.reportAll(cs)
			if healthy {
				return nil
			}
			return fmt.Errorf("the stack is not healthy after %s; `docker compose -p %s logs <service>` shows why, and `triagefactory deploy doctor` checks the rest", healthTimeout, st.Project)
		}
		if err := h.sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

// healthChecks are what the stack needs to serve: its containers, the
// control service's health endpoint, and GoTrue's signing keys. up is false
// when there are no containers to check further.
func healthChecks(ctx context.Context, h *host, st deployState) (cs []check, up bool) {
	c, containers, up := containerCheck(ctx, h, st.Project)
	if !up {
		return []check{c}, false
	}
	return []check{c, controlHealthCheck(ctx, h, containers), jwksCheck(ctx, h, containers)}, true
}

// publicURLOf is TF_PUBLIC_URL in dir's .env, empty when .env does not set it.
func publicURLOf(dir string) (string, error) {
	env, err := os.ReadFile(filepath.Join(dir, envFile))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", envFile, err)
	}
	return envValues(env).get("TF_PUBLIC_URL"), nil
}

// versionFix is what to do about a binary that is not the deployment's
// version. A newer release upgrades the deployment. An older one cannot run
// it at all: the deployment's control service has migrated the schema to its
// own version, migrations only go forward, and an older binary would boot
// against a schema newer than it knows.
func versionFix(binary, deployed string) string {
	switch c, ok := compareReleases(binary, deployed); {
	case ok && c > 0:
		return fmt.Sprintf("run `triagefactory deploy upgrade` to move the deployment to %s", binary)
	case ok && c < 0:
		return fmt.Sprintf("install the %s binary; a deployment never moves to an older version, because migrations only go forward", deployed)
	}
	return fmt.Sprintf("run the %s binary against it", deployed)
}

// compareReleases compares two release versions, reporting -1, 0 or 1, and
// ok false when either is not a release.
func compareReleases(a, b string) (c int, ok bool) {
	av, aok := parseRelease(a)
	bv, bok := parseRelease(b)
	if !aok || !bok {
		return 0, false
	}
	return slices.Compare(av[:], bv[:]), true
}

func parseRelease(v string) ([3]int, bool) {
	var out [3]int
	if !releaseVersion.MatchString(v) {
		return out, false
	}
	for i, part := range strings.Split(v[1:], ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

const upUsage = `triagefactory deploy up — start a deployment and wait for it to be healthy.

USAGE
  triagefactory deploy up [flags]

FLAGS
  --dir DIR        the deployment directory deploy init rendered (default: the
                   current directory)
  --executors N    run N executor replicas, and store N for every later up and
                   upgrade (default: the stored count, 1 after init)

WHAT IT DOES
  Checks the host, then that this binary is the deployment's version: a newer
  binary is told to run deploy upgrade, and an older one to install the
  deployment's version, because migrations only go forward. A source build
  (init --build-from) has no release version, so it is not checked, and is
  reported as an unverified build.

  Verifies the pinned image digest's cosign signature again, pulls the image if
  the host lacks it, and starts it under docker/seccomp-profile.json, which an
  older Docker Engine or libseccomp refuses. Then runs

    docker compose -p <project> up -d --scale executor=<N>

  in the deployment directory (with --build for a source build), and waits up
  to 5 minutes for every container to be running and healthy, GET /api/health
  to answer 200 and GoTrue to publish a signing key, each on the host port its
  container publishes (3000 and 9999 unless compose.override.yml moves them).

  A plain docker compose up -d resets the executor count to 1; deploy up always
  passes the stored count. Running deploy up on a stack that is already up
  changes nothing.
`
