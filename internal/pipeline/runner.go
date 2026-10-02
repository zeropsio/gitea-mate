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
// registry, replaces a registered group's runner that is broken, and imports
// one for a registered group that has none while a job of it waits. A runner
// is the one service the broker created by itself, so it is the one it may
// remove; nothing else in a project is ever deleted.
//
// A broken runner is looked for here as well as on a webhook because the
// webhook that would ask was the one that made it: a runner is imported only
// when a job queues, so one that never ran has a job waiting on it, and no
// second `queued` may ever come. The service list is the one this pass reads
// anyway. A group with no runner costs one read of its org's newest runs in
// Gitea: a replacement deletes before it imports, and an import that failed,
// or a restart in between, leaves the waiting job as the only record that the
// group is owed one.
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
	present := map[string]bool{}
	for _, service := range zerops.WithoutSystem(services) {
		if !strings.HasPrefix(service.Name, runnerPrefix) {
			continue
		}
		present[service.Name] = true
		if slug, ok := wanted[service.Name]; ok {
			if p.RunnerImport != "" {
				if err := p.mendRunner(ctx, reg, slug, service); err != nil {
					problems = append(problems, err.Error())
				}
			}
			continue
		}
		stale = append(stale, service)
	}
	if p.RunnerImport != "" {
		for _, group := range reg.Groups {
			if present[registry.RunnerHostname(group.Slug)] {
				continue
			}
			if err := p.owedRunner(ctx, reg, group.Slug); err != nil {
				problems = append(problems, err.Error())
			}
		}
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
//     imported afresh, within the bounds of [Pipeline.mayRebuild].
//
// A queued job is also what lets a group whose builds keep failing try again.
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
	p.mu.Lock()
	if p.queuedAt == nil {
		p.queuedAt = map[string]time.Time{}
	}
	p.queuedAt[hostname] = p.now()
	delete(p.runnerStopped, hostname)
	p.mu.Unlock()
	return p.reviewRunner(ctx, state.Registry, group.Slug)
}

// reviewRunner imports the group's runner if it has none and replaces it if it
// is broken. It is EnsureRunner once the group is known, and what a watched
// build that failed asks for.
func (p *Pipeline) reviewRunner(ctx context.Context, reg registry.Registry, slug string) error {
	hostname := registry.RunnerHostname(slug)
	services, err := p.Zerops.Services(ctx, p.ClientID, p.GiteaProjectID)
	if err != nil {
		return fmt.Errorf("the Gitea project's services: %w", err)
	}
	for _, service := range services {
		if service.Name == hostname {
			return p.mendRunner(ctx, reg, slug, service)
		}
	}
	if p.busy(hostname) {
		return nil
	}
	builds, err := p.runnerBuilds(ctx, hostname)
	if err != nil {
		return err
	}
	if !p.mayRebuild(slug, hostname, builds) {
		return nil
	}
	return p.importRunner(ctx, slug, hostname)
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
		p.watchRunnerBuild(slug, hostname, processes)
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
// still never run, with no build of it moving, is called broken. A runner's
// whole build took 121 s on 2026-10-02 (run 4), and a failed one 54 s; twenty
// minutes is ten times the longer.
const runnerBuildBound = 20 * time.Minute

// How rebuilding a group's runner is bounded, so a build that fails for good —
// and mails every member of the org each time it does — cannot loop. The first
// failed build is replaced at once; after that each attempt waits twice as long
// as the one before, from two minutes up to six hours, no more than three start
// in any hour, and five failed builds in a row stop it until one of the group's
// jobs queues again. The count is read from the platform's own record of the
// builds, so a restart does not lift it, and a build that finished starts it
// again.
const (
	runnerRepairBackoff    = 2 * time.Minute
	runnerRepairBackoffCap = 6 * time.Hour
	runnerRepairsPerHour   = 3
	runnerStopAfter        = 5
)

// runnerBuild is one `stack.build` of a runner hostname, as the platform lists
// it — a deleted service's included.
type runnerBuild struct {
	serviceID string
	status    string
	created   time.Time
}

// runnerBuilds reads the platform's builds of a runner hostname, newest first.
// It is read only when a runner may need building: one that never ran, or a
// group with a job waiting and no runner.
func (p *Pipeline) runnerBuilds(ctx context.Context, hostname string) ([]runnerBuild, error) {
	processes, err := p.Zerops.ProjectProcesses(ctx, p.GiteaProjectID)
	if err != nil {
		return nil, fmt.Errorf("the builds of the runner %s: %w", hostname, err)
	}
	var builds []runnerBuild
	for _, process := range processes {
		if process.ActionName != "stack.build" {
			continue
		}
		for _, stack := range process.ServiceStacks {
			if stack.Name == hostname {
				builds = append(builds, runnerBuild{serviceID: stack.ID, status: process.Status, created: process.Created})
				break
			}
		}
	}
	sort.SliceStable(builds, func(i, j int) bool { return builds[i].created.After(builds[j].created) })
	return builds, nil
}

// brokenRunner says why a runner service can never run, or "" when it has run
// or may yet. A runner that never ran is READY_TO_DEPLOY. It is broken once its
// newest build ended without finishing, or once it is older than any build
// with none of its builds still moving — a slow build is never cut short.
func brokenRunner(service zerops.Service, builds []runnerBuild, now time.Time) string {
	if service.Status != serviceReadyToDeploy {
		return ""
	}
	for _, build := range builds {
		if build.serviceID != service.ID {
			continue
		}
		if !(zerops.Process{Status: build.status}).Done() {
			return ""
		}
		if build.status != zerops.ProcessFinished {
			return "its build ended " + build.status
		}
		break
	}
	if !service.Created.IsZero() && now.Sub(service.Created) > runnerBuildBound {
		return "it has not deployed " + runnerBuildBound.String() + " after it was made"
	}
	return ""
}

// mendRunner replaces the group's runner if it is broken. A runner that runs
// lifts a stop.
func (p *Pipeline) mendRunner(ctx context.Context, reg registry.Registry, slug string, service zerops.Service) error {
	if service.Status != serviceReadyToDeploy {
		switch service.Status {
		case "ACTIVE", "STARTING", "STOPPING", "STOPPED":
			p.mu.Lock()
			delete(p.runnerStopped, service.Name)
			p.mu.Unlock()
		}
		return nil
	}
	if p.stopped(service.Name) {
		return nil
	}
	builds, err := p.runnerBuilds(ctx, service.Name)
	if err != nil {
		return err
	}
	why := brokenRunner(service, builds, p.now())
	if why == "" || p.sharedRunner(reg, service.Name) || !p.mayRebuild(slug, service.Name, builds) {
		return nil
	}
	return p.repairRunner(ctx, slug, service, why)
}

// owedRunner imports a runner for a registered group that has none while one
// of its jobs waits in Gitea.
func (p *Pipeline) owedRunner(ctx context.Context, reg registry.Registry, slug string) error {
	hostname := registry.RunnerHostname(slug)
	if p.stopped(hostname) || p.busy(hostname) || p.sharedRunner(reg, hostname) {
		return nil
	}
	queued, err := p.Gitea.QueuedOrgRuns(ctx, slug)
	if err != nil {
		return fmt.Errorf("the waiting jobs of %s: %w", slug, err)
	}
	if queued == 0 {
		return nil
	}
	builds, err := p.runnerBuilds(ctx, hostname)
	if err != nil {
		return err
	}
	if !p.mayRebuild(slug, hostname, builds) {
		return nil
	}
	p.log().Warn("a group with a job waiting has no runner; one is imported", "group", slug, "hostname", hostname, "waiting", queued)
	return p.importRunner(ctx, slug, hostname)
}

// mayRebuild decides from the platform's record of a runner's builds whether
// another may start now, and logs a refusal — once for a stop.
func (p *Pipeline) mayRebuild(slug, hostname string, builds []runnerBuild) bool {
	if len(builds) > 0 && !(zerops.Process{Status: builds[0].status}).Done() {
		return false // one is building
	}
	var failures []runnerBuild
	for _, build := range builds {
		if build.status == zerops.ProcessFinished {
			break
		}
		failures = append(failures, build)
	}
	if len(failures) == 0 {
		return true
	}
	if len(failures) >= runnerStopAfter {
		p.mu.Lock()
		queued := p.queuedAt[hostname]
		p.mu.Unlock()
		if queued.After(failures[0].created) {
			return true
		}
		p.mu.Lock()
		if p.runnerStopped == nil {
			p.runnerStopped = map[string]bool{}
		}
		p.runnerStopped[hostname] = true
		p.mu.Unlock()
		p.log().Warn("a runner's builds keep failing, so it is stopped until one of the group's jobs queues again",
			"group", slug, "hostname", hostname, "failed_in_a_row", len(failures))
		return false
	}
	// The oldest failure in the run is the build that started it; every later
	// one is a replacement.
	attempts := make([]time.Time, 0, len(failures)-1)
	for i := len(failures) - 2; i >= 0; i-- {
		attempts = append(attempts, failures[i].created)
	}
	now := p.now()
	if next := nextRepair(attempts, now); now.Before(next) {
		p.log().Warn("a broken runner's rebuild is held back", "group", slug, "hostname", hostname,
			"failed_in_a_row", len(failures), "next", next.Format(time.RFC3339))
		return false
	}
	return true
}

// stopped reports a group whose builds kept failing, until a job of it queues
// or its runner runs. Remembering it spares the pass a read of the platform's
// processes every five minutes; a restart forgets it and reads once.
func (p *Pipeline) stopped(hostname string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runnerStopped[hostname]
}

// busy reports a runner being imported or replaced right now.
func (p *Pipeline) busy(hostname string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runnerImport[hostname] || p.replacing[hostname]
}

// sharedRunner reports a hostname two registered groups map to: the slug loses
// its dashes and is cut to 25 characters, so `acme-2` and `acme2` name one
// runner. Replacing it for one group would take it from the other, so the
// broker never does, and says so once.
func (p *Pipeline) sharedRunner(reg registry.Registry, hostname string) bool {
	var slugs []string
	for _, group := range reg.Groups {
		if registry.RunnerHostname(group.Slug) == hostname {
			slugs = append(slugs, group.Slug)
		}
	}
	if len(slugs) < 2 {
		return false
	}
	p.mu.Lock()
	said := p.sharedSaid[hostname]
	if p.sharedSaid == nil {
		p.sharedSaid = map[string]bool{}
	}
	p.sharedSaid[hostname] = true
	p.mu.Unlock()
	if !said {
		p.log().Warn("two groups share one runner hostname, so the broker never replaces it",
			"hostname", hostname, "groups", strings.Join(slugs, ","))
	}
	return true
}

// repairRunner deletes a broken runner and imports a fresh one with a fresh
// registration token. One replacement runs at a time per group, a tainted
// runner's included. A job that queues meanwhile waits, as it does for a first
// import; if the import fails, the pass imports one while that job waits.
func (p *Pipeline) repairRunner(ctx context.Context, slug string, runner zerops.Service, why string) error {
	p.mu.Lock()
	if p.replacing == nil {
		p.replacing = map[string]bool{}
	}
	if p.replacing[runner.Name] {
		p.mu.Unlock()
		return nil
	}
	p.replacing[runner.Name] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.replacing, runner.Name)
		p.mu.Unlock()
	}()

	p.log().Warn("a broken runner is being replaced", "group", slug, "hostname", runner.Name, "why", why)
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
	if err := p.importRunner(ctx, slug, runner.Name); err != nil {
		p.log().Warn("a broken runner was deleted and its replacement could not be imported; a pass imports one while a job waits",
			"group", slug, "hostname", runner.Name, "err", err.Error())
		return err
	}
	p.log().Info("a broken runner was replaced", "group", slug, "hostname", runner.Name)
	return nil
}

// nextRepair is the earliest time another replacement may start, given when
// each replacement since the run of failed builds began started, oldest first.
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
// build that ends without finishing asks for the runner to be looked at again
// at once, rather than on the next pass. It runs on the broker's own context
// and polls only while the build runs.
func (p *Pipeline) watchRunnerBuild(slug, hostname string, processes []string) {
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
				// the pass takes it from here.
				p.log().Info("a runner's build could not be followed to its end", "hostname", hostname, "err", err.Error())
				return
			}
			if final.Status == zerops.ProcessFinished {
				continue
			}
			p.log().Warn("a runner's build failed", "group", slug, "hostname", hostname,
				"action", final.ActionName, "status", final.Status)
			state, err := p.State(ctx)
			if err == nil {
				err = p.reviewRunner(ctx, state.Registry, slug)
			}
			if err != nil {
				p.log().Warn("a runner whose build failed could not be replaced", "group", slug, "hostname", hostname, "err", err.Error())
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
		state, err := p.State(base)
		if err != nil {
			p.log().Warn("a tainted runner was left: the registry could not be read", "hostname", runner.Name, "err", err.Error())
			return
		}
		if p.sharedRunner(state.Registry, runner.Name) {
			return
		}
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
			// The catch-up below still starts the deploy again; its job waits,
			// and the pass imports a runner while it does.
			p.log().Warn("a fresh runner could not be imported", "group", slug, "err", err.Error())
		}
		plan, err := p.Plan(base, slug)
		if err != nil {
			p.log().Warn("a group could not be caught up on its fresh runner", "group", slug, "err", err.Error())
			return
		}
		_ = p.catchUpAll(base, plan)
	}()
}
