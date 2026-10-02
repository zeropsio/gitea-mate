package pipeline_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The routes a sweep of the group's runner registrations takes.
const (
	listRunners  = "GET /orgs/acme/actions/runners"
	deleteRunner = "DELETE /orgs/acme/actions/runners/"
	readToken    = "POST /orgs/acme/actions/runners/registration-token"
)

// taintedWorld is a granting world whose runner ran a branch's own workflow,
// with the org's runner registrations seeded: the tainted container's own, and
// one somebody registered with the token a job read there. Another group's
// runner is in another org.
func taintedWorld(t *testing.T) (*world, *lockedBuffer) {
	t.Helper()
	w := granting(t)
	now := runnerMade.Add(2 * time.Hour)
	logs := &lockedBuffer{}
	configureRunners(w.pipe, &now, logs)
	w.gitea.AddRun("acme", gitea.Run{ID: 9, Event: "push", HeadBranch: "mate/mate-p1",
		StartedAt: runnerMade.Add(30 * time.Minute), Repository: apiRepo, HeadRepository: apiRepo})
	w.gitea.AddRunner("acme", "runneracme-1")
	w.gitea.AddRunner("acme", "runneracme-1")
	w.gitea.AddRunner("beta", "runnerbeta-1")
	return w, logs
}

// taint asks for a key on the tainted runner and waits for its replacement.
func taint(t *testing.T, w *world) {
	t.Helper()
	_, err := w.pipe.Grant(context.Background(), pushJob(second))
	if refusal := refusalOf(t, err); refusal.Code != "runner_tainted" {
		t.Fatalf("refused with %+v, want runner_tainted", refusal)
	}
	w.pipe.WaitRunnerWork()
}

func callsTo(w *world, prefix string) int {
	n := 0
	for _, call := range w.gitea.Calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// TestATaintedRunnersRegistrationsAreSweptBeforeItsReplacement — a job that
// ran as root on the runner could copy the runner's own credential and the
// org's registration token, and Gitea 1.27.2 answers the org's latest token
// rather than a new one, so the replacement registers with the same token.
// Before it is imported, every registration of the org is deleted: the dead
// container's, whose copied credential could still fetch jobs, and any made
// with the token since. Another org's runners are not the group's to touch.
func TestATaintedRunnersRegistrationsAreSweptBeforeItsReplacement(t *testing.T) {
	t.Parallel()
	w, logs := taintedWorld(t)
	taint(t, w)

	if left := w.gitea.Runners("acme"); len(left) != 0 {
		t.Fatalf("the org still holds %+v, want every registration deleted", left)
	}
	if left := w.gitea.Runners("beta"); len(left) != 1 {
		t.Fatalf("another org holds %+v, want its runner untouched", left)
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("imported %d runners, want the replacement", len(w.zerops.Imports))
	}
	lastDelete, tokenRead := -1, -1
	for i, call := range w.gitea.Calls {
		switch {
		case strings.HasPrefix(call, deleteRunner):
			lastDelete = i
		case call == readToken && tokenRead < 0:
			tokenRead = i
		}
	}
	if lastDelete < 0 || tokenRead < lastDelete {
		t.Fatalf("the token was read at call %d and the last registration deleted at %d, want the sweep first:\n%s",
			tokenRead, lastDelete, strings.Join(w.gitea.Calls, "\n"))
	}
	if strings.Contains(logs.String(), registrationToken) {
		t.Fatalf("the token leaked into the log:\n%s", logs)
	}
}

// TestASweepThatFailsHoldsTheImport — a replacement that could not delete the
// org's registrations imports nothing: the deploy job waits, and the pass that
// imports a runner for it sweeps first.
func TestASweepThatFailsHoldsTheImport(t *testing.T) {
	t.Parallel()
	w, _ := taintedWorld(t)
	w.gitea.Fail[listRunners] = http.StatusServiceUnavailable
	taint(t, w)

	if len(w.zerops.DeletedServices) != 1 || len(w.zerops.Imports) != 0 {
		t.Fatalf("deleted %v and imported %d, want the tainted runner deleted and none imported",
			w.zerops.DeletedServices, len(w.zerops.Imports))
	}
	if left := w.gitea.Runners("acme"); len(left) != 2 {
		t.Fatalf("the org holds %+v after a failed list, want both", left)
	}

	delete(w.gitea.Fail, listRunners)
	w.gitea.AddRun("acme", gitea.Run{ID: 11, Event: "workflow_dispatch", HeadBranch: "main", Status: gitea.RunQueued})
	if _, err := w.pipe.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	w.pipe.WaitRunnerWork()
	if left := w.gitea.Runners("acme"); len(left) != 0 {
		t.Fatalf("the org still holds %+v, want the pass to sweep", left)
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("the pass imported %d runners, want one once the sweep is done", len(w.zerops.Imports))
	}
}

// TestASweepIsBounded — one attempt deletes at most four pages of
// registrations; an org holding more is not imported into until a later
// attempt has deleted the rest.
func TestASweepIsBounded(t *testing.T) {
	t.Parallel()
	w, logs := taintedWorld(t)
	for range 4*50 - 1 { // with the two seeded: one past the bound
		w.gitea.AddRunner("acme", "runneracme-x")
	}
	taint(t, w)

	if n := callsTo(w, deleteRunner); n != 200 {
		t.Fatalf("the first attempt deleted %d registrations, want 200", n)
	}
	if len(w.zerops.Imports) != 0 {
		t.Fatalf("imported %d runners with registrations left, want none", len(w.zerops.Imports))
	}
	if !strings.Contains(logs.String(), "remain") {
		t.Fatalf("the log does not say registrations remain:\n%s", logs)
	}

	w.gitea.AddRun("acme", gitea.Run{ID: 11, Event: "workflow_dispatch", HeadBranch: "main", Status: gitea.RunQueued})
	if _, err := w.pipe.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	w.pipe.WaitRunnerWork()
	if left := w.gitea.Runners("acme"); len(left) != 0 || len(w.zerops.Imports) != 1 {
		t.Fatalf("after the second attempt the org holds %d and %d runners were imported, want 0 and 1",
			len(left), len(w.zerops.Imports))
	}
}

// TestAHealthyGroupNeverListsItsRunners — the sweep is a taint's alone: a
// grant on a trusted runner, a first import, a broken runner's replacement and
// a deploy pass read and delete no registration, and leave the org's alone.
func TestAHealthyGroupNeverListsItsRunners(t *testing.T) {
	t.Parallel()
	hostname := registry.RunnerHostname("acme")
	runner := func(status string, created time.Time) zerops.Service {
		return zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: hostname, Status: status, Created: created}
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) *world
	}{
		{"a grant on a trusted runner", func(t *testing.T) *world {
			w := granting(t)
			if grant, err := w.pipe.Grant(context.Background(), pushJob(second)); err != nil || grant.Status != deploy.GrantGranted {
				t.Fatalf("Grant = %+v, %v", grant, err)
			}
			return w
		}},
		{"a first import", func(t *testing.T) *world {
			w, _, _ := brokenWorld(t)
			if err := w.pipe.EnsureRunner(context.Background(), "acme", deploy.QueuedJob{}); err != nil {
				t.Fatalf("EnsureRunner: %v", err)
			}
			return w
		}},
		{"a broken runner's replacement", func(t *testing.T) *world {
			w, _, _ := brokenWorld(t)
			w.zerops.SetServices(giteaPrj, runner("READY_TO_DEPLOY", runnerClock.Add(-time.Hour)))
			if err := w.pipe.EnsureRunner(context.Background(), "acme", deploy.QueuedJob{}); err != nil {
				t.Fatalf("EnsureRunner: %v", err)
			}
			return w
		}},
		{"a deploy pass", func(t *testing.T) *world {
			w, _, _ := brokenWorld(t)
			w.zerops.SetServices(giteaPrj, runner("ACTIVE", runnerClock.Add(-time.Hour)))
			if _, err := w.pipe.Pass(context.Background()); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			return w
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := tc.run(t)
			w.queue.Wait()
			w.pipe.WaitRunnerWork()
			if n := callsTo(w, listRunners) + callsTo(w, deleteRunner); n != 0 {
				t.Fatalf("a healthy group made %d calls to its runner registrations, want none", n)
			}
		})
	}
}
