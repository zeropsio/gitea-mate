package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The import document as import/runner.yaml carries it, placeholders and all.
const runnerImport = `
services:
  - hostname: __HOSTNAME__
    type: ubuntu@26.04
    buildFromGit: https://github.com/zeropsio/gitea-mate
    zeropsSetup: runner
    minContainers: 1
    vault:
      RUNNER_REGISTRATION_TOKEN:
        value: __TOKEN__
        sensitive: true
`

func TestARunnerIsImportedOnAGroupsFirstWorkflow(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.pipe.RunnerImport = runnerImport
	ctx := context.Background()

	if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("imports = %+v", w.zerops.Imports)
	}
	imported := w.zerops.Imports[0]
	if imported.ProjectID != giteaPrj {
		t.Fatalf("the runner was imported into %s, want the Gitea project", imported.ProjectID)
	}
	// docs/vocabulary.md: "runner" plus the slug with its dashes removed, cut
	// to 25 — Zerops hostnames are [a-z0-9].
	if !strings.Contains(imported.Yaml, "hostname: "+registry.RunnerHostname("acme")) {
		t.Fatalf("the document does not name the runner's hostname:\n%s", imported.Yaml)
	}
	for _, placeholder := range []string{"__HOSTNAME__", "__TOKEN__"} {
		if strings.Contains(imported.Yaml, placeholder) {
			t.Fatalf("%s was not filled in", placeholder)
		}
	}
	if !strings.Contains(imported.Yaml, "sensitive: true") {
		t.Fatalf("the registration token is not sensitive:\n%s", imported.Yaml)
	}
}

func TestARunnerIsImportedOnlyOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.pipe.RunnerImport = runnerImport
	ctx := context.Background()

	hostname := registry.RunnerHostname("acme")
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: hostname, Status: "STOPPED"},
	)

	if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if len(w.zerops.Imports) != 0 {
		t.Fatalf("a group that already has a runner imported %+v", w.zerops.Imports)
	}
}

func TestEnsureRunnerRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		org      string
		document string
		want     string
	}{
		{"an org that is not a registered group", "stranger", runnerImport, "not a registered group"},
		{"a broker with no import document", "acme", "", "no runner import document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.pipe.RunnerImport = tc.document
			err := w.pipe.EnsureRunner(context.Background(), tc.org)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("EnsureRunner = %v, want it to mention %q", err, tc.want)
			}
			if len(w.zerops.Imports) != 0 {
				t.Fatalf("a refusal still imported %+v", w.zerops.Imports)
			}
		})
	}
}

func TestARunnerOfAGroupThatLeftTheRegistryIsDeleted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner-acme", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme")},
		zerops.Service{ID: "svc-runner-gone", ProjectID: giteaPrj, Name: registry.RunnerHostname("departed")},
	)

	if _, err := w.pipe.Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	if len(w.zerops.DeletedServices) != 1 || w.zerops.DeletedServices[0] != "svc-runner-gone" {
		t.Fatalf("the pass deleted %v, want only the departed group's runner", w.zerops.DeletedServices)
	}
	// The Gitea project's own services are never touched: a runner is the one
	// service the broker made by itself.
	names := map[string]bool{}
	for _, service := range w.zerops.Services(giteaPrj) {
		names[service.Name] = true
	}
	if !names["web"] || !names[registry.RunnerHostname("acme")] {
		t.Fatalf("the pass removed something it should not have: %v", names)
	}
}

// TestADeletionThatFailsIsReportedNotAssumed: the platform answers a process,
// not a finished deletion, so a pass that did not wait would report a runner
// gone that is still there.
func TestADeletionThatFailsIsReportedNotAssumed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.zerops.FailDeletes = true
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner-gone", ProjectID: giteaPrj, Name: registry.RunnerHostname("departed")},
	)

	result, err := w.pipe.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	found := false
	for _, problem := range result.Problems {
		if strings.Contains(problem, registry.RunnerHostname("departed")) && strings.Contains(problem, "FAILED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failed deletion was not reported: %v", result.Problems)
	}
}
