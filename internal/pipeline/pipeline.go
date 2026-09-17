// Package pipeline is the broker's deploy orchestration: what the webhooks and
// the catch-up pass do with a group's protected state.
//
// It owns nothing durable. Every pass re-reads the registry from the Gitea
// project's tags, `environments.yaml` and the tiers from each group repo's
// `main`, and what is live from the Zerops app versions' names — so a restart
// is always safe, and a webhook Gitea could not deliver is caught up by the
// next pass rather than lost (guide 1.3, "The loop also catches up").
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// Pipeline is the whole deploy side of the broker.
type Pipeline struct {
	Zerops *zerops.Client
	Gitea  *gitea.Client
	Log    *slog.Logger

	// ClientID is the Zerops org; GiteaProjectID the project the registry
	// lives on and the runners are imported into.
	ClientID       string
	GiteaProjectID string

	// Resolver decides what a given environment should be running; Queue and
	// Records are the execution side.
	Resolver *deploy.Resolver
	Queue    *deploy.Queue
	Records  *deploy.Records

	// Nudge asks the rights loop for a pass soon. A Mate's recipe pull request
	// is merged by that pass (D23), and a nudge makes it now rather than at the
	// next tick. Optional.
	Nudge func()

	// RunnerImport is the text of import/runner.yaml, with its two
	// placeholders still in it.
	RunnerImport string

	// PollInterval is how often a platform process the pass started is asked
	// where it is. Zero means the client's default.
	PollInterval time.Duration

	// seenRecipe is the blob sha of each tier's import.yaml the last pass saw,
	// keyed "{slug}/{tier}". It is the only thing this package remembers, and
	// a restart deliberately starts empty: the first pass after one reports
	// differences and records what it saw, and only a later change imports.
	mu         sync.Mutex
	seenRecipe map[string]string
	// seenBlocks is each tier's per-service declaration as the last pass read
	// it, keyed "{slug}/{tier}" then hostname. It is what tells a scaling
	// change from a reordering, so one can be reported.
	seenBlocks   map[string]map[string]string
	reconciled   map[string]bool
	runnerImport map[string]bool
}

func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// State is the Zerops half of a pass: the registry and who the org's people
// are, read through the same function the rights loop uses so one rule is
// applied everywhere.
func (p *Pipeline) State(ctx context.Context) (mirror.State, error) {
	return mirror.ReadOrg(ctx, p.Zerops, p.ClientID, p.GiteaProjectID)
}

// Plan reads one group's declarations and tiers from its group repo's `main`.
// A group with no group repo, or none that declares environments, answers an
// empty plan and no error.
func (p *Pipeline) Plan(ctx context.Context, slug string) (deploy.Plan, error) {
	file, err := environments.Read(ctx, p.Gitea, slug, registry.GroupRepo)
	if err != nil {
		if gitea.IsNotFound(err) {
			return deploy.Plan{Slug: slug}, nil
		}
		return deploy.Plan{Slug: slug}, err
	}
	plan := deploy.Plan{Slug: slug, File: file, Recipes: map[environments.Tier]environments.Recipe{}}
	if len(file.Environments) == 0 {
		return plan, nil
	}

	needed := map[environments.Tier]bool{}
	for _, env := range file.Environments {
		needed[env.Tier] = true
	}
	for tier := range needed {
		recipe, _, err := environments.ReadRecipe(ctx, p.Gitea, slug, registry.GroupRepo, tier)
		if err != nil {
			// A tier that cannot be read is reported, and the environments
			// built from it deploy nothing — never guessed at.
			p.log().Warn("a group's tier could not be read", "group", slug, "tier", tier, "err", err.Error())
			continue
		}
		plan.Recipes[tier] = recipe
	}
	return plan, nil
}

// Deploy resolves an environment and queues the work. service may be empty for
// every runtime service of the tier. records are the deploy records the job
// answers for.
//
// Nothing a caller passes reaches a commit: the shas come from the resolver,
// which reads protected state alone.
func (p *Pipeline) Deploy(ctx context.Context, plan deploy.Plan, env environments.Environment, service string, records []string) error {
	targets, problems, err := p.Resolver.Resolve(ctx, plan, env, service)
	for _, problem := range problems {
		p.log().Warn("an environment could not be fully resolved", "group", plan.Slug, "problem", problem)
	}
	if err != nil {
		if errors.Is(err, deploy.ErrNoRelease) {
			p.Records.UpdateAll(records, func(r *deploy.Record) {
				r.Status = deploy.StatusFailed
				r.Message = "this group has no approved release yet"
			})
			return nil
		}
		p.Records.UpdateAll(records, func(r *deploy.Record) {
			r.Status = deploy.StatusFailed
			r.Message = err.Error()
		})
		return err
	}
	if len(targets) == 0 {
		p.Records.UpdateAll(records, func(r *deploy.Record) {
			r.Status = deploy.StatusFailed
			r.Message = "nothing to deploy: " + summarise(problems)
		})
		return nil
	}

	// The records carry the sha the resolver decided before the job is queued,
	// so the 202 answers with the commit a caller could not have named.
	p.Records.UpdateAll(records, func(r *deploy.Record) {
		for _, target := range targets {
			if r.Service == "" || r.Service == target.Service {
				r.Sha = target.Sha
				break
			}
		}
	})
	p.Queue.Submit(deploy.Job{
		Slug: plan.Slug, Environment: env, Targets: targets, Records: records,
	})
	return nil
}

func summarise(problems []string) string {
	if len(problems) == 0 {
		return "the environment resolved to no service"
	}
	return problems[0]
}

// ServiceOfRepository finds the recipe service whose repository is the given
// one.
func ServiceOfRepository(recipe environments.Recipe, owner, repo string) (environments.RecipeService, bool) {
	for _, service := range recipe.Runtimes() {
		if o, r, ok := service.Repository(); ok && o == owner && r == repo {
			return service, true
		}
	}
	return environments.RecipeService{}, false
}

// fullName splits an "owner/name".
func fullName(full string) (string, string, error) {
	owner, name, ok := cut(full)
	if !ok {
		return "", "", fmt.Errorf("%q is not an owner/name", full)
	}
	return owner, name, nil
}

func cut(full string) (string, string, bool) {
	for i := 0; i < len(full); i++ {
		if full[i] == '/' {
			return full[:i], full[i+1:], true
		}
	}
	return "", "", false
}
