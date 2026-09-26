package environments_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
)

// The fixture of docs/group-repo.md, spelled out once.
const goodFile = `
version: 1
environments:
  stage:
    tier: stage
    project: m1VrZPJlSnAmnYAvfuEZEg
    sources: [main]
    deploy: on-push
  stage-client-x:
    tier: stage
    project: p2
    sources: [main, feature/invoices]
  production:
    tier: production
    project: p3
    sources: release
    gates:
      requireOnStage: stage
`

func TestParseTheContractsExample(t *testing.T) {
	t.Parallel()
	file, err := environments.Parse([]byte(goodFile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if file.Version != 1 || len(file.Environments) != 3 {
		t.Fatalf("Parse = %+v", file)
	}
	// The order of the file is kept, so a pass walks them the same way twice.
	var names []string
	for _, e := range file.Environments {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "stage,stage-client-x,production" {
		t.Fatalf("order = %v", names)
	}

	stage, _ := file.Environment("stage")
	if stage.Tier != environments.TierStage || stage.Deploy != environments.OnPush || stage.Branch() != "main" {
		t.Fatalf("stage = %+v", stage)
	}
	mixed, _ := file.Environment("stage-client-x")
	// Several sources are realised on env/{name}; the default trigger is
	// on-push.
	if mixed.Branch() != "env/stage-client-x" || mixed.Deploy != environments.OnPush {
		t.Fatalf("stage-client-x = %+v", mixed)
	}
	prod, _ := file.Environment("production")
	if !prod.Release || len(prod.Sources) != 0 || prod.RequireOnStage != "stage" {
		t.Fatalf("production = %+v", prod)
	}
	if len(file.OfTier(environments.TierStage)) != 2 {
		t.Fatalf("OfTier(stage) = %v", file.OfTier(environments.TierStage))
	}
}

func TestParseRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"a version this broker does not read",
			"version: 2\nenvironments:\n  stage:\n    tier: stage\n    project: p\n    sources: [main]\n",
			"version is 2",
		},
		{
			"environments that are a list",
			"version: 1\nenvironments:\n  - stage\n",
			"a mapping of name to environment",
		},
		{
			"environments that are a word",
			"version: 1\nenvironments: stage\n",
			"a mapping of name to environment",
		},
		{
			"a tier that is neither",
			"version: 1\nenvironments:\n  s:\n    tier: dev\n    project: p\n    sources: [main]\n",
			"tier is \"dev\"",
		},
		{
			"an environment with no project",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    sources: [main]\n",
			"no project",
		},
		{
			"production following a branch",
			"version: 1\nenvironments:\n  p:\n    tier: production\n    project: p\n    sources: [main]\n",
			"sources are `release`",
		},
		{
			"production with no sources at all",
			"version: 1\nenvironments:\n  p:\n    tier: production\n    project: p\n",
			"sources are `release`",
		},
		{
			"a stage claiming release",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    project: p\n    sources: release\n",
			"belongs to a production environment",
		},
		{
			"a stage with no sources",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    project: p\n    sources: []\n",
			"at least one source branch",
		},
		{
			"a deploy trigger nobody declared",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    project: p\n    sources: [main]\n    deploy: hourly\n",
			"deploy is \"hourly\"",
		},
		{
			"the same name twice",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    project: p\n    sources: [main]\n  s:\n    tier: stage\n    project: q\n    sources: [main]\n",
			"declared twice",
		},
		{
			"the same source branch twice",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    project: p\n    sources: [main, main]\n",
			"listed twice",
		},
		{
			"sources that are neither a list nor release",
			"version: 1\nenvironments:\n  s:\n    tier: stage\n    project: p\n    sources: main\n",
			"not \"main\"",
		},
		{
			"a document that is not yaml at all",
			"version: 1\nenvironments: [\n",
			"environments.yaml:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := environments.Parse([]byte(tc.body))
			if err == nil {
				t.Fatalf("Parse took %q", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

const stageRecipe = `
services:
  - hostname: db
    type: postgresql@18
    mode: NON_HA
    priority: 10

  - hostname: api
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/api
    zeropsSetup: api
    priority: 5

  - hostname: web
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/web
    zeropsSetup: web
`

func TestRecipeMapsAHostnameToItsRepository(t *testing.T) {
	t.Parallel()
	recipe, err := environments.ParseRecipe([]byte(stageRecipe))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}

	for _, tc := range []struct {
		hostname  string
		owner     string
		repo      string
		setup     string
		isRuntime bool
	}{
		{"api", "acme", "api", "api", true},
		{"web", "acme", "web", "web", true},
		{"db", "", "", "", false},
	} {
		t.Run(tc.hostname, func(t *testing.T) {
			service, ok := recipe.Service(tc.hostname)
			if !ok {
				t.Fatalf("the recipe has no %s", tc.hostname)
			}
			owner, repo, _ := service.Repository()
			if owner != tc.owner || repo != tc.repo || service.ZeropsSetup != tc.setup {
				t.Fatalf("%s -> %s/%s setup %q, want %s/%s setup %q",
					tc.hostname, owner, repo, service.ZeropsSetup, tc.owner, tc.repo, tc.setup)
			}
			if service.Runtime() != tc.isRuntime {
				t.Fatalf("%s Runtime() = %v, want %v", tc.hostname, service.Runtime(), tc.isRuntime)
			}
		})
	}

	// Priority orders a deploy: higher first, ties in file order.
	var order []string
	for _, s := range recipe.Order() {
		order = append(order, s.Hostname)
	}
	if strings.Join(order, ",") != "db,api,web" {
		t.Fatalf("Order = %v", order)
	}
	var runtimes []string
	for _, s := range recipe.Runtimes() {
		runtimes = append(runtimes, s.Hostname)
	}
	if strings.Join(runtimes, ",") != "api,web" {
		t.Fatalf("Runtimes = %v", runtimes)
	}
}

func TestRepositoryOfAClonelessService(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		url   string
		owner string
		repo  string
		ok    bool
	}{
		{"the canonical clone URL", "https://web-1234-3000.prg1.zerops.app/acme/api", "acme", "api", true},
		{"a .git suffix a person wrote by hand", "https://g.example/acme/api.git", "acme", "api", true},
		{"a trailing slash", "https://g.example/acme/api/", "acme", "api", true},
		{"a managed service names none", "", "", "", false},
		{"something that is not a URL", "api", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, ok := environments.RecipeService{BuildFromGit: tc.url}.Repository()
			if owner != tc.owner || repo != tc.repo || ok != tc.ok {
				t.Fatalf("Repository(%q) = %q, %q, %v", tc.url, owner, repo, ok)
			}
		})
	}
}

func TestParseRecipeRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a service with no hostname", "services:\n  - type: nodejs@22\n", "has no hostname"},
		{"a hostname used twice", "services:\n  - hostname: api\n  - hostname: api\n", "used twice"},
		{"no services", "services: []\n", "no services"},
		{"a document that is not yaml", "services: [\n", "import.yaml:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := environments.ParseRecipe([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseRecipe = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestReadFromTheGroupRepo(t *testing.T) {
	t.Parallel()
	f := giteatest.New(t)
	f.AddRepo("acme/group", "main")
	f.AddFile("acme/group", "main", "environments.yaml", goodFile)
	f.AddFile("acme/group", "main", "3 — Stage/import.yaml", stageRecipe)
	f.AddFile("acme/group", "main", "4 — Small Production/import.yaml", stageRecipe)
	f.AddFile("acme/group", "main", "0 — AI Agent/import.yaml", stageRecipe)
	client := f.Client()
	ctx := context.Background()

	file, err := environments.Read(ctx, client, "acme", "group")
	if err != nil || len(file.Environments) != 3 {
		t.Fatalf("Read = %+v, %v", file, err)
	}

	for _, tc := range []struct {
		tier environments.Tier
		dir  string
	}{
		{environments.TierStage, "3 — Stage"},
		{environments.TierProduction, "4 — Small Production"},
	} {
		t.Run(string(tc.tier), func(t *testing.T) {
			dir, err := environments.TierDir(ctx, client, "acme", "group", tc.tier)
			if err != nil || dir != tc.dir {
				t.Fatalf("TierDir(%s) = %q, %v, want %q", tc.tier, dir, err, tc.dir)
			}
			recipe, path, err := environments.ReadRecipe(ctx, client, "acme", "group", tc.tier)
			if err != nil {
				t.Fatalf("ReadRecipe(%s): %v", tc.tier, err)
			}
			if path != tc.dir+"/import.yaml" || len(recipe.Runtimes()) != 2 {
				t.Fatalf("ReadRecipe(%s) = %q, %d runtimes", tc.tier, path, len(recipe.Runtimes()))
			}
		})
	}
}

// A file that declares nothing is a group that has declared nothing, as a
// missing file is — the state the app leaves when a group's last environment
// is taken out. Refused, it failed every webhook and deploy pass of the group
// (measured 2026-09-24, `medusa`: 17:04:54Z–17:09Z, until an entry was merged).
func TestParseAFileThatDeclaresNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"no environments key", "version: 1\n"},
		{"a bare environments key", "version: 1\nenvironments:\n"},
		{"an empty mapping", "version: 1\nenvironments: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file, err := environments.Parse([]byte(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if file.Version != 1 || len(file.Environments) != 0 {
				t.Fatalf("Parse = %+v, want version 1 and no environments", file)
			}
		})
	}
}

func TestReadAGroupThatHasDeclaredNothing(t *testing.T) {
	t.Parallel()
	f := giteatest.New(t)
	f.AddRepo("acme/group", "main")
	// A group repo with no environments.yaml is a group with no stage, not a
	// group the broker could not read.
	file, err := environments.Read(context.Background(), f.Client(), "acme", "group")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(file.Environments) != 0 {
		t.Fatalf("Read = %+v", file)
	}
}

func TestRecipeBlocksAreCanonical(t *testing.T) {
	t.Parallel()
	const written = `
services:
  - hostname: api
    type: nodejs@22
    zeropsSetup: api
    verticalAutoscaling:
      minRam: 0.5
      maxRam: 4
`
	const reordered = `
services:
  - hostname: api
    verticalAutoscaling:
      maxRam: 4
      minRam: 0.5
    zeropsSetup: api
    type: nodejs@22
`
	const rescaled = `
services:
  - hostname: api
    type: nodejs@22
    zeropsSetup: api
    verticalAutoscaling:
      minRam: 1
      maxRam: 4
`
	blocks := func(body string) map[string]string {
		t.Helper()
		recipe, err := environments.ParseRecipe([]byte(body))
		if err != nil {
			t.Fatalf("ParseRecipe: %v", err)
		}
		return recipe.Blocks()
	}

	if blocks(written)["api"] != blocks(reordered)["api"] {
		t.Fatal("a reordering reads as a change")
	}
	if blocks(written)["api"] == blocks(rescaled)["api"] {
		t.Fatal("a scaling change does not read as one")
	}
	if !strings.Contains(blocks(written)["api"], "minRam: 0.5") {
		t.Fatalf("the block lost its scaling: %q", blocks(written)["api"])
	}
}
