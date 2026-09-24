package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// pushDelivery is what Gitea posts when a branch moves.
func pushDelivery(repo, ref string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"ref": ref, "repository": map[string]any{"full_name": repo},
	})
	return raw
}

// TestTheLoopCatchesUp is the proof of guide 1.3: with the broker down, two
// pushes; one pass when it comes back; one deploy, of the second sha.
func TestTheLoopCatchesUp(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()

	// Down: nothing is delivered. `main` moves twice.
	w.gitea.SetBranch("acme/api", "main", first)
	w.gitea.SetBranch("acme/api", "main", second)

	result, err := w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	if result.Deploys != 1 {
		t.Fatalf("the pass found %d services behind, want one", result.Deploys)
	}
	if got := w.dispatched("stage"); len(got) != 1 || got[0] != second {
		t.Fatalf("the pass started jobs for %v, want one for the second sha", got)
	}
	if len(w.zerops.AppVersions("svc-stage-api")) != 0 {
		t.Fatal("the broker deployed something itself; since D27 a job does")
	}

	// The job lands it. A second pass finds the environment where it should
	// be and starts nothing.
	w.land("svc-stage-api", second)
	result, err = w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	if result.Deploys != 0 {
		t.Fatalf("a settled environment was found %d services behind", result.Deploys)
	}
	if len(w.dispatched("stage")) != 1 {
		t.Fatal("a settled environment got another job")
	}
}

// TestAPushToTheDefaultBranchIsTheRepositorysOwnJob — the workflow runs on a
// push to the default branch by itself and deploys what that branch alone
// feeds, so the broker starts nothing for it (D27); a second job for the same
// commit would only be told "in progress".
func TestAPushToTheDefaultBranchIsTheRepositorysOwnJob(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.gitea.SetBranch("acme/api", "main", second)

	if err := w.pipe.Push(ctx, "acme", pushDelivery("acme/api", "refs/heads/main")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	w.queue.Wait()
	if got := w.gitea.Dispatches; len(got) != 0 {
		t.Fatalf("the broker started %v for a push the repository's own workflow runs on", got)
	}
}

// TestAPushToAnotherSourceBranchGetsAJob — the workflow does not run on a
// branch that is not the default one, so a stage that follows such a branch is
// the broker's to start, on the default branch's workflow and at that branch's
// commit.
func TestAPushToAnotherSourceBranchGetsAJob(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.gitea.AddFile("acme/group", "main", "environments.yaml", `
version: 1
environments:
  stage:
    tier: stage
    project: prj-stage
    sources: [develop]
`)
	w.gitea.SetBranch("acme/api", "develop", second)

	if err := w.pipe.Push(ctx, "acme", pushDelivery("acme/api", "refs/heads/develop")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	w.queue.Wait()
	if got := w.dispatched("stage"); len(got) != 1 || got[0] != second {
		t.Fatalf("the broker started jobs for %v, want one for develop's head", got)
	}
	// A push to a source branch never reaches production: production deploys
	// what an approved tag lists, and nothing else.
	if len(w.dispatched("production")) != 0 {
		t.Fatal("a push started a production job")
	}
}

func TestPushesTheBrokerIgnores(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo string
		ref  string
	}{
		{"a branch no environment names", "acme/api", "refs/heads/feature/invoices"},
		{"env/*, which the broker wrote itself", "acme/api", "refs/heads/env/stage"},
		{"a tag, not a branch", "acme/api", "refs/tags/v1.0.0"},
		{"a repository of another org", "other/api", "refs/heads/main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.gitea.SetBranch("acme/api", "main", second)
			org := "acme"
			if tc.repo == "other/api" {
				org = "other"
			}
			if err := w.pipe.Push(context.Background(), org, pushDelivery(tc.repo, tc.ref)); err != nil {
				t.Fatalf("Push: %v", err)
			}
			w.queue.Wait()
			if len(w.gitea.Dispatches) != 0 {
				t.Fatalf("that push started %v", w.gitea.Dispatches)
			}
		})
	}
}

func TestAnOnRequestEnvironmentDeploysOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.gitea.AddFile("acme/group", "main", "environments.yaml", `
version: 1
environments:
  stage:
    tier: stage
    project: prj-stage
    sources: [main]
    deploy: on-request
`)
	w.gitea.SetBranch("acme/api", "main", second)

	if err := w.pipe.Push(ctx, "acme", pushDelivery("acme/api", "refs/heads/main")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	w.queue.Wait()
	if len(w.gitea.Dispatches) != 0 {
		t.Fatal("an on-request environment got a job on a push")
	}

	// Asked for by name, it deploys — and so does the catch-up pass, which is
	// about an environment falling behind, not about how it was asked.
	plan, err := w.pipe.Plan(ctx, "acme")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	env, _ := plan.File.Environment("stage")
	if err := w.pipe.Deploy(ctx, plan, env, "api", false); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	w.queue.Wait()
	if len(w.dispatched("stage")) != 1 {
		t.Fatal("an on-request environment got no job when it was asked")
	}
}

func TestAPassWithNothingRegisteredIsQuiet(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.zerops.SetProjectTags(giteaPrj, []string{"mate:tool:gitea"})

	result, err := w.pipe.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	if result.Groups != 0 || result.Deploys != 0 || len(result.Problems) != 0 {
		t.Fatalf("Pass = %+v", result)
	}
}

// TestTheDeployedShaIsReadFromTheServicesEnvironment pins where the catch-up
// pass learns what is live. The app-version API returns no name at all, so the
// only answer is the appVersionName entry of the service's own environment —
// and a service whose environment says it already runs the head is left alone.
func TestTheDeployedShaIsReadFromTheServicesEnvironment(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.gitea.SetBranch("acme/api", "main", second)

	// The stage is already there, as far as its own environment is concerned.
	w.zerops.AddAppVersion(zerops.AppVersion{
		ID: "ver-seeded", ServiceStackID: "svc-stage-api", Status: zerops.AppVersionActive, Sequence: 1,
	}, second)

	result, err := w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	if result.Deploys != 0 {
		t.Fatalf("the pass found %d services behind, want none", result.Deploys)
	}
	if len(w.gitea.Dispatches) != 0 {
		t.Fatal("a service already at the head got a job")
	}

	// A production-shaped name is read the same way: the sha is the first token.
	w.zerops.SetServices(stagePrj, zerops.Service{ID: "svc-stage-api", ProjectID: stagePrj, Name: "api",
		Status: "ACTIVE", Ports: []zerops.ServicePort{{Port: 3000, HTTPRouting: true}}})
	w.zerops.AddAppVersion(zerops.AppVersion{
		ID: "ver-named", ServiceStackID: "svc-stage-api", Status: zerops.AppVersionActive, Sequence: 2,
	}, second+" v1.0.0 u-abc")

	result, err = w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	if result.Deploys != 0 {
		t.Fatalf("a version named {sha} {tag} {tagger} was not read as its sha: %+v", result)
	}
}

// TestARunningShaTurnsAnEarlierFailureIntoLive — the status says what is true:
// a job that reported a failure (or never reported) while the platform went on
// to run its commit is corrected by the next pass, and nothing is dispatched.
func TestARunningShaTurnsAnEarlierFailureIntoLive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state string
	}{
		{"a failure", "failure"},
		{"a job that never reported", "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ctx := context.Background()
			stage := deploy.Target{Service: "api", Owner: "acme", Repo: "api", Sha: first}
			deploy.WriteStatus(ctx, w.gitea.Client(), nil, stage, "stage", "pending", deploy.DescriptionDispatched)
			if tc.state != "pending" {
				deploy.WriteStatus(ctx, w.gitea.Client(), nil, stage, "stage", tc.state, "zcli push exited 1")
			}
			w.land("svc-stage-api", first)

			if _, err := w.pipe.Pass(ctx); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			w.queue.Wait()
			if got := w.statuses(t, "acme/api", first)["mate/deploy/stage/api"]; got != "success" {
				t.Fatalf("the commit's status is %q, want success", got)
			}
			if got := w.dispatched("stage"); len(got) != 0 {
				t.Fatalf("a commit already running was dispatched: %v", got)
			}
		})
	}
}
