package deploy_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
)

const resolveEnvironments = `
version: 1
environments:
  stage:
    tier: stage
    project: prj-stage
    sources: [main]
  production:
    tier: production
    project: prj-prod
    sources: release
    gates:
      requireOnStage: stage
`

const stageTier = `
services:
  - hostname: db
    type: postgresql@18
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

const productionTier = `
services:
  - hostname: db
    type: postgresql@18
    priority: 10
  - hostname: api
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/api
    zeropsSetup: api-prod
    priority: 5
  - hostname: web
    type: nodejs@22
    buildFromGit: https://gitea.example/acme/web
    zeropsSetup: web-prod
`

func resolvePlan(t *testing.T) deploy.Plan {
	t.Helper()
	file, err := environments.Parse([]byte(resolveEnvironments))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	stage, err := environments.ParseRecipe([]byte(stageTier))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	prod, err := environments.ParseRecipe([]byte(productionTier))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	return deploy.Plan{
		Slug: "acme", File: file,
		Recipes: map[environments.Tier]environments.Recipe{
			environments.TierStage: stage, environments.TierProduction: prod,
		},
	}
}

func resolver(t *testing.T) (*giteatest.Fake, *deploy.Resolver) {
	t.Helper()
	g := giteatest.New(t)
	g.AddRepo("acme/group", "main")
	g.AddRepo("acme/api", "main")
	g.AddRepo("acme/web", "main")
	client := g.Client()
	return g, &deploy.Resolver{Gitea: client, Merger: &deploy.Merger{Gitea: client}}
}

func TestResolveAStageFollowsItsBranch(t *testing.T) {
	t.Parallel()
	g, r := resolver(t)
	g.SetBranch("acme/api", "main", apiSha)
	g.SetBranch("acme/web", "main", webSha)
	plan := resolvePlan(t)
	env, _ := plan.File.Environment("stage")

	targets, problems, err := r.Resolve(context.Background(), plan, env, "")
	if err != nil || len(problems) != 0 {
		t.Fatalf("Resolve = %v, %v", problems, err)
	}
	// The tier's priority order, and the managed database is not a target.
	if len(targets) != 2 || targets[0].Service != "api" || targets[1].Service != "web" {
		t.Fatalf("targets = %+v", targets)
	}
	if targets[0].Sha != apiSha || targets[0].VersionName != apiSha || targets[0].Setup != "api" {
		t.Fatalf("api = %+v", targets[0])
	}
	if targets[0].Gate != nil {
		t.Fatalf("a stage carries a gate: %+v", targets[0])
	}
}

func TestResolveOneServiceOnly(t *testing.T) {
	t.Parallel()
	g, r := resolver(t)
	g.SetBranch("acme/api", "main", apiSha)
	plan := resolvePlan(t)
	env, _ := plan.File.Environment("stage")

	targets, _, err := r.Resolve(context.Background(), plan, env, "api")
	if err != nil || len(targets) != 1 || targets[0].Service != "api" {
		t.Fatalf("Resolve = %+v, %v", targets, err)
	}

	// A service the tier does not carry, and one that is not a runtime.
	for _, name := range []string{"ghost", "db"} {
		if _, _, err := r.Resolve(context.Background(), plan, env, name); !errors.Is(err, deploy.ErrUnknownService) {
			t.Fatalf("Resolve(%q) = %v, want ErrUnknownService", name, err)
		}
	}
}

func TestResolveProductionFollowsTheApprovedTag(t *testing.T) {
	t.Parallel()
	g, r := resolver(t)
	client := g.Client()
	when := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	addTag(t, g, "v1.0.0", "commit-1", when, "api "+apiSha+"\nweb "+webSha+"\n")
	judge(t, client, "commit-1", "v1.0.0", deploy.ReleaseApproved)

	plan := resolvePlan(t)
	env, _ := plan.File.Environment("production")

	targets, problems, err := r.Resolve(context.Background(), plan, env, "")
	if err != nil || len(problems) != 0 {
		t.Fatalf("Resolve = %v, %v", problems, err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %+v", targets)
	}
	api := targets[0]
	if api.Sha != apiSha {
		t.Fatalf("api is at %q, want the tag's commit", api.Sha)
	}
	// docs/group-repo.md: production's version is `{sha} {tag} {tagger}`.
	if api.VersionName != apiSha+" v1.0.0 u-abc" {
		t.Fatalf("api's version name is %q", api.VersionName)
	}
	if api.Setup != "api-prod" {
		t.Fatalf("api's setup is %q, want the production tier's", api.Setup)
	}
	if api.Gate == nil || api.Gate.Environment != "stage" || api.Gate.Project != "prj-stage" {
		t.Fatalf("api's gate is %+v", api.Gate)
	}
}

func TestResolveProductionWithNothingApproved(t *testing.T) {
	t.Parallel()
	_, r := resolver(t)
	plan := resolvePlan(t)
	env, _ := plan.File.Environment("production")

	if _, _, err := r.Resolve(context.Background(), plan, env, ""); !errors.Is(err, deploy.ErrNoRelease) {
		t.Fatalf("Resolve = %v, want ErrNoRelease", err)
	}
}

func TestResolveProductionReportsAServiceTheTagOmits(t *testing.T) {
	t.Parallel()
	g, r := resolver(t)
	client := g.Client()
	addTag(t, g, "v1.0.0", "commit-1", time.Now(), "api "+apiSha+"\n")
	judge(t, client, "commit-1", "v1.0.0", deploy.ReleaseApproved)

	plan := resolvePlan(t)
	env, _ := plan.File.Environment("production")
	targets, problems, err := r.Resolve(context.Background(), plan, env, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0].Service != "api" {
		t.Fatalf("targets = %+v", targets)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "web") {
		t.Fatalf("problems = %v, want one naming web", problems)
	}
}

// ---------------------------------------------------------------------------
// The merge
// ---------------------------------------------------------------------------

const mixedEnvironments = `
version: 1
environments:
  stage-client-x:
    tier: stage
    project: prj-stage
    sources: [main, feature/invoices]
`

// commit is one step of a git fixture: a branch, where it starts from the
// first time it is named, and the files the commit writes.
type commit struct {
	branch string
	from   string
	files  map[string]string
}

// gitRepo builds a repository on disk from a list of commits, so the merge
// runs against real git and no network.
func gitRepo(t *testing.T, commits ...commit) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on the path")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main", ".")
	run("config", "user.name", "fixture")
	run("config", "user.email", "fixture@example.invalid")
	// The fixture is a non-bare origin, so a push to a branch it is not on has
	// to be allowed.
	run("config", "receive.denyCurrentBranch", "ignore")

	for i, c := range commits {
		switch {
		case i == 0:
		case c.from != "":
			run("checkout", "--quiet", "-B", c.branch, c.from)
		default:
			run("checkout", "--quiet", c.branch)
		}
		for name, body := range c.files {
			writeFile(t, dir+"/"+name, body)
			run("add", name)
		}
		run("commit", "--quiet", "-m", "on "+c.branch)
	}
	run("checkout", "--quiet", "main")
	return dir
}

func TestMergeSeveralSourcesIntoTheEnvironmentBranch(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t,
		commit{branch: "main", files: map[string]string{"a.txt": "a\n"}},
		commit{branch: "feature/invoices", from: "main", files: map[string]string{"b.txt": "b\n"}},
		commit{branch: "main", files: map[string]string{"a.txt": "a and more\n"}},
	)

	file, err := environments.Parse([]byte(mixedEnvironments))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	env := file.Environments[0]

	g := giteatest.New(t)
	g.AddRepo("acme/api", "main")
	merger := &deploy.Merger{
		Gitea:    g.Client(),
		CloneURL: func(context.Context, string, string) (string, error) { return repo, nil },
		Timeout:  30 * time.Second,
	}

	head, err := merger.Head(context.Background(), "acme", "api", env)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if len(head) != 40 {
		t.Fatalf("Head = %q, want a full sha", head)
	}
	// The merge landed on the environment's own branch in the origin, and only
	// there: `env/*` is the broker's.
	if got := showRef(t, repo, "refs/heads/env/stage-client-x"); got != head {
		t.Fatalf("env/stage-client-x is at %q, want %q", got, head)
	}
}

func TestAConflictKeepsTheLastGoodMerge(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t,
		commit{branch: "main", files: map[string]string{"same.txt": "base\n"}},
		commit{branch: "feature/invoices", from: "main", files: map[string]string{"same.txt": "another\n"}},
		commit{branch: "main", files: map[string]string{"same.txt": "one\n"}},
	)

	file, err := environments.Parse([]byte(mixedEnvironments))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	env := file.Environments[0]

	g := giteatest.New(t)
	g.AddRepo("acme/api", "main")
	// The environment is already live on an earlier merge.
	g.SetBranch("acme/api", "env/stage-client-x", oldSha)
	g.SetBranch("acme/api", "main", apiSha)
	client := g.Client()
	merger := &deploy.Merger{Gitea: client, CloneURL: func(context.Context, string, string) (string, error) { return repo, nil }, Timeout: 30 * time.Second}

	head, err := merger.Head(context.Background(), "acme", "api", env)
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("Head = %q, %v, want ErrConflict", head, err)
	}
	if head != oldSha {
		t.Fatalf("Head = %q, want the last good merge %q", head, oldSha)
	}
	if got := showRef(t, repo, "refs/heads/env/stage-client-x"); got != "" {
		t.Fatalf("a conflict still pushed env/stage-client-x (%q)", got)
	}

	// The resolver reports it on the head of the first source, and deploys
	// nothing.
	r := &deploy.Resolver{Gitea: client, Merger: merger}
	plan := deploy.Plan{Slug: "acme", File: file, Recipes: map[environments.Tier]environments.Recipe{
		environments.TierStage: mustRecipe(t, stageTier),
	}}
	targets, problems, err := r.Resolve(context.Background(), plan, env, "api")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 0 || len(problems) != 1 {
		t.Fatalf("Resolve = %+v, %v", targets, problems)
	}
	var reported gitea.CommitStatus
	for _, s := range g.Statuses("acme/api", apiSha) {
		if s.Context == deploy.MergeContext("stage-client-x") {
			reported = s
		}
	}
	if reported.State != "failure" {
		t.Fatalf("the conflict was not reported on the first source's head: %+v", reported)
	}
}

func mustRecipe(t *testing.T, body string) environments.Recipe {
	t.Helper()
	recipe, err := environments.ParseRecipe([]byte(body))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	return recipe
}

func showRef(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
