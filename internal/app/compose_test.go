package app

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
)

// composeServiceEnv reads the environment each service in the repo's
// docker-compose.yml sets, as the unexpanded value of each key: a service is a
// two-space-indented key under services:, and its environment a block of
// six-space-indented KEY: value lines under a four-space environment: line.
func composeServiceEnv(t *testing.T) map[string]map[string]string {
	t.Helper()
	f, err := os.Open("../../docker-compose.yml")
	if err != nil {
		t.Fatalf("open docker-compose.yml: %v", err)
	}
	defer f.Close()

	out := map[string]map[string]string{}
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
			out[service] = map[string]string{}
		case indent == 4:
			inEnv = trimmed == "environment:"
		case indent == 6 && inEnv && service != "":
			if key, value, ok := strings.Cut(trimmed, ":"); ok {
				out[service][key] = strings.TrimSpace(value)
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
	control, executor := env["control"], env["executor"]
	if len(control) == 0 || len(executor) == 0 {
		t.Fatalf("found no environment for the control (%d keys) or executor (%d keys) service; the compose parse is wrong", len(control), len(executor))
	}
	names := append([]string{commitsigner.EnvDeploymentSignerKey + "_FILE"}, commitsigner.DeploymentSignerEnvNames...)
	for service, keys := range env {
		for _, name := range names {
			if _, forwards := keys[name]; forwards != (service == "control") {
				t.Errorf("service %s forwards %s = %v; only the control service may, and it must", service, name, forwards)
			}
		}
	}
}

// TestCompose_ObjectStorePairsPerRole pins which object-store key pair the
// bundled stack hands each TF service. The binary reads the same names on
// every role, so this file is the whole of the scoping: control gets the
// knowledge writer and no workspace pair, an executor the workspace pair and
// the knowledge reader under the writer's names, and neither gets the
// SeaweedFS admin pair.
func TestCompose_ObjectStorePairsPerRole(t *testing.T) {
	env := composeServiceEnv(t)
	control, executor := env["control"], env["executor"]
	if len(control) == 0 || len(executor) == 0 {
		t.Fatalf("found no environment for the control (%d keys) or executor (%d keys) service; the compose parse is wrong", len(control), len(executor))
	}
	for _, key := range []string{"TF_WORKSPACE_BUCKET", "TF_WORKSPACE_ACCESS_KEY", "TF_WORKSPACE_SECRET_KEY"} {
		if _, ok := control[key]; ok {
			t.Errorf("control sets %s; control never touches workspace snapshots and must hold no workspace pair", key)
		}
		if _, ok := executor[key]; !ok {
			t.Errorf("executor does not set %s", key)
		}
	}
	for service, want := range map[string]map[string]string{
		"control": {
			"TF_KNOWLEDGE_ACCESS_KEY": "${TF_KNOWLEDGE_ACCESS_KEY:?",
			"TF_KNOWLEDGE_SECRET_KEY": "${TF_KNOWLEDGE_SECRET_KEY:?",
		},
		"executor": {
			"TF_KNOWLEDGE_ACCESS_KEY": "${TF_KNOWLEDGE_READER_ACCESS_KEY:?",
			"TF_KNOWLEDGE_SECRET_KEY": "${TF_KNOWLEDGE_READER_SECRET_KEY:?",
		},
	} {
		for key, prefix := range want {
			if got := env[service][key]; !strings.HasPrefix(got, prefix) {
				t.Errorf("service %s sets %s = %q; want it from %s…}", service, key, got, prefix)
			}
		}
	}
	for _, service := range []string{"control", "executor"} {
		for key, value := range env[service] {
			if strings.Contains(value, "SEAWEEDFS_ADMIN_") {
				t.Errorf("service %s sets %s = %q; the SeaweedFS admin pair is seaweedfs-postinit's alone", service, key, value)
			}
		}
	}
}
