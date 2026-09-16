package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
)

// The catch-up pass.
//
// Gitea does not retry a webhook it could not deliver, so a push while the
// broker was down would otherwise wait for the next push. Each pass compares
// every environment's desired head with the sha in its deployed app version's
// name and deploys the difference — which is why every deploy names its
// version by the commit: an app version carries no source ref of its own.

// PassResult is what one pass did. It is counts and problems, never a name of
// a credential.
type PassResult struct {
	Groups       int
	Environments int
	Deploys      int
	Imports      int
	Problems     []string
}

// LogValue is what the loop logs.
func (r PassResult) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("groups", r.Groups),
		slog.Int("environments", r.Environments),
		slog.Int("deploys", r.Deploys),
		slog.Int("imports", r.Imports),
		slog.Int("problems", len(r.Problems)),
	)
}

// Pass reconciles every group: the recipe first, so a new service exists
// before anything is deployed to it, then the difference between what each
// environment should run and what it does.
func (p *Pipeline) Pass(ctx context.Context) (PassResult, error) {
	var result PassResult

	state, err := p.State(ctx)
	if err != nil {
		return result, err
	}
	result.Groups = len(state.Registry.Groups)

	for _, group := range state.Registry.Groups {
		plan, err := p.Plan(ctx, group.Slug)
		if err != nil {
			result.Problems = append(result.Problems, group.Slug+": "+err.Error())
			continue
		}
		if len(plan.File.Environments) == 0 {
			continue
		}

		for _, env := range plan.File.Environments {
			result.Environments++
			deployed, problems := p.catchUp(ctx, plan, env)
			result.Deploys += deployed
			result.Problems = append(result.Problems, problems...)
		}
	}

	return result, nil
}

// catchUp deploys the difference between one environment's desired heads and
// the shas in its live app versions' names.
func (p *Pipeline) catchUp(ctx context.Context, plan deploy.Plan, env environments.Environment) (int, []string) {
	targets, problems, err := p.Resolver.Resolve(ctx, plan, env, "")
	for i, problem := range problems {
		problems[i] = plan.Slug + "/" + env.Name + ": " + problem
	}
	switch {
	case errors.Is(err, deploy.ErrNoRelease):
		// A group that has released nothing deploys nothing. Not a problem.
		return 0, problems
	case err != nil:
		return 0, append(problems, plan.Slug+"/"+env.Name+": "+err.Error())
	case len(targets) == 0:
		return 0, problems
	}

	live, err := p.liveShas(ctx, env.Project)
	if err != nil {
		return 0, append(problems, plan.Slug+"/"+env.Name+": "+err.Error())
	}

	var behind []deploy.Target
	for _, target := range targets {
		if live[target.Service] != target.Sha {
			behind = append(behind, target)
		}
	}
	if len(behind) == 0 {
		return 0, problems
	}

	p.log().Info("an environment is behind its sources",
		"group", plan.Slug, "environment", env.Name, "services", len(behind))
	p.Queue.Submit(ctx, deploy.Job{Slug: plan.Slug, Environment: env, Targets: behind})
	return len(behind), problems
}

// liveShas is the commit each service of a project is running: the first token
// of its active app version's name.
func (p *Pipeline) liveShas(ctx context.Context, projectID string) (map[string]string, error) {
	services, err := p.Zerops.Services(ctx, p.ClientID, projectID)
	if err != nil {
		return nil, fmt.Errorf("the project's services: %w", err)
	}
	out := map[string]string{}
	for _, service := range services {
		active, live, err := p.Zerops.ActiveAppVersion(ctx, service.ID)
		if err != nil {
			return nil, fmt.Errorf("%s: the app versions: %w", service.Name, err)
		}
		if live {
			out[service.Name] = active.Sha()
		}
	}
	return out, nil
}

// catchUpAll re-checks every environment of a group. It is what the two
// webhooks that mean "main of the group repo moved" end with: the declarations
// may have changed, and an environment that was already right costs one read.
func (p *Pipeline) catchUpAll(ctx context.Context, plan deploy.Plan) error {
	for _, env := range plan.File.Environments {
		if _, problems := p.catchUp(ctx, plan, env); len(problems) > 0 {
			for _, problem := range problems {
				p.log().Warn("an environment could not be caught up", "problem", problem)
			}
		}
	}
	return nil
}
