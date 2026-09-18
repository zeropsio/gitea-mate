package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/registry"
)

// Resolving is where "what is deployed" is decided, and it reads protected
// state only: a source branch's head, or the commits the newest approved
// release tag lists. A caller picks an environment and a service; nothing a
// caller says reaches a sha.

// Plan is one group's state, read fresh from its group repo.
type Plan struct {
	// Slug is the Gitea org.
	Slug string
	// File is environments.yaml.
	File environments.File
	// Recipes is each tier's import.yaml, by tier.
	Recipes map[environments.Tier]environments.Recipe
}

// Resolver turns an environment into the targets that should be live.
type Resolver struct {
	Gitea  *gitea.Client
	Merger *Merger
	Log    *slog.Logger
}

func (r *Resolver) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// ErrNoRelease is returned for a production environment with no approved tag.
// It is not a failure: a group that has released nothing deploys nothing.
var ErrNoRelease = errors.New("no approved release")

// ErrUnknownService is returned when a caller names a service the
// environment's tier does not carry.
var ErrUnknownService = errors.New("unknown service")

// Resolve reads the plan for one environment. only names a single service, or
// is empty for every runtime service of the tier, in its priority order.
//
// Problems are returned alongside the targets: a source that would not merge
// leaves that service out and says so, and the rest of the environment still
// deploys.
func (r *Resolver) Resolve(ctx context.Context, plan Plan, env environments.Environment, only string) ([]Target, []string, error) {
	recipe, known := plan.Recipes[env.Tier]
	if !known {
		return nil, nil, fmt.Errorf("the group has no %s tier", env.Tier)
	}
	services := recipe.Runtimes()
	if only != "" {
		service, found := recipe.Service(only)
		if !found || !service.Runtime() {
			return nil, nil, fmt.Errorf("%w: the %s tier has no runtime service %s", ErrUnknownService, env.Tier, only)
		}
		services = []environments.RecipeService{service}
	}

	if env.Release {
		return r.production(ctx, plan, env, services)
	}
	return r.stage(ctx, plan, env, services)
}

// stage resolves an environment that follows branches: each service's
// repository at the environment's branch.
func (r *Resolver) stage(ctx context.Context, plan Plan, env environments.Environment, services []environments.RecipeService) ([]Target, []string, error) {
	var targets []Target
	var problems []string
	for _, service := range services {
		owner, repo, ok := service.Repository()
		if !ok {
			continue
		}
		sha, err := r.Merger.Head(ctx, owner, repo, env)
		switch {
		case errors.Is(err, ErrConflict):
			problems = append(problems, fmt.Sprintf("%s/%s: %v", env.Name, service.Hostname, err))
			r.reportMerge(ctx, owner, repo, env, err)
			continue
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s/%s: %v", env.Name, service.Hostname, err))
			continue
		case sha == "":
			problems = append(problems, fmt.Sprintf("%s/%s: %s/%s has no %s yet", env.Name, service.Hostname, owner, repo, env.Branch()))
			continue
		}
		targets = append(targets, Target{
			Service: service.Hostname, Owner: owner, Repo: repo, Sha: sha,
			VersionName: sha, Setup: service.ZeropsSetup,
		})
	}
	return targets, problems, nil
}

// production resolves an environment that follows releases: the commits the
// newest approved tag lists, and nothing else.
func (r *Resolver) production(ctx context.Context, plan Plan, env environments.Environment, services []environments.RecipeService) ([]Target, []string, error) {
	release, found, err := NewestApproved(ctx, r.Gitea, plan.Slug, registry.GroupRepo)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, ErrNoRelease
	}

	gate := r.gateOf(plan, env)

	var targets []Target
	var problems []string
	for _, service := range services {
		owner, repo, ok := service.Repository()
		if !ok {
			continue
		}
		sha, listed := release.Services[service.Hostname]
		if !listed {
			problems = append(problems, fmt.Sprintf("%s: %s lists no commit for %s", env.Name, release.Tag, service.Hostname))
			continue
		}
		target := Target{
			Service: service.Hostname, Owner: owner, Repo: repo, Sha: sha,
			VersionName: sha + " " + release.Tag + " " + release.Tagger,
			Setup:       service.ZeropsSetup,
			Gate:        gate,
		}
		targets = append(targets, target)
	}
	return targets, problems, nil
}

// gateOf is the stage environments.yaml says a production commit must already
// be live on (`requireOnStage`), or nil. A gate that names no declared stage is
// reported and read as absent: the declaration is a person's to fix, and the
// release verdict already stands between a tag and production.
func (r *Resolver) gateOf(plan Plan, env environments.Environment) *Gate {
	if env.RequireOnStage == "" {
		return nil
	}
	named, found := plan.File.Environment(env.RequireOnStage)
	if !found || named.Tier != environments.TierStage {
		r.log().Warn("an environment gates on a stage that is not declared",
			"environment", env.Name, "gate", env.RequireOnStage)
		return nil
	}
	return &Gate{Environment: named.Name, Project: named.Project}
}

// reportMerge writes the conflict where a person sees it: on the head of the
// first source, in the repository the sources live in.
func (r *Resolver) reportMerge(ctx context.Context, owner, repo string, env environments.Environment, cause error) {
	head, err := r.Gitea.Branch(ctx, owner, repo, env.Sources[0])
	if err != nil {
		return
	}
	message := cause.Error()
	if len(message) > 240 {
		message = message[:237] + "..."
	}
	if _, err := r.Gitea.CreateStatus(ctx, owner, repo, head.Commit.ID, gitea.NewStatus{
		Context: MergeContext(env.Name), State: "failure", Description: message,
	}); err != nil {
		r.log().Warn("a merge conflict could not be reported", "environment", env.Name, "err", err.Error())
	}
}
