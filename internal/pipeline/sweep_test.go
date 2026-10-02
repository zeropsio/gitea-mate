package pipeline_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// taintedClock is when the tainted runner is found in these tests.
var taintedClock = runnerMade.Add(2 * time.Hour)

// taintedWorld is a granting world whose runner ran a branch's own workflow,
// with the org's runner registrations seeded: the tainted container's own, and
// one somebody registered with the token a job read there. Another group's
// runner is in another org. at moves the clock to taintedClock plus d.
func taintedWorld(t *testing.T) (*world, *lockedBuffer, func(time.Duration)) {
	t.Helper()
	w := granting(t)
	now := taintedClock
	w.zerops.Now = now
	logs := &lockedBuffer{}
	configureRunners(w.pipe, &now, logs)
	w.gitea.AddRun("acme", gitea.Run{ID: 9, Event: "push", HeadBranch: "mate/mate-p1",
		StartedAt: runnerMade.Add(30 * time.Minute), Repository: apiRepo, HeadRepository: apiRepo})
	w.gitea.AddRunner("acme", "runneracme-1")
	w.gitea.AddRunner("acme", "runneracme-1")
	w.gitea.AddRunner("beta", "runnerbeta-1")
	at := func(d time.Duration) {
		now = taintedClock.Add(d)
		w.zerops.Now = now
	}
	return w, logs, at
}

// taint asks for a key on the tainted runner and waits for its replacement
// and the deploys it starts again.
func taint(t *testing.T, w *world) {
	t.Helper()
	_, err := w.pipe.Grant(context.Background(), pushJob(second))
	if refusal := refusalOf(t, err); refusal.Code != "runner_tainted" {
		t.Fatalf("refused with %+v, want runner_tainted", refusal)
	}
	w.pipe.WaitRunnerWork()
	w.queue.Wait()
}

// pass runs one deploy pass with a job of the org waiting for a runner, and
// waits for everything it started.
func pass(t *testing.T, w *world) {
	t.Helper()
	if _, err := w.pipe.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()
	w.pipe.WaitRunnerWork()
}

// waiting queues a job of the org, so a pass owes the group a runner.
func waiting(w *world) {
	w.gitea.AddRun("acme", gitea.Run{ID: 11, Event: "workflow_dispatch", HeadBranch: "main", Status: gitea.RunQueued})
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
	w, logs, _ := taintedWorld(t)
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

// TestTheSweepWaitsForTheTaintedRunnersDeletion — a registration made while
// the tainted container still runs would outlive a sweep that ran first, so
// the platform must have said the deletion finished before the first list.
func TestTheSweepWaitsForTheTaintedRunnersDeletion(t *testing.T) {
	t.Parallel()
	w, _, _ := taintedWorld(t)
	var mu sync.Mutex
	var trace []string
	record := func(side string) func(string) {
		return func(call string) {
			mu.Lock()
			defer mu.Unlock()
			trace = append(trace, side+" "+call)
		}
	}
	w.zerops.Trace = record("zerops")
	w.gitea.Trace = record("gitea")
	taint(t, w)

	mu.Lock()
	defer mu.Unlock()
	deletion := slices.Index(trace, "zerops DELETE /service-stack/svc-runner")
	finished := -1
	for i := deletion + 1; deletion >= 0 && i < len(trace); i++ {
		if strings.HasPrefix(trace[i], "zerops GET /process/") {
			finished = i
			break
		}
	}
	list := slices.Index(trace, "gitea "+listRunners)
	if deletion < 0 || finished < 0 || list < finished {
		t.Fatalf("deleted at %d, finished at %d, first list at %d — want the list after the deletion finished:\n%s",
			deletion, finished, list, strings.Join(trace, "\n"))
	}
}

// TestARestartedBrokerStillSweeps — the sweep is no memory of a taint: every
// import sweeps the group's org first, so a broker restarted between the
// tainted runner's deletion and its replacement still deletes a registration
// somebody made in between.
func TestARestartedBrokerStillSweeps(t *testing.T) {
	t.Parallel()
	w, logs, at := taintedWorld(t)
	importRoute := "POST /project/" + giteaPrj + "/service-stack/import"
	w.zerops.Fail[importRoute] = http.StatusServiceUnavailable
	w.zerops.FailTimes[importRoute] = 1
	taint(t, w)
	if len(w.zerops.DeletedServices) != 1 || len(w.zerops.Imports) != 0 {
		t.Fatalf("deleted %v and imported %d, want the runner deleted and none imported",
			w.zerops.DeletedServices, len(w.zerops.Imports))
	}

	foreign := w.gitea.AddRunner("acme", "runneracme-1")
	at(5 * time.Minute)
	w.pipe = restarted(w, logs)
	waiting(w)
	pass(t, w)

	if left := w.gitea.Runners("acme"); slices.ContainsFunc(left, func(r gitea.Runner) bool { return r.ID == foreign }) {
		t.Fatalf("the org still holds %+v, want the registration made in between deleted", left)
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("the restarted broker imported %d runners, want one", len(w.zerops.Imports))
	}
}

// TestASweepThatFailsHoldsTheImport — a replacement that could not delete the
// org's registrations imports nothing, and says so once. The job waits; the
// sweep is tried again only after a wait that doubles from two minutes, as a
// broken runner's rebuild is, so a pass inside it makes no call at all.
func TestASweepThatFailsHoldsTheImport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fail    func(w *world) string
		deleted string // what the one log line counts
	}{
		{"the list", func(w *world) string { return listRunners }, "deleted=0"},
		{"one deletion", func(w *world) string {
			id := w.gitea.Runners("acme")[1].ID
			return deleteRunner + strconv.FormatInt(id, 10)
		}, "deleted=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, logs, at := taintedWorld(t)
			route := tc.fail(w)
			w.gitea.Fail[route] = http.StatusServiceUnavailable
			taint(t, w)

			if len(w.zerops.DeletedServices) != 1 || len(w.zerops.Imports) != 0 {
				t.Fatalf("deleted %v and imported %d, want the tainted runner deleted and none imported",
					w.zerops.DeletedServices, len(w.zerops.Imports))
			}
			if n := strings.Count(logs.String(), "registrations of a group"); n != 1 {
				t.Fatalf("the failed sweep was logged %d times, want once:\n%s", n, logs)
			}
			if !strings.Contains(logs.String(), tc.deleted+" ") {
				t.Fatalf("the failed sweep did not count %s:\n%s", tc.deleted, logs)
			}

			waiting(w)
			lists := callsTo(w, listRunners)
			at(time.Minute)
			pass(t, w)
			at(90 * time.Second)
			pass(t, w)
			if n := callsTo(w, listRunners); n != lists {
				t.Fatalf("two passes inside the wait listed the registrations %d more times, want none", n-lists)
			}

			delete(w.gitea.Fail, route)
			at(2*time.Minute + time.Second)
			pass(t, w)
			if left := w.gitea.Runners("acme"); len(left) != 0 {
				t.Fatalf("the org still holds %+v, want the pass after the wait to sweep", left)
			}
			if len(w.zerops.Imports) != 1 {
				t.Fatalf("the pass imported %d runners, want one once the sweep is done", len(w.zerops.Imports))
			}
		})
	}
}

// TestASweepIsBounded — one attempt deletes at most four rounds of fifty
// registrations; an org holding more is not imported into until a later
// attempt, after the wait, has deleted the rest.
func TestASweepIsBounded(t *testing.T) {
	t.Parallel()
	w, logs, at := taintedWorld(t)
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
	if !strings.Contains(logs.String(), "remain=1") {
		t.Fatalf("the log does not say one registration remains:\n%s", logs)
	}

	waiting(w)
	at(2*time.Minute + time.Second)
	pass(t, w)
	if left := w.gitea.Runners("acme"); len(left) != 0 || len(w.zerops.Imports) != 1 {
		t.Fatalf("after the second attempt the org holds %d and %d runners were imported, want 0 and 1",
			len(left), len(w.zerops.Imports))
	}
}

// TestASweepSurvivesRowsThatMove — Gitea orders an org's runners by a status
// it computes from the clock, so a row can cross a page boundary between two
// reads. The sweep lists the first page again after each round of deletions
// and ends only when the org holds none.
func TestASweepSurvivesRowsThatMove(t *testing.T) {
	t.Parallel()
	w, _, _ := taintedWorld(t)
	for range 58 { // sixty in all: more than a page
		w.gitea.AddRunner("acme", "runneracme-x")
	}
	reads := 0
	w.gitea.ReorderRunners = func(runners []gitea.Runner) []gitea.Runner {
		reads++
		if reads%2 == 0 {
			slices.Reverse(runners)
		}
		return runners
	}
	taint(t, w)

	if left := w.gitea.Runners("acme"); len(left) != 0 {
		t.Fatalf("the org still holds %d registrations, want none", len(left))
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("imported %d runners, want the replacement", len(w.zerops.Imports))
	}
}

// TestEveryImportSweepsTheGroupsOrgOnce — at an import the group has no
// runner service, so every registration of its org is a dead container's or
// somebody else's: a first import and a broken runner's replacement find an
// empty org with one list, and a tainted one's lists again after deleting. A grant on a
// trusted runner and a deploy pass import nothing, and read nothing.
func TestEveryImportSweepsTheGroupsOrgOnce(t *testing.T) {
	t.Parallel()
	hostname := registry.RunnerHostname("acme")
	runner := func(status string, created time.Time) zerops.Service {
		return zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: hostname, Status: status, Created: created}
	}
	for _, tc := range []struct {
		name    string
		run     func(t *testing.T) *world
		lists   int
		imports int
	}{
		{"a grant on a trusted runner", func(t *testing.T) *world {
			w := granting(t)
			if grant, err := w.pipe.Grant(context.Background(), pushJob(second)); err != nil || grant.Status != deploy.GrantGranted {
				t.Fatalf("Grant = %+v, %v", grant, err)
			}
			return w
		}, 0, 0},
		{"a deploy pass", func(t *testing.T) *world {
			w, _, _ := brokenWorld(t)
			w.zerops.SetServices(giteaPrj, runner("ACTIVE", runnerClock.Add(-time.Hour)))
			pass(t, w)
			return w
		}, 0, 0},
		{"a first import", func(t *testing.T) *world {
			w, _, _ := brokenWorld(t)
			if err := w.pipe.EnsureRunner(context.Background(), "acme", deploy.QueuedJob{}); err != nil {
				t.Fatalf("EnsureRunner: %v", err)
			}
			return w
		}, 1, 1},
		{"a broken runner's replacement", func(t *testing.T) *world {
			w, _, _ := brokenWorld(t)
			w.zerops.SetServices(giteaPrj, runner("READY_TO_DEPLOY", runnerClock.Add(-time.Hour)))
			if err := w.pipe.EnsureRunner(context.Background(), "acme", deploy.QueuedJob{}); err != nil {
				t.Fatalf("EnsureRunner: %v", err)
			}
			return w
		}, 1, 1},
		{"a tainted runner's replacement", func(t *testing.T) *world {
			w, _, _ := taintedWorld(t)
			taint(t, w)
			return w
		}, 2, 1}, // the org's two registrations, then the list that finds none
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := tc.run(t)
			w.queue.Wait()
			w.pipe.WaitRunnerWork()
			if n := callsTo(w, listRunners); n != tc.lists {
				t.Fatalf("listed the org's runner registrations %d times, want %d", n, tc.lists)
			}
			if tc.lists == 0 && callsTo(w, deleteRunner) != 0 {
				t.Fatal("a group that imported nothing deleted a registration")
			}
			if len(w.zerops.Imports) != tc.imports {
				t.Fatalf("imported %d runners, want %d", len(w.zerops.Imports), tc.imports)
			}
		})
	}
}

// TestASweepStopsWithItsContext — a sweep inside a pass with a deadline (the
// first sign-in's) stops at the first deletion after the deadline, logs once,
// and starts no wait: the next import sweeps at once.
func TestASweepStopsWithItsContext(t *testing.T) {
	t.Parallel()
	w, logs, _ := brokenWorld(t)
	for range 10 {
		w.gitea.AddRunner("acme", "runneracme-x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	w.gitea.Trace = func(call string) {
		if strings.HasPrefix(call, deleteRunner) {
			once.Do(cancel)
		}
	}
	if err := w.pipe.EnsureRunner(ctx, "acme", deploy.QueuedJob{}); err == nil {
		t.Fatal("a sweep cut short was not reported")
	}
	if n := callsTo(w, deleteRunner); n != 1 {
		t.Fatalf("deleted %d registrations after the context ended, want to stop at the first", n)
	}
	if n := strings.Count(logs.String(), "cut short"); n != 1 || len(w.zerops.Imports) != 0 {
		t.Fatalf("logged the cut %d times and imported %d, want once and none:\n%s", n, len(w.zerops.Imports), logs)
	}

	if err := w.pipe.EnsureRunner(context.Background(), "acme", deploy.QueuedJob{}); err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if left := w.gitea.Runners("acme"); len(left) != 0 || len(w.zerops.Imports) != 1 {
		t.Fatalf("the next import left %d registrations and imported %d, want 0 and 1", len(left), len(w.zerops.Imports))
	}
}
