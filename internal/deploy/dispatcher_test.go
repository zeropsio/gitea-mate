package deploy_test

import (
	"context"
	"net/http"
	"slices"
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
// whose repository carries the workflow zcp writes, and a commit to deploy.
type world struct {
	gitea      *giteatest.Fake
	zerops     *zeropstest.Fake
	dispatcher *deploy.Dispatcher
	// ahead is how far the dispatcher's clock runs ahead of the statuses'.
	ahead time.Duration
}

func newWorld(t *testing.T) *world {
	t.Helper()
	g := giteatest.New(t)
	g.AddRepo("acme/api", "main")
	g.AddFile("acme/api", "main", ".gitea/workflows/zerops.yml", "on: [push, workflow_dispatch]\n")
	g.SetBranch("acme/api", "main", "3f9c")

	z := zeropstest.New(t, "org-1")
	z.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: "org-1"})
	z.SetServices("prj-stage", zerops.Service{ID: "svc-stage-api", ProjectID: "prj-stage", Name: "api", Status: "ACTIVE"})
	z.SetServices("prj-prod", zerops.Service{ID: "svc-prod-api", ProjectID: "prj-prod", Name: "api", Status: "ACTIVE"})

	w := &world{gitea: g, zerops: z}
	w.dispatcher = &deploy.Dispatcher{
		Zerops: z.Client("broker"), Gitea: g.Client(), ClientID: "org-1",
		Now: func() time.Time { return time.Now().Add(w.ahead) },
	}
	return w
}

func stageJob() deploy.Job {
	return deploy.Job{
		Slug: "acme",
		Environment: environments.Environment{
			Name: "stage", Tier: environments.TierStage, Project: "prj-stage", Sources: []string{"main"},
		},
		Targets: []deploy.Target{{Service: "api", Owner: "acme", Repo: "api", Sha: "3f9c", VersionName: "3f9c", Setup: "api"}},
	}
}

func productionJob() deploy.Job {
	return deploy.Job{
		Slug: "acme",
		Environment: environments.Environment{
			Name: "production", Tier: environments.TierProduction, Project: "prj-prod", Release: true,
		},
		Targets: []deploy.Target{{
			Service: "api", Owner: "acme", Repo: "api", Sha: "3f9c",
			VersionName: "3f9c v1.0.0 u-abc", Setup: "api-prod",
			Gate: &deploy.Gate{Environment: "stage", Project: "prj-stage"},
		}},
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

const stageDispatch = "acme/api zerops.yml@main environment=stage service=api sha=3f9c"

// TestAnEnvironmentBehindGetsAJob — D27: the broker deploys nothing itself. An
// environment behind its source gets the repository's own workflow started, on
// the default branch, told which environment, service and commit.
func TestAnEnvironmentBehindGetsAJob(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.dispatcher.Run(context.Background(), stageJob())

	if !slices.Equal(w.gitea.Dispatches, []string{stageDispatch}) {
		t.Fatalf("dispatched %v, want [%s]", w.gitea.Dispatches, stageDispatch)
	}
	status := statusOf(t, w.gitea, "3f9c", "mate/deploy/stage/api")
	if status.State != "pending" || status.Description != deploy.DescriptionDispatched {
		t.Fatalf("the commit's status is %+v, want pending %q", status, deploy.DescriptionDispatched)
	}
	if got := w.zerops.AppVersions("svc-stage-api"); len(got) != 0 {
		t.Fatalf("the broker made %d app versions; since D27 it makes none", len(got))
	}
}

func TestACommitAlreadyLiveGetsNoJob(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.zerops.AddAppVersion(zerops.AppVersion{ServiceStackID: "svc-stage-api", Status: zerops.AppVersionActive}, "3f9c")
	w.dispatcher.Run(context.Background(), stageJob())
	if len(w.gitea.Dispatches) != 0 {
		t.Fatalf("a commit already live was dispatched: %v", w.gitea.Dispatches)
	}
}

// TestALiveCommitClosesAStatusAJobLeftPending — a job that died after its push
// landed never reported; the pass that finds the commit live says so, or the
// row would read "deploying" for ever.
func TestALiveCommitClosesAStatusAJobLeftPending(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.dispatcher.Run(context.Background(), stageJob())
	w.zerops.AddAppVersion(zerops.AppVersion{ServiceStackID: "svc-stage-api", Status: zerops.AppVersionActive}, "3f9c")
	w.dispatcher.Run(context.Background(), stageJob())

	status := statusOf(t, w.gitea, "3f9c", "mate/deploy/stage/api")
	if status.State != "success" {
		t.Fatalf("the commit's status is %+v, want success", status)
	}
	if len(w.gitea.Dispatches) != 1 {
		t.Fatalf("dispatched %d times, want once", len(w.gitea.Dispatches))
	}
}

// TestADeployOnItsWayIsLeftAlone — one job per commit while its status is
// pending and young; past the patience the job is taken for dead and started
// again.
func TestADeployOnItsWayIsLeftAlone(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.dispatcher.Run(context.Background(), stageJob())
	w.dispatcher.Run(context.Background(), stageJob())
	if len(w.gitea.Dispatches) != 1 {
		t.Fatalf("a young pending deploy was dispatched again: %v", w.gitea.Dispatches)
	}

	w.ahead = deploy.DefaultPatience + time.Minute
	w.dispatcher.Run(context.Background(), stageJob())
	if len(w.gitea.Dispatches) != 2 {
		t.Fatalf("a deploy pending past the patience was not dispatched again: %v", w.gitea.Dispatches)
	}
}

// TestAWorkflowThatCannotBeStartedSaysWhatToDo — a repository whose default
// branch carries no dispatchable workflow (one from before D27, or none) is
// told apart from a failure of the broker's.
func TestAWorkflowThatCannotBeStartedSaysWhatToDo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.gitea.AddRepo("acme/old", "main")
	job := stageJob()
	job.Targets[0].Repo = "old"

	w.dispatcher.Run(context.Background(), job)

	var last gitea.CommitStatus
	for _, s := range w.gitea.Statuses("acme/old", "3f9c") {
		last = s
	}
	if last.State != "failure" || !strings.Contains(last.Description, "merge the Mate's pull request") {
		t.Fatalf("the status is %+v, want a failure that says what to do", last)
	}
}

func TestTheGateHoldsAProductionJob(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.dispatcher.Run(context.Background(), productionJob())
	if len(w.gitea.Dispatches) != 0 {
		t.Fatalf("a commit the stage does not run was dispatched to production: %v", w.gitea.Dispatches)
	}
	status := statusOf(t, w.gitea, "3f9c", "mate/deploy/production/api")
	if status.State != "failure" || !strings.Contains(status.Description, "is not live on stage") {
		t.Fatalf("the status is %+v, want the gate's refusal", status)
	}

	w.zerops.AddAppVersion(zerops.AppVersion{ServiceStackID: "svc-stage-api", Status: zerops.AppVersionActive}, "3f9c")
	w.dispatcher.Run(context.Background(), productionJob())
	want := "acme/api zerops.yml@main environment=production service=api sha=3f9c"
	if !slices.Equal(w.gitea.Dispatches, []string{want}) {
		t.Fatalf("dispatched %v, want [%s]", w.gitea.Dispatches, want)
	}
}

func TestAServiceTheProjectDoesNotHaveIsReported(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	job := stageJob()
	job.Targets[0].Service = "worker"
	w.dispatcher.Run(context.Background(), job)
	status := statusOf(t, w.gitea, "3f9c", "mate/deploy/stage/worker")
	if status.State != "failure" || !strings.Contains(status.Description, "has no service worker") {
		t.Fatalf("the status is %+v", status)
	}
}

// TestAJobsOwnFailureReportIsFinalAcrossPasses — a job that ran and reported
// a failure is the verdict on that commit for that service: a pass never
// starts it again, however long it waits. Only a person asks again (a new
// release, a re-run of the job).
func TestAJobsOwnFailureReportIsFinalAcrossPasses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.dispatcher.Run(ctx, stageJob())
	deploy.WriteStatus(ctx, w.gitea.Client(), nil, stageJob().Targets[0], "stage", "pending", deploy.DescriptionDeploying+" · job 7")
	deploy.WriteStatus(ctx, w.gitea.Client(), nil, stageJob().Targets[0], "stage", "failure", deploy.DescriptionFailed+": zcli push exited 1")

	w.dispatcher.Run(ctx, stageJob())
	w.ahead = deploy.DefaultPatience + time.Minute
	w.dispatcher.Run(ctx, stageJob())

	if len(w.gitea.Dispatches) != 1 {
		t.Fatalf("a failed deploy was dispatched again: %v", w.gitea.Dispatches)
	}
	if status := statusOf(t, w.gitea, "3f9c", "mate/deploy/stage/api"); status.State != "failure" {
		t.Fatalf("the commit's status is %+v, want the failure left alone", status)
	}
}

// TestATransientRepoReadFailureAfterAStaleDeployingPendingIsDispatchedAgain —
// a job that was handed the key and died leaves "deploying" behind; a refusal
// the broker writes over it (here a repository read that failed) is no
// verdict on the commit, so a pass past the patience dispatches it again.
func TestATransientRepoReadFailureAfterAStaleDeployingPendingIsDispatchedAgain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	deploy.WriteStatus(ctx, w.gitea.Client(), nil, stageJob().Targets[0], "stage", "pending", deploy.DescriptionDeploying+" · job 7")
	w.ahead = deploy.DefaultPatience + time.Minute

	w.gitea.Fail["GET /repos/acme/api"] = http.StatusBadGateway
	w.dispatcher.Run(ctx, stageJob())
	if status := statusOf(t, w.gitea, "3f9c", "mate/deploy/stage/api"); status.State != "failure" {
		t.Fatalf("the commit's status is %+v, want the refusal written", status)
	}
	delete(w.gitea.Fail, "GET /repos/acme/api")
	w.dispatcher.Run(ctx, stageJob())

	if len(w.gitea.Dispatches) != 1 {
		t.Fatalf("the refused deploy was not dispatched again: %v", w.gitea.Dispatches)
	}
}

// TestARefusalNamingAnOwnerThatStartsWithFailedIsDispatchedAgain — a refusal
// the broker writes can open with the repository's owner, a name a person
// chose; only a job's own report ([deploy.DescriptionFailed] and its colon)
// is final, so an owner called "failed-x" leaves the refusal retryable.
func TestARefusalNamingAnOwnerThatStartsWithFailedIsDispatchedAgain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.gitea.AddRepo("failed-x/api", "main")
	job := stageJob()
	job.Targets[0].Owner = "failed-x"

	w.dispatcher.Run(ctx, job)
	var last gitea.CommitStatus
	for _, s := range w.gitea.Statuses("failed-x/api", "3f9c") {
		last = s
	}
	if last.State != "failure" || !strings.HasPrefix(last.Description, "failed-x/api") {
		t.Fatalf("the status is %+v, want the refusal naming failed-x/api", last)
	}
	w.gitea.AddFile("failed-x/api", "main", ".gitea/workflows/zerops.yml", "on: [push, workflow_dispatch]\n")
	w.ahead = deploy.DefaultPatience + time.Minute
	w.dispatcher.Run(ctx, job)

	if len(w.gitea.Dispatches) != 1 {
		t.Fatalf("the refused deploy was not dispatched again: %v", w.gitea.Dispatches)
	}
}
