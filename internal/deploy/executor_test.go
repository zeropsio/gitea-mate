package deploy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

// world is one group with a stage and a production project, one service `api`
// whose repository carries two setups, and a commit ready to deploy.
type world struct {
	gitea    *giteatest.Fake
	zerops   *zeropstest.Fake
	records  *deploy.Records
	executor *deploy.Executor
}

const apiYaml = `
zerops:
  - setup: api
    build:
      base: nodejs@22
      buildCommands: [npm ci]
      deployFiles: ./
    run:
      base: nodejs@22
      start: npm start
  - setup: api-prod
    build:
      base: nodejs@22
      buildCommands: [npm ci]
      deployFiles: ./
    run:
      base: nodejs@22
      start: npm start
      envVariables: {NODE_ENV: production}
  - setup: api-different
    build:
      base: nodejs@22
      buildCommands: [npm ci, npm run bundle]
      deployFiles: ./
`

func newWorld(t *testing.T) *world {
	t.Helper()
	g := giteatest.New(t)
	g.AddRepo("acme/api", "main")
	g.AddFile("acme/api", "3f9c", "zerops.yaml", apiYaml)
	g.SetArchive("acme/api", "3f9c", []byte("the api archive at 3f9c"))
	g.SetBranch("acme/api", "main", "3f9c")

	z := zeropstest.New(t, "org-1")
	z.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: "org-1"})
	z.SetServices("prj-stage", zerops.Service{ID: "svc-stage-api", ProjectID: "prj-stage", Name: "api",
		Status: "ACTIVE", Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}})
	z.SetServices("prj-prod", zerops.Service{ID: "svc-prod-api", ProjectID: "prj-prod", Name: "api",
		Status: "ACTIVE", Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}})

	records := deploy.NewRecords(0)
	return &world{
		gitea: g, zerops: z, records: records,
		executor: &deploy.Executor{
			Zerops: z.Client("broker"), Gitea: g.Client(), ClientID: "org-1",
			Records: records, PollInterval: time.Millisecond, Timeout: 2 * time.Second,
		},
	}
}

func stageJob(records ...string) deploy.Job {
	return deploy.Job{
		Slug: "acme",
		Environment: environments.Environment{
			Name: "stage", Tier: environments.TierStage, Project: "prj-stage", Sources: []string{"main"},
		},
		Targets: []deploy.Target{{
			Service: "api", Owner: "acme", Repo: "api", Sha: "3f9c",
			VersionName: "3f9c", Setup: "api",
		}},
		Records: records,
	}
}

func productionJob(setup string, records ...string) deploy.Job {
	return deploy.Job{
		Slug: "acme",
		Environment: environments.Environment{
			Name: "production", Tier: environments.TierProduction, Project: "prj-prod", Release: true,
		},
		Targets: []deploy.Target{{
			Service: "api", Owner: "acme", Repo: "api", Sha: "3f9c",
			VersionName: "3f9c v1.0.0 u-abc", Setup: setup,
			PromoteFrom: &deploy.PromoteSource{Project: "prj-stage", Setup: "api"},
		}},
		Records: records,
	}
}

func statusOf(t *testing.T, g *giteatest.Fake, sha, context string) gitea.CommitStatus {
	t.Helper()
	var last gitea.CommitStatus
	for _, s := range g.Statuses("acme/api", sha) {
		if s.Context == context {
			last = s
		}
	}
	return last
}

func TestDeployAStageFromTheCommitArchive(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	record := w.records.New("stage", "api", "acme/api", "3f9c")

	w.executor.Run(context.Background(), stageJob(record.ID))

	versions := w.zerops.AppVersions("svc-stage-api")
	if len(versions) != 1 {
		t.Fatalf("the platform holds %d app versions, want one", len(versions))
	}
	if versions[0].Name != "3f9c" {
		t.Fatalf("the version is named %q, want the bare sha", versions[0].Name)
	}
	if string(versions[0].Archive) != "the api archive at 3f9c" {
		t.Fatalf("the uploaded bytes are %q, want Gitea's archive", versions[0].Archive)
	}
	if versions[0].Setup != "api" {
		t.Fatalf("build-and-deploy named the setup %q", versions[0].Setup)
	}

	back, _ := w.records.Get(record.ID)
	if back.Status != deploy.StatusActive || back.VersionID == "" {
		t.Fatalf("the record ended %+v, want active with a version id", back)
	}

	status := statusOf(t, w.gitea, "3f9c", deploy.StatusContext("stage", "api"))
	if status.State != "success" || status.Description != versions[0].ID {
		t.Fatalf("the commit status is %+v, want success with the app version id", status)
	}
	// Public access is turned on after the first deploy of an HTTP service.
	if !w.zerops.SubdomainEnabled("svc-stage-api") {
		t.Fatal("the stage's subdomain is still off after its first deploy")
	}
}

func TestDeployPromotesTheStageArtifactToProduction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()

	w.executor.Run(ctx, stageJob())
	stageVersion := w.zerops.AppVersions("svc-stage-api")[0]
	// Gitea's archive of the same commit is replaced with different bytes, so
	// the two paths can be told apart by what production ends up holding.
	w.gitea.SetArchive("acme/api", "3f9c", []byte("a fresh archive of 3f9c"))

	// The production tier's setup runs differently but builds the same, so the
	// artifact stage built is what production gets.
	w.executor.Run(ctx, productionJob("api-prod"))

	prod := w.zerops.AppVersions("svc-prod-api")
	if len(prod) != 1 {
		t.Fatalf("production holds %d versions, want one", len(prod))
	}
	if string(prod[0].Archive) != string(stageVersion.Archive) {
		t.Fatalf("production got %q, want the stage artifact %q", prod[0].Archive, stageVersion.Archive)
	}
	if prod[0].Name != "3f9c v1.0.0 u-abc" {
		t.Fatalf("production's version is named %q, want the sha, the tag and the tagger", prod[0].Name)
	}
	if prod[0].Setup != "api-prod" {
		t.Fatalf("production built with the setup %q", prod[0].Setup)
	}
}

func TestDeployRebuildsWhenTheBuildSectionsDiffer(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()

	w.executor.Run(ctx, stageJob())
	stageVersion := w.zerops.AppVersions("svc-stage-api")[0]
	w.gitea.SetArchive("acme/api", "3f9c", []byte("a fresh archive of 3f9c"))

	w.executor.Run(ctx, productionJob("api-different"))

	prod := w.zerops.AppVersions("svc-prod-api")
	if len(prod) != 1 {
		t.Fatalf("production holds %d versions, want one", len(prod))
	}
	if string(prod[0].Archive) == string(stageVersion.Archive) {
		t.Fatal("a setup that builds differently still promoted the stage artifact")
	}
	if string(prod[0].Archive) != "a fresh archive of 3f9c" {
		t.Fatalf("production got %q, want Gitea's archive", prod[0].Archive)
	}
}

func TestDeployRebuildsWhenNoStageVersionExists(t *testing.T) {
	t.Parallel()
	w := newWorld(t)

	// Nothing ever reached stage: production takes the commit archive.
	w.executor.Run(context.Background(), productionJob("api-prod"))

	prod := w.zerops.AppVersions("svc-prod-api")
	if len(prod) != 1 || string(prod[0].Archive) != "the api archive at 3f9c" {
		t.Fatalf("production got %d versions, archive %q", len(prod), prod[0].Archive)
	}
}

func TestDeployOfACommitAlreadyLiveDoesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()

	w.executor.Run(ctx, stageJob())
	first := w.zerops.AppVersions("svc-stage-api")

	record := w.records.New("stage", "api", "acme/api", "3f9c")
	w.executor.Run(ctx, stageJob(record.ID))

	if got := w.zerops.AppVersions("svc-stage-api"); len(got) != len(first) {
		t.Fatalf("a second deploy of the live commit made %d versions, want %d", len(got), len(first))
	}
	back, _ := w.records.Get(record.ID)
	if back.Status != deploy.StatusActive || back.Message != "already live" {
		t.Fatalf("the record ended %+v", back)
	}
}

func TestAFailedBuildIsRecordedAsFailure(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.zerops.FailBuild("3f9c")
	record := w.records.New("stage", "api", "acme/api", "3f9c")

	w.executor.Run(context.Background(), stageJob(record.ID))

	back, _ := w.records.Get(record.ID)
	if back.Status != deploy.StatusFailed || back.Message == "" {
		t.Fatalf("the record ended %+v, want failed with a reason", back)
	}
	if status := statusOf(t, w.gitea, "3f9c", deploy.StatusContext("stage", "api")); status.State != "failure" {
		t.Fatalf("the commit status is %+v, want failure", status)
	}
	if w.zerops.SubdomainEnabled("svc-stage-api") {
		t.Fatal("a failed deploy still turned public access on")
	}
}

func TestDeployRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*world, *deploy.Job)
		want   string
	}{
		{
			name: "a service the environment's project does not carry",
			mutate: func(_ *world, j *deploy.Job) {
				j.Targets[0].Service = "ghost"
			},
			want: "has no service ghost",
		},
		{
			name: "a commit with no zerops.yaml",
			mutate: func(w *world, j *deploy.Job) {
				j.Targets[0].Sha = "c0ffee"
				w.gitea.SetArchive("acme/api", "c0ffee", []byte("x"))
			},
			want: "carries no zerops.yaml",
		},
		{
			name: "a setup the tier names and the repository does not carry",
			mutate: func(_ *world, j *deploy.Job) {
				j.Targets[0].Setup = "ghost"
			},
			want: `has no setup "ghost"`,
		},
		{
			name: "a commit Gitea has no archive for",
			mutate: func(w *world, j *deploy.Job) {
				j.Targets[0].Sha = "deadbee"
				w.gitea.AddFile("acme/api", "deadbee", "zerops.yaml", apiYaml)
			},
			want: "archive could not be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			record := w.records.New("stage", "api", "acme/api", "3f9c")
			j := stageJob(record.ID)
			tc.mutate(w, &j)

			w.executor.Run(context.Background(), j)

			back, _ := w.records.Get(record.ID)
			if back.Status != deploy.StatusFailed {
				t.Fatalf("the record ended %+v, want failed", back)
			}
			if !strings.Contains(back.Message, tc.want) {
				t.Fatalf("the reason is %q, want it to mention %q", back.Message, tc.want)
			}
			if len(w.zerops.AppVersions("svc-stage-api")) != 0 {
				t.Fatal("a refused deploy still made an app version")
			}
		})
	}
}

func TestTheGateRefusesACommitTheStageIsNotRunning(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	record := w.records.New("production", "api", "acme/api", "3f9c")

	j := productionJob("api-prod", record.ID)
	j.Targets[0].Gate = &deploy.Gate{Environment: "stage", Project: "prj-stage"}

	// Nothing is live on stage, so production deploys nothing.
	w.executor.Run(context.Background(), j)

	back, _ := w.records.Get(record.ID)
	if back.Status != deploy.StatusFailed || !strings.Contains(back.Message, "not live on stage") {
		t.Fatalf("the record ended %+v, want a refusal naming the gate", back)
	}
	if len(w.zerops.AppVersions("svc-prod-api")) != 0 {
		t.Fatal("a gate that is not met still deployed")
	}
}

func TestTheGateOpensOnceTheStageRunsTheCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.executor.Run(ctx, stageJob())

	j := productionJob("api-prod")
	j.Targets[0].Gate = &deploy.Gate{Environment: "stage", Project: "prj-stage"}
	w.executor.Run(ctx, j)

	if len(w.zerops.AppVersions("svc-prod-api")) != 1 {
		t.Fatal("the gate was met and production still did not deploy")
	}
}

func TestAGateThatAnswersNothingIsNotAnOpenOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	record := w.records.New("production", "api", "acme/api", "3f9c")

	j := productionJob("api-prod", record.ID)
	// A stage project that carries no such service: a gate the broker cannot
	// read is never read as an open one.
	j.Targets[0].Gate = &deploy.Gate{Environment: "stage", Project: "prj-unreachable"}
	w.executor.Run(context.Background(), j)

	back, _ := w.records.Get(record.ID)
	if back.Status != deploy.StatusFailed {
		t.Fatalf("the record ended %+v, want failed", back)
	}
	if len(w.zerops.AppVersions("svc-prod-api")) != 0 {
		t.Fatal("a gate the broker could not read still let a deploy through")
	}
}
