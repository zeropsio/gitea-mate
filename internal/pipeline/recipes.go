package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// ---------------------------------------------------------------------------
// Recipe deltas
// ---------------------------------------------------------------------------

// recipes imports what a changed tier added, into every environment built from
// it. It runs before the deploys of the same pass, so a new service exists
// before anything is deployed to it.
func (p *Pipeline) recipes(ctx context.Context, plan deploy.Plan) (int, []string) {
	var problems []string
	imported := 0

	for tier := range plan.Recipes {
		key := plan.Slug + "/" + string(tier)
		sha, err := p.recipeSha(ctx, plan.Slug, tier)
		if err != nil {
			problems = append(problems, key+": "+err.Error())
			continue
		}

		blocks := plan.Recipes[tier].Blocks()

		p.mu.Lock()
		if p.seenRecipe == nil {
			p.seenRecipe = map[string]string{}
			p.seenBlocks = map[string]map[string]string{}
			p.reconciled = map[string]bool{}
		}
		previous, seen := p.seenRecipe[key]
		previousBlocks := p.seenBlocks[key]
		first := !p.reconciled[key]
		p.seenRecipe[key] = sha
		p.seenBlocks[key] = blocks
		p.reconciled[key] = true
		p.mu.Unlock()

		// A restart forgets what it saw, so the first pass after one only
		// reports the differences and takes the current recipe as seen; a
		// later change is what imports.
		if first || !seen {
			for _, problem := range p.report(ctx, plan, tier) {
				problems = append(problems, key+": "+problem)
			}
			continue
		}
		if previous == sha {
			continue
		}

		p.log().Info("a tier's recipe changed", "group", plan.Slug, "tier", tier)
		count, tierProblems := p.importDelta(ctx, plan, tier, previousBlocks, blocks)
		imported += count
		for _, problem := range tierProblems {
			problems = append(problems, key+": "+problem)
		}
	}
	return imported, problems
}

// recipeSha is the blob sha of a tier's import.yaml on `main` — what a pass
// compares against the one it last saw.
func (p *Pipeline) recipeSha(ctx context.Context, slug string, tier environments.Tier) (string, error) {
	dir, err := environments.TierDir(ctx, p.Gitea, slug, registry.GroupRepo, tier)
	if err != nil {
		return "", err
	}
	entries, err := p.Gitea.Dir(ctx, slug, registry.GroupRepo, dir, environments.MainBranch)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name == environments.RecipeFile {
			return entry.SHA, nil
		}
	}
	return "", fmt.Errorf("%s carries no %s", dir, environments.RecipeFile)
}

// importDelta imports every service a tier declares that an environment of it
// does not have, converted to startWithoutCode. A service the project has and
// the recipe no longer does is reported, never deleted.
func (p *Pipeline) importDelta(ctx context.Context, plan deploy.Plan, tier environments.Tier, before, now map[string]string) (int, []string) {
	recipe := plan.Recipes[tier]
	var problems []string
	imported := 0

	for _, env := range plan.File.OfTier(tier) {
		services, err := p.Zerops.Services(ctx, p.ClientID, env.Project)
		if err != nil {
			problems = append(problems, env.Name+": the project's services: "+err.Error())
			continue
		}
		services = zerops.WithoutSystem(services)
		present := map[string]bool{}
		for _, service := range services {
			present[service.Name] = true
		}

		var missing []environments.RecipeService
		declared := map[string]bool{}
		for _, service := range recipe.Order() {
			declared[service.Hostname] = true
			if !present[service.Hostname] {
				missing = append(missing, service)
			}
		}
		for _, service := range services {
			if !declared[service.Name] {
				problems = append(problems, fmt.Sprintf("%s: %s is in the project and no longer in the recipe; the broker never deletes a service", env.Name, service.Name))
				continue
			}
			// A service the project already has keeps whatever it was created
			// with: a re-import with `override` restarts it and its semantics
			// are unmeasured, so a changed declaration is reported and left to
			// a person (docs/group-repo.md, "Recipe deltas").
			if was, had := before[service.Name]; had && was != now[service.Name] {
				problems = append(problems, fmt.Sprintf("%s: the recipe for %s changed — scaling and shape are reported, never applied to a service that exists", env.Name, service.Name))
			}
		}
		if len(missing) == 0 {
			continue
		}

		document, err := environments.StartWithoutCode(missing)
		if err != nil {
			problems = append(problems, env.Name+": "+err.Error())
			continue
		}
		if _, err := p.Zerops.ImportServices(ctx, env.Project, document); err != nil {
			problems = append(problems, env.Name+": the delta could not be imported: "+err.Error())
			continue
		}
		imported += len(missing)
		p.log().Info("a recipe delta was imported",
			"group", plan.Slug, "environment", env.Name, "services", len(missing))
	}
	return imported, problems
}

// report says what a delta would do without doing it — the one pass after a
// restart, which must not act on a recipe it has never seen before.
func (p *Pipeline) report(ctx context.Context, plan deploy.Plan, tier environments.Tier) []string {
	recipe := plan.Recipes[tier]
	var problems []string
	for _, env := range plan.File.OfTier(tier) {
		services, err := p.Zerops.Services(ctx, p.ClientID, env.Project)
		if err != nil {
			problems = append(problems, env.Name+": the project's services: "+err.Error())
			continue
		}
		services = zerops.WithoutSystem(services)
		present := map[string]bool{}
		for _, service := range services {
			present[service.Name] = true
		}
		var missing, orphans []string
		declared := map[string]bool{}
		for _, service := range recipe.Order() {
			declared[service.Hostname] = true
			if !present[service.Hostname] {
				missing = append(missing, service.Hostname)
			}
		}
		for _, service := range services {
			if !declared[service.Name] {
				orphans = append(orphans, service.Name)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("%s: the recipe declares %s, which the project does not have (reported, not imported: this is the first pass since a restart)", env.Name, strings.Join(missing, ", ")))
		}
		if len(orphans) > 0 {
			problems = append(problems, fmt.Sprintf("%s: the project has %s, which the recipe no longer declares; the broker never deletes a service", env.Name, strings.Join(orphans, ", ")))
		}
	}
	return problems
}

// reconcileRecipes takes the recipe half of a pass on its own, for the two
// webhooks that mean "main of the group repo moved".
func (p *Pipeline) reconcileRecipes(ctx context.Context, plan deploy.Plan) error {
	if len(plan.File.Environments) == 0 {
		return nil
	}
	_, problems := p.recipes(ctx, plan)
	for _, problem := range problems {
		p.log().Warn("a recipe could not be reconciled", "group", plan.Slug, "problem", problem)
	}
	// The declarations may have moved too, so every environment is re-checked.
	return p.catchUpAll(ctx, plan)
}
