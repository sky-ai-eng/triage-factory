package app

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
)

// composeServiceEnv reads the environment keys each service in the repo's
// docker-compose.yml forwards: a service is a two-space-indented key under
// services:, and its environment a block of six-space-indented KEY: value
// lines under a four-space environment: line.
func composeServiceEnv(t *testing.T) map[string]map[string]bool {
	t.Helper()
	f, err := os.Open("../../docker-compose.yml")
	if err != nil {
		t.Fatalf("open docker-compose.yml: %v", err)
	}
	defer f.Close()

	out := map[string]map[string]bool{}
	var (
		inServices, inEnv bool
		service           string
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 0:
			inServices = trimmed == "services:"
			service, inEnv = "", false
		case !inServices:
		case indent == 2 && strings.HasSuffix(trimmed, ":"):
			service, inEnv = strings.TrimSuffix(trimmed, ":"), false
			out[service] = map[string]bool{}
		case indent == 4:
			inEnv = trimmed == "environment:"
		case indent == 6 && inEnv && service != "":
			if key, _, ok := strings.Cut(trimmed, ":"); ok {
				out[service][key] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	return out
}

// TestCompose_DeploymentCommitSignerIsControlOnly pins where the bundled stack
// sends the deployment commit signer: to the control service, the only place
// a signature is made, and never to the executor, which therefore never holds
// the signing key, whatever an operator puts in .env.
func TestCompose_DeploymentCommitSignerIsControlOnly(t *testing.T) {
	env := composeServiceEnv(t)
	control, executor := env["triagefactory"], env["executor"]
	if len(control) == 0 || len(executor) == 0 {
		t.Fatalf("found no environment for the control (%d keys) or executor (%d keys) service; the compose parse is wrong", len(control), len(executor))
	}
	names := append([]string{commitsigner.EnvDeploymentSignerKey + "_FILE"}, commitsigner.DeploymentSignerEnvNames...)
	for _, name := range names {
		if !control[name] {
			t.Errorf("the control service does not forward %s", name)
		}
		if executor[name] {
			t.Errorf("the executor service forwards %s; an executor must never hold the deployment signer", name)
		}
	}
	for service, keys := range env {
		if service == "triagefactory" {
			continue
		}
		for _, name := range names {
			if keys[name] {
				t.Errorf("service %s forwards %s; only the control service signs", service, name)
			}
		}
	}
}
