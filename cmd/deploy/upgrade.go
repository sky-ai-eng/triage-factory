package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/envfile"
)

const (
	// defaultDrainTimeout is how long upgrade waits for drained executors'
	// runs to finish before it recreates the executors anyway.
	defaultDrainTimeout = 10 * time.Minute

	// liveHeartbeat is how recent an executor's heartbeat must be for the
	// drain to count it. The registry keeps a stopped instance's row, with
	// its last run count frozen, for days; waiting on it would only wait
	// out the timeout.
	liveHeartbeat = 30 * time.Second
)

// upgradeOptions are upgrade's flags.
type upgradeOptions struct {
	dir          string
	force        bool
	drainTimeout time.Duration
}

func runUpgrade(ctx context.Context, args []string, b Bundle, h *host) int {
	fset := newFlagSet("upgrade", upgradeUsage, h)
	var o upgradeOptions
	fset.StringVar(&o.dir, "dir", ".", "")
	fset.BoolVar(&o.force, "force", false, "")
	fset.DurationVar(&o.drainTimeout, "drain-timeout", defaultDrainTimeout, "")
	if code := parseFlags(fset, args, h); code >= 0 {
		return code
	}
	if o.drainTimeout < 0 {
		fmt.Fprintf(h.stderr, "deploy upgrade: --drain-timeout %s is negative\n", o.drainTimeout)
		return 2
	}
	if err := upgrade(ctx, h, b, o); err != nil {
		fmt.Fprintf(h.stderr, "deploy upgrade: %v\n", err)
		return 1
	}
	return 0
}

// upgrade moves dir's deployment to this binary's version. Everything that
// can refuse — the version, the new image's signature, an edited rendered
// file, a value the new release needs that .env cannot get, the new image's
// pull and its seccomp profile — is settled before anything changes; from
// the drain on, the stack is being upgraded.
func upgrade(ctx context.Context, h *host, b Bundle, o upgradeOptions) error {
	dir, err := deploymentDir(o.dir)
	if err != nil {
		return err
	}
	h.heading("Preflight")
	checks, held := preflight(ctx, h, false)
	if failed := h.reportAll(checks); failed || !held {
		return errors.New("the host failed preflight; nothing was changed")
	}
	st, err := readDeployment(dir)
	if err != nil {
		return err
	}
	if st.BuildFrom != nil {
		return fmt.Errorf("this deployment runs a source build of %s, which has no release version to move to; run `triagefactory deploy up`, which rebuilds it from the checkout", *st.BuildFrom)
	}
	switch c, ok := compareReleases(b.Version, st.Version); {
	case !ok:
		return fmt.Errorf("this binary is %s, not a release, and the deployment is %s; upgrade moves a deployment to the version of the release binary running it", b.Version, st.Version)
	case c == 0:
		fmt.Fprintf(h.stdout, "\n%s is already at %s; `triagefactory deploy up` starts it if it is not running.\n", dir, st.Version)
		return nil
	case c < 0:
		return fmt.Errorf("the deployment is %s and this binary is %s, which is older; %s", st.Version, b.Version, versionFix(b.Version, st.Version))
	}

	h.heading("Image")
	ref, err := h.resolveImage(ctx, b.Version)
	if err != nil {
		h.report(check{fail, "Pinned", "no verified digest for " + b.Version + "; nothing was changed"})
		return err
	}
	h.report(check{pass, "Pinned", ref})
	h.report(check{pass, "Signature", "verified: " + signerIdentity(b.Version)})
	files, err := render(b, st.Project, source{image: ref})
	if err != nil {
		return err
	}

	h.heading("Rendered files")
	edited, missing, err := changedFiles(dir, st)
	if err != nil {
		return err
	}
	if len(edited)+len(missing) == 0 {
		h.report(check{pass, "Rendered files", fmt.Sprintf("all %d match what deploy rendered", len(st.Files))})
	} else {
		changed := describeChanges(edited, missing)
		if !o.force {
			h.report(check{fail, "Rendered files", changed})
			return fmt.Errorf("these rendered files changed since deploy rendered them, and the upgrade would replace them: %s. Changes belong in compose.override.yml, which deploy never writes; move them there, or pass --force to overwrite them. Nothing was changed", changed)
		}
		h.report(check{warn, "Rendered files", changed + "; --force overwrites them"})
	}

	// What the new release needs in .env, before any executor is drained:
	// compose refuses to start on a missing required value, and a refusal
	// after the drain would leave the executors drained and not restarted.
	h.heading("Environment")
	envBase, err := readEnvBase(b, dir)
	if err != nil {
		return err
	}
	// upgrade takes no input flags: without a terminal to ask on, a value
	// only the operator can supply has to be in .env already.
	if keys := missingInputs(envBase); len(keys) > 0 && !h.terminal {
		h.report(check{fail, "Environment", "missing: " + strings.Join(keys, ", ")})
		return fmt.Errorf("%s does not set %s, which only you can supply; set them there, or run upgrade on a terminal to be asked. Nothing was changed", envFile, strings.Join(keys, ", "))
	}
	envVars, err := planEnvFill(h, envBase, inputFlags{})
	if err != nil {
		return err
	}
	if missing := missingRequired(files[composeFile], envfile.Set(envBase, envVars), h.getenv); len(missing) > 0 {
		h.report(check{fail, "Environment", "missing: " + strings.Join(missing, ", ")})
		return fmt.Errorf("%s's %s requires %s, which %s does not set and deploy cannot generate; set them there (.env.example in the release documents each), then run upgrade again. Nothing was changed", b.Version, composeFile, strings.Join(missing, ", "), envFile)
	}
	if len(envVars) == 0 {
		h.report(check{pass, "Environment", envFile + " holds every value " + b.Version + " needs"})
	} else {
		keys := make([]string, len(envVars))
		for i, v := range envVars {
			keys[i] = v.Key
		}
		h.report(check{pass, "Environment", fmt.Sprintf("%s adds %d value(s) to %s: %s", b.Version, len(keys), envFile, strings.Join(keys, ", "))})
	}

	// The new image, under the new release's profile, before any executor
	// is drained: a host that refuses the profile would leave every
	// executor unable to start once compose recreates it.
	h.heading("Seccomp")
	if err := pullIfMissing(ctx, h, ref); err != nil {
		return fmt.Errorf("%w. Nothing was changed", err)
	}
	probe, err := newSeccompCheck(ctx, h, dir, ref, files[seccompFile])
	if err != nil {
		return err
	}
	if failed := h.reportAll([]check{probe}); failed {
		return fmt.Errorf("%s's executor seccomp profile is refused on this host, so its executor would not start; nothing was changed", b.Version)
	}

	h.heading("Drain")
	drainExecutors(ctx, h, dir, st.Project, o.drainTimeout)

	if len(envVars) > 0 {
		if err := writeEnvFill(h, dir, envBase, envVars); err != nil {
			return fmt.Errorf("%w. The executors are drained until they restart; once the cause is fixed, run `triagefactory deploy upgrade` again", err)
		}
	}
	from, oldFiles := st.Version, st.Files
	st.Version, st.Image, st.Files = b.Version, &ref, map[string]string{}
	for path, data := range files {
		st.Files[path] = sha256Hex(data)
	}
	if err := writeRendered(dir, files, oldFiles, st); err != nil {
		// The state file is written last, so it still names the old
		// version, and the files written so far no longer match its
		// hashes: a re-run needs --force to replace them.
		return fmt.Errorf("%w. The executors are drained until they restart, and %s may be partly re-rendered; once the cause is fixed, run `triagefactory deploy upgrade --force` again", err, dir)
	}
	if err := composeUp(ctx, h, dir, st); err != nil {
		return fmt.Errorf("%w. %s is rendered for %s now; once the cause is fixed, `triagefactory deploy up` starts it", err, dir, st.Version)
	}
	if err := waitHealthy(ctx, h, st); err != nil {
		return err
	}
	fmt.Fprintf(h.stdout, "\nUpgraded %s from %s to %s.\n", dir, from, st.Version)
	return nil
}

func describeChanges(edited, missing []string) string {
	var parts []string
	if len(edited) > 0 {
		parts = append(parts, "edited: "+strings.Join(edited, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "missing: "+strings.Join(missing, ", "))
	}
	return strings.Join(parts, "; ")
}

// writeRendered writes the re-rendered bundle over the old one, removes a
// file the old bundle had (oldFiles, keyed by slash path) and the new one
// does not, and writes the state file last, so a state file that names the
// new version means every file with it was written.
func writeRendered(dir string, files map[string][]byte, oldFiles map[string]string, st deployState) error {
	if err := writeBundleFiles(dir, files); err != nil {
		return err
	}
	for path := range oldFiles {
		if _, ok := files[path]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s, which %s no longer ships: %w", path, st.Version, err)
		}
	}
	return writeState(dir, st)
}

// requiredVar is a ${NAME:?…} or ${NAME?…} interpolation, which compose
// refuses to start without.
var requiredVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):?\?`)

// missingRequired lists the variables compose requires that neither env (a
// .env's content) nor the environment deploy runs compose in sets. Comment
// lines are skipped: compose does not interpolate them.
func missingRequired(compose, env []byte, getenv func(string) string) []string {
	values := envValues(env)
	var missing []string
	for _, line := range strings.Split(string(compose), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range requiredVar.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if values.get(name) == "" && getenv(name) == "" && !slices.Contains(missing, name) {
				missing = append(missing, name)
			}
		}
	}
	slices.Sort(missing)
	return missing
}

// newSeccompCheck starts image under profile, the new release's seccomp
// profile, which is not in dir until the re-render. The docker CLI reads a
// profile itself and hands the daemon its contents, so a copy staged in dir
// for the probe and removed after it is all the probe needs.
func newSeccompCheck(ctx context.Context, h *host, dir, image string, profile []byte) (check, error) {
	f, err := os.CreateTemp(dir, ".seccomp-probe-*.json")
	if err != nil {
		return check{}, fmt.Errorf("stage the new seccomp profile: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(profile)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return check{}, fmt.Errorf("stage the new seccomp profile: %w", err)
	}
	return seccompProbe(ctx, h, image, f.Name()), nil
}

// fleetInstance is the part of `triagefactory instance list --json` the
// drain reads. The control container runs the version being upgraded from,
// so this is read from an older binary than this one.
type fleetInstance struct {
	ID                  string  `json:"id"`
	Role                string  `json:"role"`
	Drain               string  `json:"drain"`
	ActiveRuns          *int    `json:"active_runs"`
	HeartbeatAgeSeconds float64 `json:"heartbeat_age_seconds"`
}

// drainExecutors drains every live executor that is not draining already,
// until its next restart, and waits up to timeout for their runs to finish.
// It drains until restart rather than draining and undraining afterwards,
// because an upgrade that stopped between the two would leave every
// executor healthy and taking no work; and it leaves an executor an operator
// drained alone, so that drain outlives the upgrade.
//
// Nothing here stops the upgrade. An executor stopped with runs in flight
// hands them off with workspace snapshots, and they resume on the upgraded
// executors; the drain only spares them the interruption.
func drainExecutors(ctx context.Context, h *host, dir, project string, timeout time.Duration) {
	const name = "Executors"
	out, errOut, err := h.docker(ctx, dir, "compose", "-p", project, "ps", "--status", "running", "--quiet", "control")
	switch {
	case err != nil:
		h.report(check{warn, name, "could not tell whether control is running (" + firstLine(errOut, err) + "); continuing without a drain"})
		return
	case strings.TrimSpace(string(out)) == "":
		h.report(check{skip, name, "control is not running, so there is nothing to drain"})
		return
	}
	fleet, err := listFleet(ctx, h, dir, project)
	if err != nil {
		h.report(check{warn, name, err.Error() + "; continuing without a drain, so runs in flight are handed off when their executor stops"})
		return
	}
	executors := liveExecutors(fleet)
	if len(executors) == 0 {
		h.report(check{skip, name, "no live executors"})
		return
	}
	var drained, already, failed []string
	for _, e := range executors {
		if e.Drain != "none" {
			already = append(already, e.ID)
			continue
		}
		if err := drainUntilRestart(ctx, h, dir, project, e.ID); err != nil {
			h.report(check{warn, name, fmt.Sprintf("could not drain %s (%v); its runs are handed off when it stops", e.ID, err)})
			failed = append(failed, e.ID)
			continue
		}
		drained = append(drained, e.ID)
	}
	drainedAt := h.now()
	result := check{pass, name, fmt.Sprintf("drained %d until their restart", len(drained))}
	if len(already) > 0 {
		result.detail += fmt.Sprintf("; %d already draining (%s)", len(already), strings.Join(already, ", "))
	}
	if len(failed) > 0 {
		result.status = warn
		result.detail += fmt.Sprintf("; %d not drained (%s)", len(failed), strings.Join(failed, ", "))
	}
	h.report(result)

	// An executor that is not draining keeps claiming work, so its run
	// count says nothing about when the others are done.
	ids := slices.Concat(drained, already)
	if len(ids) == 0 {
		h.report(check{skip, "Runs", "no executor is draining, so there are no runs to wait for"})
		return
	}
	waitForRuns(ctx, h, dir, project, ids, drainedAt, timeout)
}

// waitForRuns waits until the draining executors ids report no runs in
// flight, or until timeout passes. An executor learns of its drain on its
// next heartbeat, and the run count it reports is the one that heartbeat
// wrote, so its zero counts only once it has heartbeat since drainedAt: until
// then it may still be claiming. An executor whose heartbeat has gone stale
// stopped, its count frozen, and is not waited on.
func waitForRuns(ctx context.Context, h *host, dir, project string, ids []string, drainedAt time.Time, timeout time.Duration) {
	const name = "Runs"
	deadline := h.now().Add(timeout)
	last := -1
	for {
		polledAt := h.now()
		fleet, err := listFleet(ctx, h, dir, project)
		if err != nil {
			h.report(check{warn, name, err.Error() + "; continuing, and runs in flight are handed off when their executor stops"})
			return
		}
		runs, unsettled := 0, 0
		for _, inst := range fleet {
			if !slices.Contains(ids, inst.ID) || inst.HeartbeatAgeSeconds > liveHeartbeat.Seconds() {
				continue
			}
			// The list ran after polledAt, so a heartbeat younger than the
			// time since the drain landed after it.
			if inst.HeartbeatAgeSeconds >= polledAt.Sub(drainedAt).Seconds() {
				unsettled++
			}
			if inst.ActiveRuns != nil {
				runs += *inst.ActiveRuns
			}
		}
		if runs == 0 && unsettled == 0 {
			h.report(check{pass, name, "none in flight"})
			return
		}
		if !h.now().Before(deadline) {
			stillGoing := fmt.Sprintf("%d still in flight", runs)
			if runs == 0 {
				stillGoing = fmt.Sprintf("%d executor(s) have not heartbeat since the drain", unsettled)
			}
			h.report(check{warn, name, fmt.Sprintf("%s after %s; continuing: each executor parks its runs with a workspace snapshot when it stops, and they resume on the upgraded executors", stillGoing, timeout)})
			return
		}
		if runs > 0 && runs != last {
			fmt.Fprintf(h.stdout, "        waiting for %d run(s) to finish (up to %s)\n", runs, deadline.Sub(h.now()).Round(time.Second))
			last = runs
		}
		if err := h.sleep(ctx, pollInterval); err != nil {
			h.report(check{warn, name, err.Error()})
			return
		}
	}
}

// drainUntilRestart runs `triagefactory instance drain --until-restart id`
// in the control container.
func drainUntilRestart(ctx context.Context, h *host, dir, project, id string) error {
	_, errOut, err := h.docker(ctx, dir, "compose", "-p", project, "exec", "-T", "control",
		"triagefactory", "instance", "drain", "--until-restart", id)
	if err != nil {
		return errors.New(firstLine(errOut, err))
	}
	return nil
}

// listFleet runs `triagefactory instance list --json` in the control
// container.
func listFleet(ctx context.Context, h *host, dir, project string) ([]fleetInstance, error) {
	out, errOut, err := h.docker(ctx, dir, "compose", "-p", project, "exec", "-T", "control",
		"triagefactory", "instance", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("`triagefactory instance list --json` failed in the control container: %s", firstLine(errOut, err))
	}
	var fleet []fleetInstance
	if err := json.Unmarshal(out, &fleet); err != nil {
		return nil, fmt.Errorf("`triagefactory instance list --json` in the control container printed no instance list (%v); a control binary from before --json existed prints a table instead", err)
	}
	return fleet, nil
}

func liveExecutors(fleet []fleetInstance) []fleetInstance {
	var out []fleetInstance
	for _, inst := range fleet {
		if inst.Role == "executor" && inst.HeartbeatAgeSeconds <= liveHeartbeat.Seconds() {
			out = append(out, inst)
		}
	}
	return out
}

const upgradeUsage = `triagefactory deploy upgrade — move a deployment to this binary's version.

USAGE
  triagefactory deploy upgrade [flags]

FLAGS
  --dir DIR               the deployment directory (default: the current
                          directory)
  --force                 overwrite rendered files that changed since deploy
                          rendered them, after listing them
  --drain-timeout DUR     how long to wait for the executors' runs to finish
                          before recreating them anyway (default: 10m)

Run it after installing a newer binary (brew upgrade triagefactory). On a
deployment already at this binary's version it says so and changes nothing. It
refuses a binary older than the deployment, because migrations only go
forward, and a source build (init --build-from), which has no release version;
re-run deploy up to rebuild one.

WHAT IT DOES
  1. Resolves ghcr.io/sky-ai-eng/triage-factory:<this binary's version> to a
     digest and verifies its cosign signature, as init does. A failure stops the
     upgrade before anything changes.
  2. Checks the rendered files against the hashes deploy recorded. A file that
     changed is listed and the upgrade refused: changes belong in
     compose.override.yml. --force overwrites them.
  3. Works out what the new release needs in .env and that this binary can
     generate, the way a re-run of init does: nothing .env has is changed. A
     value compose requires that .env does not set and deploy cannot generate
     stops the upgrade before anything changes.
  4. Pulls the new image if the host lacks it, and starts it under the new
     release's docker/seccomp-profile.json. A failed pull, or a profile this
     host's Docker Engine or libseccomp refuses, stops the upgrade before
     anything changes.
  5. Drains every live executor until its next restart
     (triagefactory instance drain --until-restart, run in the control
     container) and waits for their runs to finish, up to --drain-timeout. A run
     still in flight after that is parked with a workspace snapshot when its
     executor stops, and resumes on the upgraded executors. An executor an
     operator drained stays drained.
  6. Writes what step 3 found into .env, and re-renders the bundle files and
     the state file.
  7. Runs docker compose -p <project> up -d --scale executor=<N> with the
     stored executor count. control migrates the schema as it boots, and the
     executors come up on the new image; their drain ended when they
     restarted, so there is nothing to undo.
  8. Waits for the stack to be healthy, as deploy up does.

The API is unavailable while control restarts. Zero-downtime upgrades are not
supported.
`
