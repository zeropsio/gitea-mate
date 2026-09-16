package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The stage tier after someone merged a pull request adding a cache and a
// second runtime service.
const stageImportGrown = `
services:
  - hostname: api
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/api
    zeropsSetup: api
    priority: 5
    verticalAutoscaling:
      minRam: 0.5
  - hostname: web
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/web
    zeropsSetup: web
  - hostname: cache
    type: valkey@7.2
    mode: NON_HA
    priority: 10
`

func TestStartWithoutCodeKeepsEverythingButTheTwoGitFields(t *testing.T) {
	t.Parallel()
	recipe, err := environments.ParseRecipe([]byte(stageImportGrown))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	document, err := environments.StartWithoutCode(recipe.Order())
	if err != nil {
		t.Fatalf("StartWithoutCode: %v", err)
	}

	// The platform cannot clone a private repository, so a service imported
	// from the recipe is created empty and deployed by the broker afterwards.
	for _, gone := range []string{"buildFromGit", "zeropsSetup"} {
		if strings.Contains(document, gone) {
			t.Fatalf("%s survived the conversion:\n%s", gone, document)
		}
	}
	for _, kept := range []string{"hostname: api", "verticalAutoscaling", "minRam: 0.5", "type: valkey@7.2", "mode: NON_HA"} {
		if !strings.Contains(document, kept) {
			t.Fatalf("%q did not survive the conversion:\n%s", kept, document)
		}
	}
	if strings.Count(document, "startWithoutCode: true") != 3 {
		t.Fatalf("the document does not start every service without code:\n%s", document)
	}

	// And the result is a document the recipe parser reads back.
	back, err := environments.ParseRecipe([]byte(document))
	if err != nil {
		t.Fatalf("the converted document does not parse: %v\n%s", err, document)
	}
	if len(back.Services) != 3 {
		t.Fatalf("the converted document carries %d services", len(back.Services))
	}
	for _, service := range back.Services {
		if service.Runtime() {
			t.Fatalf("%s still names a repository after the conversion", service.Hostname)
		}
	}
}

func TestARecipeChangeIsImportedIntoEveryEnvironmentOfItsTier(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()

	// The first pass after a restart only reports: the broker has never seen
	// this recipe before, and a recipe it has not been watching is not a
	// change.
	if _, err := w.pipe.Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	if len(w.zerops.Imports) != 0 {
		t.Fatalf("the first pass imported %v", w.zerops.Imports)
	}

	// Now the tier grows.
	w.gitea.AddFile("acme/group", "main", "3 — Stage/import.yaml", stageImportGrown)
	result, err := w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	if result.Imports != 2 {
		t.Fatalf("the pass imported %d services, want the two the project lacked", result.Imports)
	}
	if len(w.zerops.Imports) != 1 || w.zerops.Imports[0].ProjectID != stagePrj {
		t.Fatalf("imports = %+v", w.zerops.Imports)
	}
	document := w.zerops.Imports[0].Yaml
	for _, want := range []string{"hostname: web", "hostname: cache", "startWithoutCode: true"} {
		if !strings.Contains(document, want) {
			t.Fatalf("the delta does not carry %q:\n%s", want, document)
		}
	}
	// `api` is already in the project, so it is not imported again.
	if strings.Contains(document, "hostname: api") {
		t.Fatalf("the delta re-imported a service the project already has:\n%s", document)
	}
	// And production's tier did not change, so nothing was imported there.
	for _, imported := range w.zerops.Imports {
		if imported.ProjectID == prodPrj {
			t.Fatal("an unchanged tier was imported anyway")
		}
	}
}

func TestAServiceGoneFromTheRecipeIsReportedNeverDeleted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	// The stage project carries a service the recipe never declared.
	w.zerops.SetServices(stagePrj, stageServices()...)

	result, err := w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	found := false
	for _, problem := range result.Problems {
		if strings.Contains(problem, "legacy") && strings.Contains(problem, "never deletes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the orphan was not reported: %v", result.Problems)
	}
	if len(w.zerops.DeletedServices) != 0 {
		t.Fatalf("the broker deleted %v", w.zerops.DeletedServices)
	}
	names := map[string]bool{}
	for _, service := range w.zerops.Services(stagePrj) {
		names[service.Name] = true
	}
	if !names["legacy"] {
		t.Fatal("the orphan is gone from the project")
	}
}

// stageServices is the stage project with one service nothing declares.
func stageServices() []zerops.Service {
	return []zerops.Service{
		{ID: "svc-stage-api", ProjectID: stagePrj, Name: "api", Status: "ACTIVE",
			Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}},
		{ID: "svc-stage-legacy", ProjectID: stagePrj, Name: "legacy", Status: "ACTIVE"},
	}
}
