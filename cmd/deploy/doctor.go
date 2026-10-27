package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func runDoctor(ctx context.Context, args []string, b Bundle, h *host) int {
	fset := newFlagSet("doctor", doctorUsage, h)
	dirFlag := fset.String("dir", ".", "")
	if code := parseFlags(fset, args, h); code >= 0 {
		return code
	}
	dir, err := deploymentDir(*dirFlag)
	if err != nil {
		fmt.Fprintf(h.stderr, "deploy doctor: %v\n", err)
		return 1
	}
	if failed := doctor(ctx, h, b, dir); failed {
		return 1
	}
	return 0
}

// doctor reports every check on dir's deployment and whether any failed.
// Each group needs the one before it — the bundle checks a state file, the
// seccomp check the image it names — so a group that cannot run says so on
// one line instead of failing check by check.
func doctor(ctx context.Context, h *host, b Bundle, dir string) (failed bool) {
	st, stateErr := readState(dir)

	h.heading("Preflight")
	checks, held := preflight(ctx, h, errors.Is(stateErr, fs.ErrNotExist))
	failed = h.reportAll(checks)
	if !held {
		return true
	}

	h.heading("Bundle")
	switch {
	case errors.Is(stateErr, fs.ErrNotExist):
		h.report(check{fail, "Deployment", fmt.Sprintf("%s has no %s; run deploy init there", dir, stateFile)})
		return true
	case stateErr != nil:
		h.report(check{fail, "Deployment", stateErr.Error()})
		return true
	}
	failed = h.reportAll(bundleChecks(h, b, dir, st)) || failed

	h.heading("Seccomp")
	failed = h.reportAll([]check{seccompCheck(ctx, h, dir, st)}) || failed

	h.heading("Running stack")
	failed = h.reportAll(stackChecks(ctx, h, dir, st)) || failed
	return failed
}

func bundleChecks(h *host, b Bundle, dir string, st deployState) []check {
	cs := []check{{pass, "Deployment", fmt.Sprintf("%s, compose project %s", st.Version, st.Project)}}
	if st.BuildFrom != nil {
		cs = append(cs, unverifiedSourceCheck("Image", *st.BuildFrom))
	} else {
		cs = append(cs, check{pass, "Image", *st.Image + " (signature verified at init)"})
	}

	edited, missing, err := changedFiles(dir, st)
	switch {
	case err != nil:
		cs = append(cs, check{fail, "Rendered files", err.Error()})
	case len(edited)+len(missing) == 0:
		cs = append(cs, check{pass, "Rendered files", fmt.Sprintf("all %d match what init rendered", len(st.Files))})
	default:
		cs = append(cs, check{warn, "Rendered files", describeChanges(edited, missing) + ". Changes belong in compose.override.yml; deploy upgrade refuses to replace these files without --force"})
	}

	if st.Version == b.Version {
		cs = append(cs, check{pass, "Binary version", b.Version})
	} else {
		cs = append(cs, check{warn, "Binary version", fmt.Sprintf("the deployment is %s and this binary is %s; %s", st.Version, b.Version, versionFix(b.Version, st.Version))})
	}
	return cs
}

// seccompCheck starts the deployment's image under the executor's seccomp
// profile. Docker Engine and libseccomp refuse a profile that names a
// syscall they do not know, and on an older host that refusal would
// otherwise first show up as an executor that will not start.
func seccompCheck(ctx context.Context, h *host, dir string, st deployState) check {
	const name = "Seccomp profile"
	image := st.imageRef()
	if _, _, err := h.docker(ctx, "", "image", "inspect", "--format", "{{.Id}}", image); err != nil {
		return check{skip, name, fmt.Sprintf("%s is not on this host yet; this runs once it is pulled or built", image)}
	}
	profile := filepath.Join(dir, filepath.FromSlash(seccompFile))
	if _, err := os.Stat(profile); err != nil {
		return check{fail, name, fmt.Sprintf("%s is missing, and the executor will not start without it", seccompFile)}
	}
	_, errOut, err := h.docker(ctx, "", "run", "--rm", "--pull=never", "--network=none",
		"--security-opt", "seccomp="+profile, "--entrypoint", "true", image)
	if err != nil {
		return check{fail, name, "Docker refused to start a container under it: " + firstLine(errOut, err)}
	}
	return check{pass, name, "Docker starts a container under " + seccompFile}
}

// container is the part of `docker inspect` stackChecks reads.
type container struct {
	Name  string
	State struct {
		Status   string
		ExitCode int
		Health   *struct {
			Status string
		}
	}
	HostConfig struct {
		RestartPolicy struct {
			Name string
		}
	}
}

// stackChecks checks a running stack: its containers, the control service's
// health endpoint, GoTrue's signing keys, and the public URL from the host.
func stackChecks(ctx context.Context, h *host, dir string, st deployState) []check {
	c, up := containerCheck(ctx, h, st.Project)
	if !up {
		return []check{c}
	}
	return []check{c, controlHealthCheck(ctx, h), jwksCheck(ctx, h), publicURLCheck(ctx, h, dir)}
}

// containerCheck reads the project's containers and folds them into one
// check. up is false when there are none to check further, because docker
// could not list them or the project has none. Containers are found by the
// project label compose puts on them, so the check needs nothing from the
// compose file or .env.
func containerCheck(ctx context.Context, h *host, project string) (c check, up bool) {
	ids, errOut, err := h.docker(ctx, "", "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return check{fail, "Containers", "docker ps failed: " + firstLine(errOut, err)}, false
	}
	idList := strings.Fields(string(ids))
	none := check{skip, "Containers", fmt.Sprintf("compose project %s has no containers; the running-stack checks run once it is up", project)}
	if len(idList) == 0 {
		return none, false
	}
	// A container removed between the two calls (a one-shot run with --rm)
	// makes inspect exit non-zero while still printing the rest, so its
	// output is read before its exit status.
	out, errOut, err := h.docker(ctx, "", append([]string{"inspect"}, idList...)...)
	var containers []container
	if jerr := json.Unmarshal(out, &containers); jerr != nil || len(containers) == 0 {
		if err != nil {
			return check{fail, "Containers", "docker inspect failed: " + firstLine(errOut, err)}, false
		}
		if jerr != nil {
			return check{fail, "Containers", "read docker inspect: " + jerr.Error()}, false
		}
		return none, false
	}
	return containersCheck(containers), true
}

func controlHealthCheck(ctx context.Context, h *host) check {
	const name = "Control health"
	code, _, err := h.get(ctx, h.controlURL+"/api/health")
	switch {
	case err != nil:
		return check{fail, name, err.Error()}
	case code != http.StatusOK:
		return check{fail, name, fmt.Sprintf("GET %s/api/health answered %d", h.controlURL, code)}
	}
	return check{pass, name, "GET /api/health answered 200"}
}

// containersCheck folds the project's containers into one line. A container
// with no restart policy is a one-shot (the postinit jobs), which is done
// when it has exited 0; every other container must be running, and healthy
// where it has a healthcheck.
func containersCheck(containers []container) check {
	var problems, starting []string
	for _, c := range containers {
		name := strings.TrimPrefix(c.Name, "/")
		oneShot := c.HostConfig.RestartPolicy.Name == "" || c.HostConfig.RestartPolicy.Name == "no"
		switch {
		case c.State.Status == "exited" && oneShot && c.State.ExitCode == 0:
		case c.State.Status == "exited":
			problems = append(problems, fmt.Sprintf("%s exited %d", name, c.State.ExitCode))
		case c.State.Status != "running":
			problems = append(problems, fmt.Sprintf("%s is %s", name, c.State.Status))
		case c.State.Health == nil, c.State.Health.Status == "healthy":
		case c.State.Health.Status == "starting":
			starting = append(starting, name)
		default:
			problems = append(problems, fmt.Sprintf("%s is %s", name, c.State.Health.Status))
		}
	}
	switch {
	case len(problems) > 0:
		return check{fail, "Containers", strings.Join(problems, "; ")}
	case len(starting) > 0:
		return check{warn, "Containers", "still starting: " + strings.Join(starting, ", ")}
	}
	return check{pass, "Containers", fmt.Sprintf("all %d running and healthy, or finished", len(containers))}
}

// jwksCheck wants GoTrue publishing at least one signing key: with none,
// no session TF verifies can exist.
func jwksCheck(ctx context.Context, h *host) check {
	const name = "GoTrue JWKS"
	url := h.gotrueURL + "/.well-known/jwks.json"
	code, body, err := h.get(ctx, url)
	if err != nil {
		return check{fail, name, err.Error()}
	}
	if code != http.StatusOK {
		return check{fail, name, fmt.Sprintf("GET %s answered %d", url, code)}
	}
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil || len(jwks.Keys) == 0 {
		return check{fail, name, "GoTrue publishes no signing key; check GOTRUE_JWT_KEYS in .env"}
	}
	return check{pass, name, fmt.Sprintf("%d signing key(s)", len(jwks.Keys))}
}

// publicURLCheck reaches the deployment the way its users do. A failure is a
// warning: the stack itself may be fine and the proxy or DNS in front of it
// not, or this host may simply not be able to see its own public name.
func publicURLCheck(ctx context.Context, h *host, dir string) check {
	const name = "Public URL"
	env, err := os.ReadFile(filepath.Join(dir, envFile))
	if err != nil {
		return check{warn, name, fmt.Sprintf("read %s: %v", envFile, err)}
	}
	publicURL := envValues(env).get("TF_PUBLIC_URL")
	if publicURL == "" {
		return check{warn, name, "TF_PUBLIC_URL is not set in " + envFile}
	}
	url := publicURL + "/api/health"
	code, _, err := h.get(ctx, url)
	switch {
	case err != nil:
		return check{warn, name, fmt.Sprintf("%v; check the reverse proxy and DNS in front of the stack", err)}
	case code != http.StatusOK:
		return check{warn, name, fmt.Sprintf("GET %s answered %d; check the reverse proxy in front of the stack", url, code)}
	}
	return check{pass, name, "GET " + url + " answered 200"}
}

// get fetches url and returns its status and up to 1 MiB of its body.
func (h *host) get(ctx context.Context, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

const doctorUsage = `triagefactory deploy doctor — check a deployment.

USAGE
  triagefactory deploy doctor [--dir DIR]

One line per check, each pass, warn, fail or skip. Exits non-zero when any
check fails; warnings do not.

  Preflight      Linux, Docker Engine reachable, Docker Compose 2 or later,
                 cgroup v2, and ports 3000, 9999 and 3030 free (before the
                 first init only)
  Bundle         the state file is present, the rendered files match what
                 init rendered (a warning if not), and this binary is the
                 deployment's version (a warning if not)
  Seccomp        the image starts under docker/seccomp-profile.json, which an
                 older Docker Engine or libseccomp refuses (once the image is
                 on the host)
  Running stack  every container running and healthy, GET /api/health on
                 port 3000, GoTrue publishing a signing key, and the public
                 URL reachable from this host (a warning if not); run once
                 the stack is up
`
