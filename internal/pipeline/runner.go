package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

// reconcileRunners deletes the runner service of a group that left the
// registry, and replaces a registered group's runner that is broken. A runner
// is the one service the broker created by itself, so it is the one it may
// remove; nothing else in a project is ever deleted.
//
// A broken runner is looked for here as well as on a webhook because the
// webhook that would ask was the one that made it: a runner is imported only
// when a job queues, so one that never ran has a job waiting on it, and no
// second `queued` may ever come. The list is the one this pass reads anyway.
func (p *Pipeline) reconcileRunners(ctx context.Context, reg registry.Registry) []string {
	services, err := p.Zerops.Services(ctx, p.ClientID, p.GiteaProjectID)
	if err != nil {
		return []string{"the Gitea project's services: " + err.Error()}
	}
	wanted := map[string]string{}
	for _, group := range reg.Groups {
		wanted[registry.RunnerHostname(group.Slug)] = group.Slug
	}

	var problems []string
	var stale []zerops.Service
	for _, service := range zerops.WithoutSystem(services) {
		if !strings.HasPrefix(service.Name, runnerPrefix) {
			continue
		}
		if slug, ok := wanted[service.Name]; ok {
			if p.RunnerImport != "" {
				if err := p.mendRunner(ctx, slug, service); err != nil {
					problems = append(problems, err.Error())
				}
			}
			continue
		}
		stale = append(stale, service)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	for _, service := range stale {
		// The platform answers a process, not a finished deletion: the service
		// is gone only once it has FINISHED, and a pass that did not wait would
		// report a deletion that had not happened yet.
		process, err := p.Zerops.DeleteService(ctx, service.ID)
		if err != nil {
			problems = append(problems, "the runner "+service.Name+" could not be deleted: "+err.Error())
			continue
		}
		final, err := p.Zerops.AwaitProcess(ctx, process.ID, p.PollInterval)
		switch {
		case err != nil:
			problems = append(problems, "the deletion of the runner "+service.Name+" could not be followed: "+err.Error())
		case final.Status != zerops.ProcessFinished:
			problems = append(problems, "the deletion of the runner "+service.Name+" ended as "+final.Status)
		default:
			p.log().Info("a runner of a group that left the registry was deleted", "hostname", service.Name)
		}
	}
	return problems
}

// runnerPrefix is what every runner service's hostname starts with
// (docs/vocabulary.md).
const runnerPrefix = "runner"

// placeholders of import/runner.yaml.
const (
	runnerHostnamePlaceholder = "__HOSTNAME__"
	runnerTokenPlaceholder    = "__TOKEN__"
)

// EnsureRunner gives a group an Actions runner that can run. It is what a
// group's `workflow_job` `queued` calls: a job that queues with no runner online
// waits, and one registered afterwards takes it within a second of starting
// (ledger 2026-09-16).
//
//   - No runner service: one is imported.
//   - A runner that has run, or is still building: nothing.
//   - A broken runner — one that exists and can never run: it is deleted and
//     imported afresh, within the bounds of [Pipeline.repairRunner].
//
// The registration token goes into the import document and nowhere else — not
// a log, not an error, not a file.
func (p *Pipeline) EnsureRunner(ctx context.Context, org string) error {
	if p.RunnerImport == "" {
		return errors.New("the broker holds no runner import document")
	}

	state, err := p.State(ctx)
	if err != nil {
		return err
	}
	group, known := state.Registry.Group(org)
	if !known {
		return fmt.Errorf("the org %s is not a registered group", org)
	}
	hostname := registry.RunnerHostname(group.Slug)

	services, err := p.Zerops.Services(ctx, p.ClientID, p.GiteaProjectID)
	if err != nil {
		return fmt.Errorf("the Gitea project's services: %w", err)
	}
	for _, service := range services {
		if service.Name == hostname {
			return p.mendRunner(ctx, group.Slug, service)
		}
	}
	return p.importRunner(ctx, group.Slug, hostname)
}

// importRunner imports a fresh runner for a group, with a registration token
// minted for it, and watches its build.
func (p *Pipeline) importRunner(ctx context.Context, slug, hostname string) error {
	// One import at a time per group: two `queued` deliveries arrive within a
	// second of each other, and the platform would make two services.
	p.mu.Lock()
	if p.runnerImport == nil {
		p.runnerImport = map[string]bool{}
	}
	if p.runnerImport[hostname] {
		p.mu.Unlock()
		return nil
	}
	p.runnerImport[hostname] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.runnerImport, hostname)
		p.mu.Unlock()
	}()

	// Org scope, never the instance-wide route: labels route jobs, but only
	// the registration scope isolates them (guide 1.6).
	token, err := p.Gitea.RunnerRegistrationToken(ctx, slug)
	if err != nil {
		return fmt.Errorf("a registration token for %s: %w", slug, err)
	}
	if token == "" {
		return fmt.Errorf("Gitea answered an empty registration token for %s", slug)
	}

	document := strings.ReplaceAll(p.RunnerImport, runnerHostnamePlaceholder, hostname)
	document = strings.ReplaceAll(document, runnerTokenPlaceholder, token)
	result, err := p.Zerops.ImportServices(ctx, p.GiteaProjectID, document)
	if err != nil {
		// The error can carry the document, and the document carries the
		// token, so only the fact is reported.
		return fmt.Errorf("the runner %s could not be imported (%d)", hostname, zerops.Status(err))
	}
	p.log().Info("a group's runner was imported", "group", slug, "hostname", hostname)
	for _, stack := range result.ServiceStacks {
		if stack.Name != hostname {
			continue
		}
		processes := make([]string, 0, len(stack.Processes))
		for _, process := range stack.Processes {
			processes = append(processes, process.ID)
		}
		p.watchRunnerBuild(slug, hostname, stack.ID, processes)
	}
	return nil
}

// ---------------------------------------------------------------------------
// A broken runner
// ---------------------------------------------------------------------------

// The platform's status of a service that has never run: no app version is
// active, so there is nothing to start (ledger 2026-09-16, run 4 2026-10-02).
const serviceReadyToDeploy = "READY_TO_DEPLOY"

// runnerBuildBound is how long a runner may take to build before one that has
// still never run is called broken. A runner's whole build took 121 s on
// 2026-10-02 (run 4), and a failed one 54 s; twenty minutes is ten times the
// longer, so a runner this old that has not run is not going to.
const runnerBuildBound = 20 * time.Minute

// How replacing a broken runner is bounded, so a build that fails for good —
// and mails every member of the org each time it does — cannot loop: each
// attempt waits twice as long as the one before, from two minutes up to six
// hours, and no more than three are made in any hour. The count starts again
// once the group's runner has run.
const (
	runnerRepairBackoff    = 2 * time.Minute
	runnerRepairBackoffCap = 6 * time.Hour
	runnerRepairsPerHour   = 3
)

// brokenRunner says why a runner service can never run, or "" when it has run
// or may yet. A runner that never ran is READY_TO_DEPLOY; it is broken once
// the build the broker watched ended without finishing, or once it is older
// than any build.
func (p *Pipeline) brokenRunner(service zerops.Service) string {
	if service.Status != serviceReadyToDeploy {
		return ""
	}
	p.mu.Lock()
	failed := p.failedBuilds[service.ID]
	p.mu.Unlock()
	if failed {
		return "its build failed"
	}
	if !service.Created.IsZero() && p.now().Sub(service.Created) > runnerBuildBound {
		return "it has not deployed " + runnerBuildBound.String() + " after it was made"
	}
	return ""
}

// mendRunner replaces the group's runner if it is broken, and forgets the
// group's replacements once its runner has run. Only a status a service
// reaches by running counts: one being deleted says nothing, and forgetting on
// it would let a replacement skip its wait.
func (p *Pipeline) mendRunner(ctx context.Context, slug string, service zerops.Service) error {
	if why := p.brokenRunner(service); why != "" {
		return p.repairRunner(ctx, slug, service, why)
	}
	switch service.Status {
	case "ACTIVE", "STARTING", "STOPPING", "STOPPED":
		p.mu.Lock()
		delete(p.runnerRepairs, service.Name)
		p.mu.Unlock()
	}
	return nil
}

// repairRunner deletes a broken runner and imports a fresh one with a fresh
// registration token. One replacement runs at a time per group — a tainted
// runner's included — and each is bounded (runnerRepairBackoff): an attempt
// the bound holds back is logged and left to a later webhook or pass. A job
// that queues meanwhile waits, as it does for a first import.
func (p *Pipeline) repairRunner(ctx context.Context, slug string, runner zerops.Service, why string) error {
	now := p.now()
	p.mu.Lock()
	if p.replacing == nil {
		p.replacing = map[string]bool{}
	}
	if p.runnerRepairs == nil {
		p.runnerRepairs = map[string][]time.Time{}
	}
	if p.replacing[runner.Name] {
		p.mu.Unlock()
		return nil
	}
	attempts := p.runnerRepairs[runner.Name]
	if next := nextRepair(attempts, now); now.Before(next) {
		p.mu.Unlock()
		p.log().Warn("a broken runner's replacement is held back", "group", slug, "hostname", runner.Name,
			"why", why, "attempts", len(attempts), "next", next.Format(time.RFC3339))
		return nil
	}
	attempts = append(attempts, now)
	p.runnerRepairs[runner.Name] = attempts
	p.replacing[runner.Name] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.replacing, runner.Name)
		p.mu.Unlock()
	}()

	p.log().Warn("a broken runner is being replaced", "group", slug, "hostname", runner.Name,
		"why", why, "attempt", len(attempts))
	process, err := p.Zerops.DeleteService(ctx, runner.ID)
	if err != nil {
		return fmt.Errorf("the broken runner %s could not be deleted: %w", runner.Name, err)
	}
	final, err := p.Zerops.AwaitProcess(ctx, process.ID, p.PollInterval)
	if err != nil {
		return fmt.Errorf("the deletion of the broken runner %s could not be followed: %w", runner.Name, err)
	}
	if final.Status != zerops.ProcessFinished {
		return fmt.Errorf("the deletion of the broken runner %s ended as %s", runner.Name, final.Status)
	}
	p.mu.Lock()
	delete(p.failedBuilds, runner.ID)
	p.mu.Unlock()
	if err := p.importRunner(ctx, slug, runner.Name); err != nil {
		return err
	}
	p.log().Info("a broken runner was replaced", "group", slug, "hostname", runner.Name, "attempt", len(attempts))
	return nil
}

// nextRepair is the earliest time another replacement may start, given the
// times of the attempts made since the group's runner last ran.
func nextRepair(attempts []time.Time, now time.Time) time.Time {
	if len(attempts) == 0 {
		return time.Time{}
	}
	wait := runnerRepairBackoff
	for i := 1; i < len(attempts) && wait < runnerRepairBackoffCap; i++ {
		wait *= 2
	}
	wait = min(wait, runnerRepairBackoffCap)
	next := attempts[len(attempts)-1].Add(wait)

	var lastHour []time.Time
	for _, at := range attempts {
		if now.Sub(at) < time.Hour {
			lastHour = append(lastHour, at)
		}
	}
	if len(lastHour) >= runnerRepairsPerHour {
		if hourly := lastHour[len(lastHour)-runnerRepairsPerHour].Add(time.Hour); hourly.After(next) {
			next = hourly
		}
	}
	return next
}

// watchRunnerBuild follows the processes an import answered for a runner. A
// build that ends without finishing marks the runner broken at once, rather
// than [runnerBuildBound] later, and asks for its replacement. It runs on the
// broker's own context and polls only while the build runs.
func (p *Pipeline) watchRunnerBuild(slug, hostname, serviceID string, processes []string) {
	if len(processes) == 0 {
		return
	}
	base := p.Base
	if base == nil {
		base = context.Background()
	}
	p.runnerWork.Add(1)
	go func() {
		defer p.runnerWork.Done()
		ctx, cancel := context.WithTimeout(base, runnerBuildBound)
		defer cancel()
		for _, id := range processes {
			final, err := p.Zerops.AwaitProcess(ctx, id, p.PollInterval)
			if err != nil {
				// A build still moving at the bound, or a read that failed:
				// the age rule and the next pass take it from here.
				p.log().Info("a runner's build could not be followed to its end", "hostname", hostname, "err", err.Error())
				return
			}
			if final.Status == zerops.ProcessFinished {
				continue
			}
			p.log().Warn("a runner's build failed", "group", slug, "hostname", hostname,
				"action", final.ActionName, "status", final.Status)
			p.mu.Lock()
			if p.failedBuilds == nil {
				p.failedBuilds = map[string]bool{}
			}
			p.failedBuilds[serviceID] = true
			p.mu.Unlock()
			if err := p.EnsureRunner(ctx, slug); err != nil {
				p.log().Warn("a broken runner could not be replaced", "group", slug, "hostname", hostname, "err", err.Error())
			}
			return
		}
	}()
}

// ---------------------------------------------------------------------------
// A runner's trust (D27)
// ---------------------------------------------------------------------------

// runnerTrust reports whether the group's runner has run nothing but the
// default branches' own workflows since it was made.
//
// Jobs share one container and are root in it, so a branch's own workflow
// could leave a process behind that reads the next job's key. Nothing on the
// runner can be asked — it is the thing in doubt — so the answer is read from
// Gitea: every run of the org that started since the service was created.
// It fails closed: a runner that cannot be found or aged gets no key near it.
func (p *Pipeline) runnerTrust(ctx context.Context, slug string) (zerops.Service, bool, string, error) {
	hostname := registry.RunnerHostname(slug)
	services, err := p.Zerops.Services(ctx, p.ClientID, p.GiteaProjectID)
	if err != nil {
		return zerops.Service{}, false, "", fmt.Errorf("the Gitea project's services: %w", err)
	}
	var runner zerops.Service
	found := false
	for _, service := range services {
		if service.Name == hostname {
			runner, found = service, true
			break
		}
	}
	if !found {
		return zerops.Service{}, false, "", fmt.Errorf("the group has no runner service %s", hostname)
	}
	if runner.Created.IsZero() {
		return runner, false, "", fmt.Errorf("the runner %s does not say when it was made", hostname)
	}

	runs, err := p.Gitea.ListOrgRunsSince(ctx, slug, runner.Created)
	if err != nil {
		return runner, false, "", fmt.Errorf("the org's runs: %w", err)
	}
	for _, run := range runs {
		if run.StartedAt.IsZero() || deploy.TrustedRun(run) {
			continue
		}
		return runner, false, fmt.Sprintf("run %d of %s ran %q's own workflow on the group's runner",
			run.ID, run.Repository.FullName, run.HeadBranch), nil
	}
	return runner, true, "", nil
}

// replaceRunner throws a group's runner away and imports a fresh one, then
// lets a pass start whatever was waiting for it. It returns at once: the work
// runs on the broker's own context, never a request's.
func (p *Pipeline) replaceRunner(slug string, runner zerops.Service) {
	p.mu.Lock()
	if p.replacing == nil {
		p.replacing = map[string]bool{}
	}
	if p.replacing[runner.Name] {
		p.mu.Unlock()
		return
	}
	p.replacing[runner.Name] = true
	p.mu.Unlock()

	base := p.Base
	if base == nil {
		base = context.Background()
	}
	p.runnerWork.Add(1)
	go func() {
		defer p.runnerWork.Done()
		defer func() {
			p.mu.Lock()
			delete(p.replacing, runner.Name)
			p.mu.Unlock()
		}()
		process, err := p.Zerops.DeleteService(base, runner.ID)
		if err != nil {
			p.log().Warn("a tainted runner could not be deleted", "hostname", runner.Name, "err", err.Error())
			return
		}
		final, err := p.Zerops.AwaitProcess(base, process.ID, p.PollInterval)
		if err != nil || final.Status != zerops.ProcessFinished {
			p.log().Warn("a tainted runner's deletion did not finish", "hostname", runner.Name)
			return
		}
		p.log().Info("a tainted runner was deleted", "hostname", runner.Name)
		if err := p.importRunner(base, slug, runner.Name); err != nil {
			p.log().Warn("a fresh runner could not be imported", "group", slug, "err", err.Error())
			return
		}
		plan, err := p.Plan(base, slug)
		if err != nil {
			p.log().Warn("a group could not be caught up on its fresh runner", "group", slug, "err", err.Error())
			return
		}
		_ = p.catchUpAll(base, plan)
	}()
}
