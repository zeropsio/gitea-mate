package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

// reconcileRunners deletes the runner service of a group that left the
// registry. A runner is the one service the broker created by itself, so it is
// the one it may remove; nothing else in a project is ever deleted.
func (p *Pipeline) reconcileRunners(ctx context.Context, reg registry.Registry) []string {
	services, err := p.Zerops.Services(ctx, p.ClientID, p.GiteaProjectID)
	if err != nil {
		return []string{"the Gitea project's services: " + err.Error()}
	}
	wanted := map[string]bool{}
	for _, group := range reg.Groups {
		wanted[registry.RunnerHostname(group.Slug)] = true
	}

	var problems []string
	var stale []zerops.Service
	for _, service := range zerops.WithoutSystem(services) {
		if !strings.HasPrefix(service.Name, runnerPrefix) || wanted[service.Name] {
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

// EnsureRunner imports a group's Actions runner if the Gitea project does not
// have one yet. It is what a group's first `workflow_job` `queued` calls: a job
// that queues with no runner online waits, and one registered afterwards takes
// it within a second of starting (ledger 2026-09-16).
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
			return nil
		}
	}

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
	token, err := p.Gitea.RunnerRegistrationToken(ctx, group.Slug)
	if err != nil {
		return fmt.Errorf("a registration token for %s: %w", group.Slug, err)
	}
	if token == "" {
		return fmt.Errorf("Gitea answered an empty registration token for %s", group.Slug)
	}

	document := strings.ReplaceAll(p.RunnerImport, runnerHostnamePlaceholder, hostname)
	document = strings.ReplaceAll(document, runnerTokenPlaceholder, token)
	if _, err := p.Zerops.ImportServices(ctx, p.GiteaProjectID, document); err != nil {
		// The error can carry the document, and the document carries the
		// token, so only the fact is reported.
		return fmt.Errorf("the runner %s could not be imported (%d)", hostname, zerops.Status(err))
	}
	p.log().Info("a group's runner was imported", "group", group.Slug, "hostname", hostname)
	return nil
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
	go func() {
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
		if err := p.EnsureRunner(base, slug); err != nil {
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
