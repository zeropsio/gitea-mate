package pipeline_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/pipeline"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The import document as import/runner.yaml carries it, placeholders and all.
const runnerImport = `
services:
  - hostname: __HOSTNAME__
    type: ubuntu@26.04
    buildFromGit: https://github.com/zeropsio/gitea-mate
    zeropsSetup: runner
    minContainers: 1
    vault:
      RUNNER_REGISTRATION_TOKEN:
        value: __TOKEN__
        sensitive: true
`

func TestARunnerIsImportedOnAGroupsFirstWorkflow(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.pipe.RunnerImport = runnerImport
	ctx := context.Background()

	if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("imports = %+v", w.zerops.Imports)
	}
	imported := w.zerops.Imports[0]
	if imported.ProjectID != giteaPrj {
		t.Fatalf("the runner was imported into %s, want the Gitea project", imported.ProjectID)
	}
	// docs/vocabulary.md: "runner" plus the slug with its dashes removed, cut
	// to 25 — Zerops hostnames are [a-z0-9].
	if !strings.Contains(imported.Yaml, "hostname: "+registry.RunnerHostname("acme")) {
		t.Fatalf("the document does not name the runner's hostname:\n%s", imported.Yaml)
	}
	for _, placeholder := range []string{"__HOSTNAME__", "__TOKEN__"} {
		if strings.Contains(imported.Yaml, placeholder) {
			t.Fatalf("%s was not filled in", placeholder)
		}
	}
	if !strings.Contains(imported.Yaml, "sensitive: true") {
		t.Fatalf("the registration token is not sensitive:\n%s", imported.Yaml)
	}
}

func TestARunnerIsImportedOnlyOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.pipe.RunnerImport = runnerImport
	ctx := context.Background()

	hostname := registry.RunnerHostname("acme")
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: hostname, Status: "STOPPED"},
	)

	if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
		t.Fatalf("EnsureRunner: %v", err)
	}
	if len(w.zerops.Imports) != 0 {
		t.Fatalf("a group that already has a runner imported %+v", w.zerops.Imports)
	}
}

func TestEnsureRunnerRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		org      string
		document string
		want     string
	}{
		{"an org that is not a registered group", "stranger", runnerImport, "not a registered group"},
		{"a broker with no import document", "acme", "", "no runner import document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.pipe.RunnerImport = tc.document
			err := w.pipe.EnsureRunner(context.Background(), tc.org)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("EnsureRunner = %v, want it to mention %q", err, tc.want)
			}
			if len(w.zerops.Imports) != 0 {
				t.Fatalf("a refusal still imported %+v", w.zerops.Imports)
			}
		})
	}
}

func TestARunnerOfAGroupThatLeftTheRegistryIsDeleted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner-acme", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme")},
		zerops.Service{ID: "svc-runner-gone", ProjectID: giteaPrj, Name: registry.RunnerHostname("departed")},
	)

	if _, err := w.pipe.Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	if len(w.zerops.DeletedServices) != 1 || w.zerops.DeletedServices[0] != "svc-runner-gone" {
		t.Fatalf("the pass deleted %v, want only the departed group's runner", w.zerops.DeletedServices)
	}
	// The Gitea project's own services are never touched: a runner is the one
	// service the broker made by itself.
	names := map[string]bool{}
	for _, service := range w.zerops.Services(giteaPrj) {
		names[service.Name] = true
	}
	if !names["web"] || !names[registry.RunnerHostname("acme")] {
		t.Fatalf("the pass removed something it should not have: %v", names)
	}
}

// TestADeletionThatFailsIsReportedNotAssumed: the platform answers a process,
// not a finished deletion, so a pass that did not wait would report a runner
// gone that is still there.
func TestADeletionThatFailsIsReportedNotAssumed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.zerops.FailDeletes = true
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner-gone", ProjectID: giteaPrj, Name: registry.RunnerHostname("departed")},
	)

	result, err := w.pipe.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	w.queue.Wait()

	found := false
	for _, problem := range result.Problems {
		if strings.Contains(problem, registry.RunnerHostname("departed")) && strings.Contains(problem, "FAILED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failed deletion was not reported: %v", result.Problems)
	}
}

// ---------------------------------------------------------------------------
// A broken runner
// ---------------------------------------------------------------------------

// runnerClock is the time every broken-runner test starts at.
var runnerClock = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// lockedBuffer is a log the broker's goroutines may write while a test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// brokenWorld is a world whose pipeline imports runners and logs to a buffer
// the test can read. Its clock, the broker's and the platform's alike, starts
// at runnerClock and moves with at.
func brokenWorld(t *testing.T) (*world, *lockedBuffer, func(time.Duration)) {
	t.Helper()
	w := newWorld(t)
	now := runnerClock
	w.zerops.Now = now
	logs := &lockedBuffer{}
	configureRunners(w.pipe, &now, logs)
	at := func(d time.Duration) {
		now = runnerClock.Add(d)
		w.zerops.Now = now
	}
	return w, logs, at
}

func configureRunners(p *pipeline.Pipeline, now *time.Time, logs *lockedBuffer) {
	p.RunnerImport = runnerImport
	p.PollInterval = time.Millisecond
	p.Now = func() time.Time { return *now }
	p.Log = slog.New(slog.NewTextHandler(logs, nil))
}

// restarted is the broker after a restart: the same platform and Gitea, and
// nothing in memory.
func restarted(w *world, logs *lockedBuffer) *pipeline.Pipeline {
	old := w.pipe
	now := old.Now()
	fresh := &pipeline.Pipeline{
		Zerops: old.Zerops, Gitea: old.Gitea, ClientID: old.ClientID, GiteaProjectID: old.GiteaProjectID,
		Resolver: old.Resolver, Queue: old.Queue, Records: old.Records,
	}
	configureRunners(fresh, &now, logs)
	fresh.Now = old.Now
	return fresh
}

// registrationToken is what every token the fake Gitea mints starts with.
const registrationToken = "fake-registration-" + "token"

// seedBuild records a runner build on the platform, as its process list shows
// it.
func seedBuild(w *world, id, serviceID, hostname, status string, created time.Time) {
	w.zerops.AddProcess(zerops.Process{
		ID: id, ProjectID: giteaPrj, ActionName: "stack.build", Status: status, Created: created,
		ServiceStacks: []zerops.ProcessStack{{ID: serviceID, Name: hostname}},
	})
}

// TestEnsureRunnerTreatsABrokenRunnerLikeAMissingOne: a runner service that
// exists but can never run — its build failed, or it has not deployed long
// after a build would have — is thrown away and imported afresh, with a fresh
// registration token. One that may yet run is left alone, a build still moving
// past the age above all.
func TestEnsureRunnerTreatsABrokenRunnerLikeAMissingOne(t *testing.T) {
	t.Parallel()
	hostname := registry.RunnerHostname("acme")
	for _, tc := range []struct {
		name    string
		runner  *zerops.Service
		build   string
		deleted []string
		imports int
	}{
		{name: "missing", imports: 1},
		{name: "running", runner: &zerops.Service{Status: "ACTIVE", Created: runnerClock.Add(-time.Hour)}},
		{name: "asleep", runner: &zerops.Service{Status: "STOPPED", Created: runnerClock.Add(-time.Hour)}},
		{name: "still building", runner: &zerops.Service{Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-5 * time.Minute)}, build: "RUNNING"},
		{
			name:   "past any build, but its build still runs",
			runner: &zerops.Service{Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-30 * time.Minute)}, build: "RUNNING",
		},
		{
			name:   "its build failed",
			runner: &zerops.Service{Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-time.Minute)}, build: zerops.ProcessFailed,
			deleted: []string{"svc-runner"}, imports: 1,
		},
		{
			name:    "never deployed, past any build",
			runner:  &zerops.Service{Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-30 * time.Minute)},
			deleted: []string{"svc-runner"}, imports: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, logs, _ := brokenWorld(t)
			if tc.runner != nil {
				runner := *tc.runner
				runner.ID, runner.ProjectID, runner.Name = "svc-runner", giteaPrj, hostname
				w.zerops.SetServices(giteaPrj, zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"}, runner)
				if tc.build != "" {
					seedBuild(w, "proc-seeded-build", runner.ID, hostname, tc.build, runner.Created)
				}
			}

			if err := w.pipe.EnsureRunner(context.Background(), "acme"); err != nil {
				t.Fatalf("EnsureRunner: %v", err)
			}
			w.pipe.WaitRunnerWork()

			if !slices.Equal(w.zerops.DeletedServices, tc.deleted) {
				t.Fatalf("deleted %v, want %v", w.zerops.DeletedServices, tc.deleted)
			}
			if len(w.zerops.Imports) != tc.imports {
				t.Fatalf("imported %d runners, want %d", len(w.zerops.Imports), tc.imports)
			}
			if tc.imports > 0 && !strings.Contains(w.zerops.Imports[0].Yaml, registrationToken) {
				t.Fatalf("the import carries no fresh registration token:\n%s", w.zerops.Imports[0].Yaml)
			}
			if strings.Contains(logs.String(), registrationToken) {
				t.Fatalf("a registration token reached the log:\n%s", logs)
			}
		})
	}
}

// TestABuildThatAlwaysFailsIsBoundedAndStops is run 4 carried on: a runner's
// build failed on a download, and the service sat READY_TO_DEPLOY with its job
// waiting. The broker watches the build it started and replaces a runner whose
// build failed at once; after that each attempt waits twice as long, no more
// than three replacements start in an hour, and five failed builds in a row
// stop it until a job queues again — each failed build mails every member of
// the org. The count is the platform's own record of the builds, so a restart
// does not lift it, and a build that finished starts it again.
func TestABuildThatAlwaysFailsIsBoundedAndStops(t *testing.T) {
	t.Parallel()
	w, logs, at := brokenWorld(t)
	w.zerops.BuildImports(zerops.ProcessFailed)
	w.gitea.AddRun("acme", gitea.Run{ID: 41, Event: "push", HeadBranch: "main", Status: gitea.RunQueued})
	ctx := context.Background()
	hostname := registry.RunnerHostname("acme")

	for _, step := range []struct {
		at      time.Duration
		do      string
		imports int
		why     string
	}{
		{0, "queued", 2, "the first import, and one replacement once its build failed"},
		{time.Minute, "pass", 2, "held: the second replacement waits two minutes"},
		{2 * time.Minute, "pass", 3, "the second replacement"},
		{5 * time.Minute, "pass", 3, "held: the third waits four minutes"},
		{6 * time.Minute, "pass", 4, "the third replacement"},
		{40 * time.Minute, "pass", 4, "held: three replacements in an hour"},
		{61 * time.Minute, "pass", 5, "the first replacement has left the hour"},
		{3 * time.Hour, "pass", 5, "stopped: five failed builds in a row"},
		{3*time.Hour + time.Minute, "restart", 5, "a restart does not lift the stop"},
		{3*time.Hour + 2*time.Minute, "queued", 6, "a job queued again: one more build"},
		{3*time.Hour + 30*time.Minute, "pass", 6, "stopped again: that build failed too"},
		{3*time.Hour + 31*time.Minute, "fixed", 7, "a job queued, and this build finishes"},
		{4 * time.Hour, "later", 8, "a later runner's first failed build is replaced at once"},
	} {
		at(step.at)
		var err error
		switch step.do {
		case "queued":
			err = w.pipe.EnsureRunner(ctx, "acme")
		case "pass":
			_, err = w.pipe.Pass(ctx)
		case "restart":
			w.pipe = restarted(w, logs)
			_, err = w.pipe.Pass(ctx)
		case "fixed":
			w.zerops.BuildImports(zerops.ProcessFinished)
			err = w.pipe.EnsureRunner(ctx, "acme")
		case "later":
			// The runner that ran was replaced (a tainted one is), and the
			// replacement's build failed.
			w.zerops.BuildImports(zerops.ProcessFailed)
			later := zerops.Service{ID: "svc-later", ProjectID: giteaPrj, Name: hostname,
				Status: "READY_TO_DEPLOY", Created: runnerClock.Add(step.at)}
			w.zerops.SetServices(giteaPrj, zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"}, later)
			seedBuild(w, "proc-later-build", later.ID, hostname, zerops.ProcessFailed, later.Created)
			_, err = w.pipe.Pass(ctx)
		}
		if err != nil {
			t.Fatalf("+%s %s: %v", step.at, step.do, err)
		}
		w.queue.Wait()
		w.pipe.WaitRunnerWork()
		if len(w.zerops.Imports) != step.imports {
			t.Fatalf("+%s %s: %d imports, want %d (%s)", step.at, step.do, len(w.zerops.Imports), step.imports, step.why)
		}
	}

	// Every import registered with a token of its own.
	tokens := map[string]bool{}
	for _, imported := range w.zerops.Imports {
		for _, line := range strings.Split(imported.Yaml, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "value: "); ok {
				tokens[value] = true
			}
		}
	}
	if len(tokens) != len(w.zerops.Imports) {
		t.Fatalf("%d imports registered with %d distinct tokens", len(w.zerops.Imports), len(tokens))
	}
	log := logs.String()
	if strings.Contains(log, registrationToken) {
		t.Fatalf("a registration token reached the log:\n%s", log)
	}
	for _, said := range []string{"held back", "stopped", "build failed"} {
		if !strings.Contains(log, said) {
			t.Fatalf("the log never says %q:\n%s", said, log)
		}
	}
}

// TestARunnerDeletedButNotImportedIsOwedOne: a replacement deletes first and
// imports second, and the import can fail — or the broker can restart in
// between. The job the runner was made for still waits in Gitea, and that is
// what a pass reads to import the runner the group is owed.
func TestARunnerDeletedButNotImportedIsOwedOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		waiting bool
		restart bool
		imports int
	}{
		{name: "the same broker", waiting: true, imports: 1},
		{name: "a restarted broker", waiting: true, restart: true, imports: 1},
		{name: "no job waits", imports: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, logs, at := brokenWorld(t)
			if tc.waiting {
				w.gitea.AddRun("acme", gitea.Run{ID: 41, Event: "push", HeadBranch: "main", Status: gitea.RunQueued})
			}
			w.zerops.SetServices(giteaPrj,
				zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
				zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme"),
					Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-time.Hour)})
			importRoute := "POST /project/" + giteaPrj + "/service-stack/import"
			w.zerops.Fail[importRoute] = http.StatusServiceUnavailable
			w.zerops.FailTimes[importRoute] = 1

			if err := w.pipe.EnsureRunner(context.Background(), "acme"); err == nil {
				t.Fatal("a refused import was not reported")
			}
			w.pipe.WaitRunnerWork()
			if len(w.zerops.DeletedServices) != 1 || len(w.zerops.Imports) != 0 {
				t.Fatalf("deleted %v and imported %d, want the runner deleted and none imported",
					w.zerops.DeletedServices, len(w.zerops.Imports))
			}

			at(5 * time.Minute)
			if tc.restart {
				w.pipe = restarted(w, logs)
			}
			if _, err := w.pipe.Pass(context.Background()); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			w.queue.Wait()
			w.pipe.WaitRunnerWork()
			if len(w.zerops.Imports) != tc.imports {
				t.Fatalf("the pass imported %d runners, want %d", len(w.zerops.Imports), tc.imports)
			}
		})
	}
}

// TestOneReplacementAtATime: a second look at a broken runner while its
// replacement is still deleting it leaves it to that replacement.
func TestOneReplacementAtATime(t *testing.T) {
	t.Parallel()
	w, _, _ := brokenWorld(t)
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme"),
			Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-time.Hour)})
	release := make(chan struct{})
	w.zerops.HoldDeletes = release
	deletion := "DELETE /service-stack/svc-runner"

	first := make(chan error, 1)
	go func() { first <- w.pipe.EnsureRunner(context.Background(), "acme") }()
	deadline := time.Now().Add(5 * time.Second)
	for w.zerops.Served(deletion) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first replacement never started deleting")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = w.pipe.EnsureRunner(ctx, "acme")
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first replacement: %v", err)
	}
	w.pipe.WaitRunnerWork()
	if n := w.zerops.Served(deletion); n != 1 {
		t.Fatalf("the runner was deleted %d times, want once", n)
	}
	if len(w.zerops.Imports) != 1 {
		t.Fatalf("imported %d runners, want one", len(w.zerops.Imports))
	}
}

// TestARunnerTwoGroupsShareIsNeverReplaced: a hostname drops the slug's dashes
// and is cut to 25 characters, so two groups can name one runner. Replacing it
// for one would take it from the other; it is left alone and said once.
func TestARunnerTwoGroupsShareIsNeverReplaced(t *testing.T) {
	t.Parallel()
	w, logs, _ := brokenWorld(t)
	hq, _ := w.zerops.Project(giteaPrj)
	hq.TagList = append(hq.TagList, "mate:gn:g-2:ac-me")
	var projects []zerops.Project
	for _, id := range []string{giteaPrj, matePrj, stagePrj, prodPrj} {
		project, _ := w.zerops.Project(id)
		if id == giteaPrj {
			project = hq
		}
		projects = append(projects, project)
	}
	w.zerops.SetProjects(projects...)
	if registry.RunnerHostname("ac-me") != registry.RunnerHostname("acme") {
		t.Fatal("the fixture's two slugs do not share a runner hostname")
	}
	w.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
		zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme"),
			Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-time.Hour)})

	for range 2 {
		if err := w.pipe.EnsureRunner(context.Background(), "acme"); err != nil {
			t.Fatalf("EnsureRunner: %v", err)
		}
		if _, err := w.pipe.Pass(context.Background()); err != nil {
			t.Fatalf("Pass: %v", err)
		}
		w.queue.Wait()
		w.pipe.WaitRunnerWork()
	}
	if len(w.zerops.DeletedServices) != 0 || len(w.zerops.Imports) != 0 {
		t.Fatalf("deleted %v and imported %d, want the shared runner untouched", w.zerops.DeletedServices, len(w.zerops.Imports))
	}
	if n := strings.Count(logs.String(), "share"); n != 1 {
		t.Fatalf("the clash was said %d times, want once:\n%s", n, logs)
	}
}

// TestARefusedRunnerImportKeepsItsToken: the platform's refusal may quote the
// document, and the document carries the token.
func TestARefusedRunnerImportKeepsItsToken(t *testing.T) {
	t.Parallel()
	w, logs, _ := brokenWorld(t)
	w.zerops.FailImports = true
	w.zerops.SetServices(giteaPrj, zerops.Service{ID: "svc-runner", ProjectID: giteaPrj,
		Name: registry.RunnerHostname("acme"), Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-time.Hour)})

	err := w.pipe.EnsureRunner(context.Background(), "acme")
	w.pipe.WaitRunnerWork()
	if err == nil {
		t.Fatal("a refused import was not reported")
	}
	if len(w.zerops.DeletedServices) != 1 {
		t.Fatalf("deleted %v, want the broken runner", w.zerops.DeletedServices)
	}
	if strings.Contains(err.Error(), registrationToken) || strings.Contains(logs.String(), registrationToken) {
		t.Fatalf("the token leaked: err %v, log:\n%s", err, logs)
	}
}

// TestAPassReplacesABrokenRunner: the webhook that would have asked was the one
// that made the runner, so the deploy pass looks too.
func TestAPassReplacesABrokenRunner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		age     time.Duration
		deleted []string
	}{
		{"never deployed, past any build", 30 * time.Minute, []string{"svc-runner"}},
		{"still building", 5 * time.Minute, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, _, _ := brokenWorld(t)
			w.zerops.SetServices(giteaPrj,
				zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"},
				zerops.Service{ID: "svc-runner", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme"),
					Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-tc.age)},
			)
			if _, err := w.pipe.Pass(context.Background()); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			w.queue.Wait()
			w.pipe.WaitRunnerWork()
			if !slices.Equal(w.zerops.DeletedServices, tc.deleted) {
				t.Fatalf("deleted %v, want %v", w.zerops.DeletedServices, tc.deleted)
			}
			if len(w.zerops.Imports) != len(tc.deleted) {
				t.Fatalf("imported %d runners, want %d", len(w.zerops.Imports), len(tc.deleted))
			}
		})
	}
}
