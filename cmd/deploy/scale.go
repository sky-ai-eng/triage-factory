package deploy

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// scaleDown removes the executors beyond n, draining each first. compose
// would stop them itself when it scales the service down, but without a
// drain, and it picks which; removing them here first lets deploy drain
// exactly the ones that go, and compose then has none left to remove. They
// are the highest-numbered, the ones compose keeps last.
//
// As in upgrade, a drain problem is a warning: an executor stopped with
// runs in flight hands them off with workspace snapshots. A stop or a
// removal that fails is an error, because compose would then remove other
// executors, undrained.
func scaleDown(ctx context.Context, h *host, dir, project string, n int, timeout time.Duration) error {
	containers, err := projectContainers(ctx, h, project)
	if err != nil {
		return err
	}
	var executors []container
	for _, c := range containers {
		if c.Config.Labels[serviceLabel] == "executor" {
			executors = append(executors, c)
		}
	}
	if len(executors) <= n {
		return nil
	}
	slices.SortFunc(executors, func(a, b container) int {
		return cmp.Compare(containerNumber(b), containerNumber(a))
	})
	going := executors[:len(executors)-n]

	h.heading("Scale down")
	const name = "Executors"
	var names, containerIDs, drained []string
	for _, c := range going {
		names = append(names, strings.TrimPrefix(c.Name, "/"))
		containerIDs = append(containerIDs, c.ID)
		if c.State.Status != "running" {
			continue
		}
		id, err := instanceIDOf(ctx, h, c)
		if err == nil {
			err = drainUntilRestart(ctx, h, dir, project, id)
		}
		if err != nil {
			h.report(check{warn, name, fmt.Sprintf("could not drain %s (%v); its runs are handed off when it stops", strings.TrimPrefix(c.Name, "/"), err)})
			continue
		}
		drained = append(drained, id)
	}
	h.report(check{pass, name, fmt.Sprintf("%d of %d to remove: %s; drained %d until they stop", len(going), len(executors), strings.Join(names, ", "), len(drained))})
	if len(drained) > 0 {
		waitForRuns(ctx, h, dir, project, drained, h.now(), timeout)
	}

	// docker stop sends SIGTERM and waits the container's own stop timeout,
	// the hand-off compose's scale-down would give it.
	if _, errOut, err := h.docker(ctx, "", append([]string{"stop"}, containerIDs...)...); err != nil {
		return fmt.Errorf("stop %s: %s; they are drained until they restart, so `docker compose -p %s restart executor` puts them back to work", strings.Join(names, ", "), firstLine(errOut, err), project)
	}
	if _, errOut, err := h.docker(ctx, "", append([]string{"rm"}, containerIDs...)...); err != nil {
		return fmt.Errorf("remove %s: %s", strings.Join(names, ", "), firstLine(errOut, err))
	}
	h.report(check{pass, "Removed", strings.Join(names, ", ")})
	return nil
}

// containerNumber is the replica number compose gave c, 0 when it has none.
func containerNumber(c container) int {
	n, _ := strconv.Atoi(c.Config.Labels[numberLabel])
	return n
}

// instanceIDOf reads the registry id of the executor running in c: the
// identity file in its state root.
func instanceIDOf(ctx context.Context, h *host, c container) (string, error) {
	out, errOut, err := h.docker(ctx, "", "exec", c.ID, "sh", "-c", `cat "${TF_STATE_ROOT:-/data}/instance-id"`)
	if err != nil {
		return "", fmt.Errorf("read its instance id: %s", firstLine(errOut, err))
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("its instance id file is empty")
	}
	return id, nil
}
