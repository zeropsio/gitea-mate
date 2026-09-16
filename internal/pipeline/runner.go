package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	for _, service := range services {
		if !strings.HasPrefix(service.Name, runnerPrefix) || wanted[service.Name] {
			continue
		}
		stale = append(stale, service)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	for _, service := range stale {
		if err := p.Zerops.DeleteService(ctx, service.ID); err != nil {
			problems = append(problems, "the runner "+service.Name+" could not be deleted: "+err.Error())
			continue
		}
		p.log().Info("a runner of a group that left the registry was deleted", "hostname", service.Name)
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
