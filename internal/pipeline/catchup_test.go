package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"
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
		t.Fatalf("the pass deployed %d services, want one", result.Deploys)
	}
	versions := w.zerops.AppVersions("svc-stage-api")
	if len(versions) != 1 {
		t.Fatalf("stage holds %d app versions, want one", len(versions))
	}
	if versions[0].Name != second {
		t.Fatalf("stage deployed %q, want the second sha", versions[0].Name)
	}

	// A second pass finds the environment where it should be and deploys
	// nothing.
	result, err = w.pipe.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	if result.Deploys != 0 {
		t.Fatalf("a settled environment deployed %d services", result.Deploys)
	}
	if len(w.zerops.AppVersions("svc-stage-api")) != 1 {
		t.Fatal("a settled environment was deployed again")
	}
}

func TestAPushDeploysTheEnvironmentsItFeeds(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.gitea.SetBranch("acme/api", "main", second)

	if err := w.pipe.Push(ctx, "acme", pushDelivery("acme/api", "refs/heads/main")); err != nil {
		t.Fatalf("Push: %v", err)
	}
	w.queue.Wait()

	versions := w.zerops.AppVersions("svc-stage-api")
	if len(versions) != 1 || versions[0].Name != second {
		t.Fatalf("stage holds %+v, want one version of the second sha", versions)
	}
	// A push to a source branch never reaches production: production deploys
	// what an approved tag lists, and nothing else.
	if len(w.zerops.AppVersions("svc-prod-api")) != 0 {
		t.Fatal("a push deployed production")
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
			if len(w.zerops.AppVersions("svc-stage-api")) != 0 {
				t.Fatal("that push deployed something")
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
	if len(w.zerops.AppVersions("svc-stage-api")) != 0 {
		t.Fatal("an on-request environment deployed on a push")
	}

	// Asked for by name, it deploys — and so does the catch-up pass, which is
	// about an environment falling behind, not about how it was asked.
	plan, err := w.pipe.Plan(ctx, "acme")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	env, _ := plan.File.Environment("stage")
	if err := w.pipe.Deploy(ctx, plan, env, "api", nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	w.queue.Wait()
	if len(w.zerops.AppVersions("svc-stage-api")) != 1 {
		t.Fatal("an on-request environment did not deploy when it was asked")
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
