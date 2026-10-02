package pipeline_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// runnerClock is the time every broken-runner test is read at.
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

// brokenWorld is a world whose pipeline imports runners, reads the given
// clock, and logs to a buffer the test can read.
func brokenWorld(t *testing.T, now *time.Time) (*world, *lockedBuffer) {
	t.Helper()
	w := newWorld(t)
	w.pipe.RunnerImport = runnerImport
	w.pipe.PollInterval = time.Millisecond
	w.pipe.Now = func() time.Time { return *now }
	logs := &lockedBuffer{}
	w.pipe.Log = slog.New(slog.NewTextHandler(logs, nil))
	return w, logs
}

// registrationToken is what every token the fake Gitea mints starts with.
const registrationToken = "fake-registration-" + "token"

// TestEnsureRunnerTreatsABrokenRunnerLikeAMissingOne: a runner service that
// exists but can never run — it has not deployed long after a build would have
// — is thrown away and imported afresh, with a fresh registration token. One
// that may yet run is left alone.
func TestEnsureRunnerTreatsABrokenRunnerLikeAMissingOne(t *testing.T) {
	t.Parallel()
	hostname := registry.RunnerHostname("acme")
	for _, tc := range []struct {
		name    string
		runner  *zerops.Service
		deleted []string
		imports int
	}{
		{name: "missing", imports: 1},
		{name: "running", runner: &zerops.Service{Status: "ACTIVE", Created: runnerClock.Add(-time.Hour)}},
		{name: "asleep", runner: &zerops.Service{Status: "STOPPED", Created: runnerClock.Add(-time.Hour)}},
		{name: "still building", runner: &zerops.Service{Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-5 * time.Minute)}},
		{
			name:    "never deployed, past any build",
			runner:  &zerops.Service{Status: "READY_TO_DEPLOY", Created: runnerClock.Add(-30 * time.Minute)},
			deleted: []string{"svc-runner"}, imports: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := runnerClock
			w, logs := brokenWorld(t, &now)
			if tc.runner != nil {
				runner := *tc.runner
				runner.ID, runner.ProjectID, runner.Name = "svc-runner", giteaPrj, hostname
				w.zerops.SetServices(giteaPrj, zerops.Service{ID: "svc-web", ProjectID: giteaPrj, Name: "web"}, runner)
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

// TestARunnerWhoseBuildFailedIsReplacedWithinBounds is run 4: a runner's build
// failed on a download, the service sat READY_TO_DEPLOY, and the job it was
// made for waited for good. The broker watches the build it started, replaces
// a runner whose build failed at once, and backs off when the replacement fails
// too: a little longer each time, and never more than three times an hour.
func TestARunnerWhoseBuildFailedIsReplacedWithinBounds(t *testing.T) {
	t.Parallel()
	now := runnerClock
	w, logs := brokenWorld(t, &now)
	w.zerops.BuildImports(zerops.ProcessFailed)
	ctx := context.Background()

	for _, step := range []struct {
		at      time.Duration
		imports int
		why     string
	}{
		{0, 2, "the first import, and one replacement once its build failed"},
		{time.Minute, 2, "held: the second attempt waits two minutes"},
		{2 * time.Minute, 3, "the second replacement"},
		{5 * time.Minute, 3, "held: the third attempt waits four minutes"},
		{6 * time.Minute, 4, "the third replacement"},
		{40 * time.Minute, 4, "held: three replacements in an hour"},
		{61 * time.Minute, 5, "the first replacement has left the hour"},
	} {
		now = runnerClock.Add(step.at)
		if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
			t.Fatalf("+%s EnsureRunner: %v", step.at, err)
		}
		w.pipe.WaitRunnerWork()
		if len(w.zerops.Imports) != step.imports {
			t.Fatalf("+%s: %d imports, want %d (%s)", step.at, len(w.zerops.Imports), step.imports, step.why)
		}
		if len(w.zerops.DeletedServices) != step.imports-1 {
			t.Fatalf("+%s: deleted %v, want every runner but the newest (%s)", step.at, w.zerops.DeletedServices, step.why)
		}
	}

	// A runner that runs clears the count: the next breakage is replaced at
	// once, however recent the last replacement.
	var newest zerops.Service
	for _, service := range w.zerops.Services(giteaPrj) {
		if service.Name == registry.RunnerHostname("acme") {
			newest = service
		}
	}
	running := newest
	running.Status = "ACTIVE"
	w.zerops.SetServices(giteaPrj, running)
	if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
		t.Fatalf("EnsureRunner on a running runner: %v", err)
	}
	stale := newest
	stale.Created = now.Add(-time.Hour)
	w.zerops.SetServices(giteaPrj, stale)
	if err := w.pipe.EnsureRunner(ctx, "acme"); err != nil {
		t.Fatalf("EnsureRunner after it ran: %v", err)
	}
	w.pipe.WaitRunnerWork()
	if len(w.zerops.Imports) != 6 {
		t.Fatalf("%d imports, want a sixth: the runner ran, so its next breakage waits for nothing", len(w.zerops.Imports))
	}

	// Every replacement registered with a token of its own.
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
	if !strings.Contains(log, "held back") || !strings.Contains(log, "build failed") {
		t.Fatalf("the attempts and their bound were not logged:\n%s", log)
	}
}

// TestARefusedRunnerImportKeepsItsToken: the platform's refusal may quote the
// document, and the document carries the token.
func TestARefusedRunnerImportKeepsItsToken(t *testing.T) {
	t.Parallel()
	now := runnerClock
	w, logs := brokenWorld(t, &now)
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
// that made the runner, so the deploy pass looks too — from the service list it
// reads anyway.
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
			now := runnerClock
			w, _ := brokenWorld(t, &now)
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
