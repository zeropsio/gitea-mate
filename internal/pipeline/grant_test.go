package pipeline_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// runnerMade is when the group's runner service was created in these tests.
var runnerMade = time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

var apiRepo = gitea.Repo{FullName: "acme/api", DefaultBranch: "main"}

// granting is the world with what a grant reads on top: the group's runner,
// the broker's own service holding the stage's and production's keys, and one
// run of the default branch's workflow (run 7, job 41).
func granting(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.gitea.SetBranch("acme/api", "main", second)
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-broker", ProjectID: giteaPrj, Name: "broker"},
		zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: "runneracme", Created: runnerMade},
	)
	w.zerops.SetUserData("svc-broker",
		zerops.ServiceUserData{Key: deploy.TokenVariable(stagePrj), Content: "the-stage-key", Sensitive: true},
		zerops.ServiceUserData{Key: deploy.TokenVariable(prodPrj), Content: "the-production-key", Sensitive: true},
	)
	w.gitea.AddRun("acme", gitea.Run{ID: 7, Event: "push", HeadBranch: "main", HeadSHA: second,
		StartedAt: runnerMade.Add(time.Hour), Repository: apiRepo, HeadRepository: apiRepo})
	return w
}

func pushJob(sha string) deploy.GrantRequest {
	return deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 7, TaskID: "41", Sha: sha}
}

func refusalOf(t *testing.T, err error) *deploy.Refusal {
	t.Helper()
	var refusal *deploy.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return refusal
}

// TestAJobOfTheDefaultBranchIsHandedTheKey is D27's happy path: a job started
// by a push names no environment and is granted what its branch feeds — the
// environment's own key, where to push and under which name.
func TestAJobOfTheDefaultBranchIsHandedTheKey(t *testing.T) {
	t.Parallel()
	w := granting(t)

	grant, err := w.pipe.Grant(context.Background(), pushJob(second))
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	want := deploy.Grant{
		ID: grant.ID, Status: deploy.GrantGranted, Environment: "stage", Service: "api", Sha: second,
		Token: "the-stage-key", ProjectID: stagePrj, ServiceID: "svc-stage-api", Setup: "api", VersionName: second,
	}
	if grant != want || !strings.HasPrefix(grant.ID, "d_") {
		t.Fatalf("Grant = %+v, want %+v", grant, want)
	}
	var last gitea.CommitStatus
	for _, s := range w.gitea.Statuses("acme/api", second) {
		last = s
	}
	if last.Context != "mate/deploy/stage/api" || last.State != "pending" || last.Description != "deploying · job 41" {
		t.Fatalf("the commit's status is %+v", last)
	}
	if strings.Contains(last.Description, "the-stage-key") {
		t.Fatal("the key reached a commit status")
	}
}

// TestWhoGetsNoKey — the refusals, in the order the contract lists them. None
// of them is a job's fault to retry; each says what is wrong.
func TestWhoGetsNoKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		arm    func(*world)
		req    deploy.GrantRequest
		status int
		code   string
	}{
		{
			name: "a Mate's branch running a workflow of its own",
			arm: func(w *world) {
				w.gitea.AddRun("acme", gitea.Run{ID: 8, Event: "push", HeadBranch: "mate/mate-p1",
					Repository: apiRepo, HeadRepository: apiRepo})
			},
			req:    deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 8, TaskID: "42", Sha: second},
			status: http.StatusForbidden, code: "untrusted_ref",
		},
		{
			name: "a pull request's run",
			arm: func(w *world) {
				w.gitea.AddRun("acme", gitea.Run{ID: 8, Event: "pull_request", HeadBranch: "main",
					Repository: apiRepo, HeadRepository: apiRepo})
			},
			req:    deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 8, TaskID: "42", Sha: second},
			status: http.StatusForbidden, code: "untrusted_ref",
		},
		{
			name:   "a job that does not say which commit it holds",
			req:    deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 7, TaskID: "41"},
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{
			name: "an environment the group does not have",
			req: deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 7, TaskID: "41", Sha: second,
				Environment: "preview"},
			status: http.StatusNotFound, code: "unknown_environment",
		},
		{
			name: "a service this repository does not build",
			req: deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 7, TaskID: "41", Sha: second,
				Environment: "stage", Service: "worker"},
			status: http.StatusNotFound, code: "unknown_service",
		},
		{
			name: "an environment nobody minted a key for",
			arm: func(w *world) {
				w.zerops.SetUserData("svc-broker")
			},
			req:    pushJob(second),
			status: http.StatusFailedDependency, code: "no_deploy_token",
		},
		{
			name: "a group whose runner cannot be found",
			arm: func(w *world) {
				w.zerops.SetServices(giteaPrj, zerops.Service{ID: "svc-broker", ProjectID: giteaPrj, Name: "broker"})
			},
			req:    pushJob(second),
			status: http.StatusServiceUnavailable, code: "runner_unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := granting(t)
			if tc.arm != nil {
				tc.arm(w)
			}
			grant, err := w.pipe.Grant(context.Background(), tc.req)
			refusal := refusalOf(t, err)
			if refusal.Status != tc.status || refusal.Code != tc.code {
				t.Fatalf("refused %d %s (%s), want %d %s", refusal.Status, refusal.Code, refusal.Message, tc.status, tc.code)
			}
			if grant.Token != "" {
				t.Fatal("a refusal carried a key")
			}
		})
	}
}

// TestNothingToDoIsNotAFailure — a job that has nothing to deploy ends green
// and is told why; none of these answers carries a key.
func TestNothingToDoIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		arm  func(*world)
		sha  string
		want string
	}{
		{name: "an older commit than the branch's head", sha: first, want: deploy.GrantSuperseded},
		{
			name: "a commit already live",
			arm:  func(w *world) { w.land("svc-stage-api", second) },
			sha:  second, want: deploy.GrantLive,
		},
		{
			name: "a repository whose branch feeds no environment",
			arm: func(w *world) {
				w.gitea.AddFile("acme/group", "main", "environments.yaml",
					"version: 1\nenvironments:\n  stage:\n    tier: stage\n    project: prj-stage\n    sources: [develop]\n")
			},
			sha: second, want: deploy.GrantNothing,
		},
		{
			// The push that lands a group's first code, before anybody added a
			// stage: it failed every such workflow red until D27 (primer, open 21).
			name: "a group that has declared no environment yet",
			arm:  func(w *world) { w.gitea.RemoveFile("acme/group", "main", "environments.yaml") },
			sha:  second, want: deploy.GrantNothing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := granting(t)
			if tc.arm != nil {
				tc.arm(w)
			}
			grant, err := w.pipe.Grant(context.Background(), pushJob(tc.sha))
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if grant.Status != tc.want || grant.Token != "" || grant.Message == "" {
				t.Fatalf("Grant = %+v, want %s with a reason and no key", grant, tc.want)
			}
		})
	}
}

// TestOneJobPerCommit — two jobs for one commit (a push's own and one the
// catch-up pass started): the second is told the first has it, until the first
// has been silent for longer than the patience.
func TestOneJobPerCommit(t *testing.T) {
	t.Parallel()
	w := granting(t)
	ahead := time.Duration(0)
	w.pipe.Now = func() time.Time { return time.Now().Add(ahead) }

	if grant, err := w.pipe.Grant(context.Background(), pushJob(second)); err != nil || grant.Status != deploy.GrantGranted {
		t.Fatalf("the first job: %+v, %v", grant, err)
	}
	grant, err := w.pipe.Grant(context.Background(), pushJob(second))
	if err != nil || grant.Status != deploy.GrantInProgress || grant.Token != "" {
		t.Fatalf("the second job: %+v, %v — want in_progress and no key", grant, err)
	}

	ahead = deploy.DefaultPatience + time.Minute
	grant, err = w.pipe.Grant(context.Background(), pushJob(second))
	if err != nil || grant.Status != deploy.GrantGranted {
		t.Fatalf("a job after the patience: %+v, %v — want the key", grant, err)
	}
}

// TestARunnerThatRanABranchsWorkflowGetsNoKey — jobs share one container and
// are root in it. A run of anything but a default branch's own workflow since
// the runner was made taints it: the key stays where it is, the runner is
// thrown away, and a run from before the runner existed does not count.
func TestARunnerThatRanABranchsWorkflowGetsNoKey(t *testing.T) {
	t.Parallel()
	branchRun := gitea.Run{ID: 9, Event: "push", HeadBranch: "mate/mate-p1", Repository: apiRepo, HeadRepository: apiRepo}

	t.Run("since the runner was made", func(t *testing.T) {
		t.Parallel()
		w := granting(t)
		tainted := branchRun
		tainted.StartedAt = runnerMade.Add(30 * time.Minute)
		w.gitea.AddRun("acme", tainted)

		grant, err := w.pipe.Grant(context.Background(), pushJob(second))
		refusal := refusalOf(t, err)
		if refusal.Code != "runner_tainted" || refusal.Status != http.StatusServiceUnavailable || grant.Token != "" {
			t.Fatalf("refused %+v with %+v, want runner_tainted and no key", refusal, grant)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(w.zerops.DeletedServices) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if len(w.zerops.DeletedServices) != 1 || w.zerops.DeletedServices[0] != "svc-runner" {
			t.Fatalf("deleted %v, want the tainted runner", w.zerops.DeletedServices)
		}
	})

	t.Run("before the runner was made", func(t *testing.T) {
		t.Parallel()
		w := granting(t)
		old := branchRun
		old.StartedAt = runnerMade.Add(-time.Hour)
		w.gitea.AddRun("acme", old)
		// Newest first, as Gitea lists them: the default branch's run on top.
		w.gitea.AddRun("acme", gitea.Run{ID: 10, Event: "push", HeadBranch: "main", HeadSHA: second,
			StartedAt: runnerMade.Add(2 * time.Hour), Repository: apiRepo, HeadRepository: apiRepo})

		grant, err := w.pipe.Grant(context.Background(), pushJob(second))
		if err != nil || grant.Status != deploy.GrantGranted {
			t.Fatalf("Grant = %+v, %v — a run older than the runner must not taint it", grant, err)
		}
	})

	t.Run("queued and not started", func(t *testing.T) {
		t.Parallel()
		w := granting(t)
		w.gitea.AddRun("acme", branchRun)
		grant, err := w.pipe.Grant(context.Background(), pushJob(second))
		if err != nil || grant.Status != deploy.GrantGranted {
			t.Fatalf("Grant = %+v, %v — a run that has not started has touched nothing", grant, err)
		}
	})
}

// TestProductionIsGrantedWhatTheReleaseLists — a dispatched job names the
// environment; the commit must be the one the newest approved tag lists, the
// stage must already run it, and the version carries the tag and the tagger.
func TestProductionIsGrantedWhatTheReleaseLists(t *testing.T) {
	t.Parallel()
	w := granting(t)
	ctx := context.Background()
	w.tag(t, "v1.0.0", "commit-1", "api "+second, time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC))
	if err := w.pipe.Create(ctx, "acme", tagDelivery("v1.0.0", "commit-1", roles.Login(ownerUser))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.queue.Wait()
	w.gitea.AddRun("acme", gitea.Run{ID: 11, Event: "workflow_dispatch", HeadBranch: "main",
		StartedAt: runnerMade.Add(3 * time.Hour), Repository: apiRepo, HeadRepository: apiRepo})
	req := deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 11, TaskID: "50", Sha: second,
		Environment: "production", Service: "api"}

	grant, err := w.pipe.Grant(ctx, req)
	if err != nil || grant.Status != deploy.GrantGranted {
		t.Fatalf("Grant = %+v, %v", grant, err)
	}
	if grant.Token != "the-production-key" || grant.ServiceID != "svc-prod-api" ||
		grant.VersionName != second+" v1.0.0 u-v1.0.0" {
		t.Fatalf("Grant = %+v", grant)
	}

	other := req
	other.Sha = first
	if grant, err := w.pipe.Grant(ctx, other); err != nil || grant.Status != deploy.GrantSuperseded || grant.Token != "" {
		t.Fatalf("a commit the release does not list: %+v, %v", grant, err)
	}
}

// TestTheJobsReport — the status says what is true: a reported success is
// checked against what the service runs.
func TestTheJobsReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		land      bool
		outcome   string
		message   string
		wantState string
		wantPart  string
		subdomain bool
	}{
		{name: "success, and the service runs the commit", land: true, outcome: "success", wantState: "success", wantPart: "live", subdomain: true},
		{name: "success, but the service runs something else", outcome: "success", wantState: "failure", wantPart: "reported success"},
		{name: "failure, in the job's words", outcome: "failure", message: "the build failed: npm ci", wantState: "failure", wantPart: "npm ci"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := granting(t)
			ctx := context.Background()
			grant, err := w.pipe.Grant(ctx, pushJob(second))
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if tc.land {
				w.land("svc-stage-api", second)
			}
			if err := w.pipe.Result(ctx, grant.ID, "acme/api", tc.outcome, tc.message); err != nil {
				t.Fatalf("Result: %v", err)
			}
			var last gitea.CommitStatus
			for _, s := range w.gitea.Statuses("acme/api", second) {
				last = s
			}
			if last.State != tc.wantState || !strings.Contains(last.Description, tc.wantPart) {
				t.Fatalf("the commit's status is %+v, want %s mentioning %q", last, tc.wantState, tc.wantPart)
			}
			if got := w.zerops.SubdomainEnabled("svc-stage-api"); got != tc.subdomain {
				t.Fatalf("public access on = %v, want %v", got, tc.subdomain)
			}
		})
	}

	t.Run("refused for another repository's job, and for a grant nobody holds", func(t *testing.T) {
		t.Parallel()
		w := granting(t)
		ctx := context.Background()
		grant, err := w.pipe.Grant(ctx, pushJob(second))
		if err != nil {
			t.Fatalf("Grant: %v", err)
		}
		if refusal := refusalOf(t, w.pipe.Result(ctx, grant.ID, "acme/web", "success", "")); refusal.Code != "wrong_repository" {
			t.Fatalf("refused %+v", refusal)
		}
		if refusal := refusalOf(t, w.pipe.Result(ctx, "d_gone", "acme/api", "success", "")); refusal.Code != "unknown_deploy" {
			t.Fatalf("refused %+v", refusal)
		}
	})
}

// TestAGrantTakesATiersNameForItsOnlyEnvironment — the workflow zcp writes
// names the tier (`stage`) while the app names an environment after its group
// (`acme-stage`): on the owner's run of 2026-09-17 every job's request answered
// 404 for that. A tier's name is its only environment; two of a tier need
// naming, and the refusal lists them.
func TestAGrantTakesATiersNameForItsOnlyEnvironment(t *testing.T) {
	t.Parallel()
	named := func(extra string) string {
		return "version: 1\nenvironments:\n  acme-stage:\n    tier: stage\n    project: prj-stage\n    sources: [main]\n" + extra
	}
	req := pushJob(second)
	req.Environment = "stage"

	w := granting(t)
	w.gitea.AddFile("acme/group", "main", "environments.yaml", named(""))
	w.zerops.SetUserData("svc-broker",
		zerops.ServiceUserData{Key: deploy.TokenVariable(stagePrj), Content: "the-stage-key", Sensitive: true})
	grant, err := w.pipe.Grant(context.Background(), req)
	if err != nil || grant.Status != deploy.GrantGranted || grant.Environment != "acme-stage" {
		t.Fatalf("Grant = %+v, %v — want the tier's only environment", grant, err)
	}

	w = granting(t)
	w.gitea.AddFile("acme/group", "main", "environments.yaml",
		named("  acme-preview:\n    tier: stage\n    project: prj-prod\n    sources: [main]\n"))
	_, err = w.pipe.Grant(context.Background(), req)
	refusal := refusalOf(t, err)
	if refusal.Code != "unknown_environment" || !strings.Contains(refusal.Message, "acme-stage") ||
		!strings.Contains(refusal.Message, "acme-preview") {
		t.Fatalf("refused %+v, want both names listed", refusal)
	}
}
