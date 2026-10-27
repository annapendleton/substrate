// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package kubectlate runs the kubectl-ate binary against a real ate-api-server.
//
// The kubectl-ate unit tests cover flag parsing, manifest decoding and the
// command runners against fake clients. Nothing else drives the CLI end to
// end, so a change that breaks it against the real server (authentication,
// authorization, or the output of real responses) would pass CI. These tests
// run each verb once against the installed system, using the counter fixture.
package kubectlate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/e2e"
)

// kubectlAte is the kubectl-ate binary built for this run, pointed at the
// cluster under test.
type kubectlAte struct {
	bin string
}

// buildKubectlAte returns the prebuilt binary named by KUBECTL_ATE_BIN (CI
// sets it), or else compiles ./cmd/kubectl-ate from the checkout under test,
// so the CLI and the server come from the same commit.
func buildKubectlAte(t *testing.T) *kubectlAte {
	t.Helper()
	if bin := os.Getenv("KUBECTL_ATE_BIN"); bin != "" {
		return &kubectlAte{bin: bin}
	}
	root, err := e2e.FindRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "kubectl-ate")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/kubectl-ate")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building kubectl-ate: %v\n%s", err, out)
	}
	return &kubectlAte{bin: bin}
}

// run executes kubectl-ate with args, plus the kubeconfig and context the
// suite was given, and returns its stdout.
func (k *kubectlAte) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if e2e.KubeConfig != "" {
		args = append(args, "--kubeconfig", e2e.KubeConfig)
	}
	if e2e.KubeContext != "" {
		args = append(args, "--context", e2e.KubeContext)
	}
	t.Logf("kubectl ate %s", strings.Join(args, " "))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// mustRun is run, failing the test on error.
func (k *kubectlAte) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := k.run(t, args...)
	if err != nil {
		t.Fatalf("kubectl ate %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// getJSON runs a command with -o json and decodes the result.
func (k *kubectlAte) getJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	return decode(t, k.mustRun(t, append(args, "-o", "json")...))
}

func decode(t *testing.T, out string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decoding kubectl ate output: %v\n%s", err, out)
	}
	return v
}

// field walks nested JSON objects by key.
func field(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// resume runs `resume actor` and returns the resumed actor. Like
// e2e.ResumeActorAwaitCapacity, it retries ResourceExhausted (no free worker
// while other suites hold the shared pool) and Aborted for up to 90s.
func (k *kubectlAte) resume(t *testing.T, atespace, name string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := k.run(t, "resume", "actor", name, "-a", atespace, "-o", "json")
		if err == nil {
			return decode(t, out)
		}
		retryable := strings.Contains(err.Error(), "ResourceExhausted") || strings.Contains(err.Error(), "Aborted")
		if !retryable || time.Now().After(deadline) {
			t.Fatalf("kubectl ate resume actor %s: %v", name, err)
		}
		t.Logf("resume actor %s: %v; retrying", name, err)
		time.Sleep(2 * time.Second)
	}
}

func uniqueName(base string) string {
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func TestKubectlAte(t *testing.T) {
	fixture := e2e.SubstrateCounterFixture()
	e2e.WaitForSubstrateTemplateReady(t.Context(), t, e2e.GetClients(), fixture.Atespace, fixture.Name)
	k := buildKubectlAte(t)

	// The subtests use their own uniquely named resources, so they run in
	// parallel: the suite takes as long as the actor lifecycle, not the sum.

	t.Run("GetWorkers", func(t *testing.T) {
		t.Parallel()
		workers, _ := k.getJSON(t, "get", "workers")["workers"].([]any)
		if len(workers) == 0 {
			t.Fatal("kubectl ate get workers returned no workers")
		}
	})

	t.Run("GetActorTemplate", func(t *testing.T) {
		t.Parallel()
		tmpl := k.getJSON(t, "get", "actor-template", fixture.Name, "-a", fixture.Atespace)
		if got := field(tmpl, "metadata", "name"); got != fixture.Name {
			t.Errorf("metadata.name = %v, want %s", got, fixture.Name)
		}
		if field(tmpl, "resources", "limits", "cpuMillicores") == nil || field(tmpl, "resources", "limits", "memoryBytes") == nil {
			t.Errorf("template has no cpuMillicores/memoryBytes limits: %v", field(tmpl, "resources"))
		}
	})

	t.Run("AtespaceLifecycle", func(t *testing.T) {
		t.Parallel()
		name := uniqueName("kubectl-ate-e2e")
		k.mustRun(t, "create", "atespace", name)
		t.Cleanup(func() { _, _ = k.run(t, "delete", "atespace", name) })
		if got := field(k.getJSON(t, "get", "atespace", name), "metadata", "name"); got != name {
			t.Errorf("get atespace: metadata.name = %v, want %s", got, name)
		}
		k.mustRun(t, "delete", "atespace", name)
		if _, err := k.run(t, "get", "atespace", name); err == nil || !strings.Contains(err.Error(), "NotFound") {
			t.Errorf("get atespace after delete: err = %v, want NotFound", err)
		}
	})

	t.Run("ActorLifecycle", func(t *testing.T) {
		t.Parallel()
		ns, name, tag := fixture.Atespace, uniqueName("kubectl-ate-e2e"), uniqueName("kubectl-ate-e2e-tag")
		k.mustRun(t, "create", "actor", name, "--template", fixture.Name, "-a", ns)
		t.Cleanup(func() { _, _ = k.run(t, "delete", "actor", name, "-a", ns) })
		if got := field(k.getJSON(t, "get", "actor", name, "-a", ns), "actorTemplate", "name"); got != fixture.Name {
			t.Errorf("actorTemplate.name = %v, want %s", got, fixture.Name)
		}
		// ResumeActor returns once the actor is running.
		if got := field(k.resume(t, ns, name), "status", "state"); got != "ACTOR_STATE_RUNNING" {
			t.Fatalf("state after resume = %v, want ACTOR_STATE_RUNNING", got)
		}
		// --mode=sync returns once the snapshot is in object storage, so the
		// actor is already SUSPENDED and can be tagged right away.
		k.mustRun(t, "suspend", "actor", name, "-a", ns, "--mode=sync")
		if got := field(k.getJSON(t, "get", "actor", name, "-a", ns), "status", "state"); got != "ACTOR_STATE_SUSPENDED" {
			t.Fatalf("state after suspend --mode=sync = %v, want ACTOR_STATE_SUSPENDED", got)
		}

		k.mustRun(t, "create", "tag", tag, "-a", ns, "--actor", name)
		t.Cleanup(func() { _, _ = k.run(t, "delete", "tag", tag, "-a", ns) })
		if out := k.mustRun(t, "get", "tags", "-a", ns); !strings.Contains(out, tag) {
			t.Errorf("get tags does not list %s:\n%s", tag, out)
		}
		k.mustRun(t, "delete", "tag", tag, "-a", ns)

		k.mustRun(t, "delete", "actor", name, "-a", ns)
		if _, err := k.run(t, "get", "actor", name, "-a", ns); err == nil || !strings.Contains(err.Error(), "NotFound") {
			t.Errorf("get actor after delete: err = %v, want NotFound", err)
		}
	})
}
