// Package environments reads the two files a group's deploys are decided from:
// `environments.yaml` and a tier's `import.yaml`, both on `main` of
// `{slug}/group`.
//
// docs/group-repo.md is the contract. Parsing is pure and total — nothing here
// talks to a network — and it refuses rather than guesses: a production
// environment whose sources are not `release`, a name used twice, a tier that
// is neither stage nor production, all come back as an error naming the line.
// An environment the broker cannot read does not deploy, and one it read
// wrongly would deploy the wrong thing.
package environments

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier is which of the group repo's tiers built an environment.
type Tier string

// The two tiers an environment may name.
const (
	TierStage      Tier = "stage"
	TierProduction Tier = "production"
)

// Deploy is when an environment deploys.
type Deploy string

// The two triggers an environment may declare.
const (
	// OnPush deploys on every push to a source branch. It is the default.
	OnPush Deploy = "on-push"
	// OnRequest deploys only when a workflow asks through POST /deploy.
	OnRequest Deploy = "on-request"
)

// SourceRelease is the one value a production environment's sources may take:
// the newest approved release tag of the group repo, and nothing else.
const SourceRelease = "release"

// File is a parsed environments.yaml.
type File struct {
	Version      int
	Environments []Environment
}

// Environment is one deploy target of a group.
type Environment struct {
	// Name is what a workflow's deploy step asks for.
	Name string
	// Tier says which tier's import.yaml created it.
	Tier Tier
	// Project is the Zerops project.
	Project string
	// Sources are the branches of every service repository that feed a stage.
	// A production environment has none: its Release is true instead.
	Sources []string
	// Release marks `sources: release` — the newest approved v* tag.
	Release bool
	// Deploy is on-push or on-request.
	Deploy Deploy
	// RequireOnStage is the optional gate: every listed commit must already be
	// live on this stage.
	RequireOnStage string
}

// Branch is the branch the broker realises a multi-source stage on. A single
// source deploys that branch's head directly; several are merged into this one
// in a throwaway working copy (docs/group-repo.md).
func (e Environment) Branch() string {
	if len(e.Sources) == 1 {
		return e.Sources[0]
	}
	return "env/" + e.Name
}

// Environment finds one by name.
func (f File) Environment(name string) (Environment, bool) {
	for _, e := range f.Environments {
		if e.Name == name {
			return e, true
		}
	}
	return Environment{}, false
}

// OfTier returns every environment built from one tier, in file order.
func (f File) OfTier(tier Tier) []Environment {
	var out []Environment
	for _, e := range f.Environments {
		if e.Tier == tier {
			out = append(out, e)
		}
	}
	return out
}

// document is the wire shape. environments is kept as a node so a duplicate
// name is an error rather than a silent last-wins.
type document struct {
	Version      int       `yaml:"version"`
	Environments yaml.Node `yaml:"environments"`
}

type entry struct {
	Tier    string    `yaml:"tier"`
	Project string    `yaml:"project"`
	Sources yaml.Node `yaml:"sources"`
	Deploy  string    `yaml:"deploy"`
	Gates   struct {
		RequireOnStage string `yaml:"requireOnStage"`
	} `yaml:"gates"`
}

// Parse reads an environments.yaml. Every rule docs/group-repo.md states is
// enforced here, and the first one broken is the error.
func Parse(raw []byte) (File, error) {
	var doc document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return File{}, fmt.Errorf("environments.yaml: %w", err)
	}
	if doc.Version != 1 {
		return File{}, fmt.Errorf("environments.yaml: version is %d, and this broker reads version 1", doc.Version)
	}
	if doc.Environments.Kind == 0 {
		return File{}, fmt.Errorf("environments.yaml: no environments")
	}
	if doc.Environments.Kind != yaml.MappingNode {
		return File{}, fmt.Errorf("environments.yaml: environments is a mapping of name to environment")
	}

	out := File{Version: doc.Version}
	seen := map[string]bool{}
	for i := 0; i+1 < len(doc.Environments.Content); i += 2 {
		name := doc.Environments.Content[i].Value
		if name == "" {
			return File{}, fmt.Errorf("environments.yaml: an environment with no name")
		}
		if seen[name] {
			return File{}, fmt.Errorf("environments.yaml: %s is declared twice", name)
		}
		seen[name] = true

		var body entry
		if err := doc.Environments.Content[i+1].Decode(&body); err != nil {
			return File{}, fmt.Errorf("environments.yaml: %s: %w", name, err)
		}
		env, err := build(name, body)
		if err != nil {
			return File{}, err
		}
		out.Environments = append(out.Environments, env)
	}
	if len(out.Environments) == 0 {
		return File{}, fmt.Errorf("environments.yaml: no environments")
	}
	return out, nil
}

func build(name string, body entry) (Environment, error) {
	env := Environment{Name: name, Tier: Tier(body.Tier), Project: body.Project,
		Deploy: Deploy(body.Deploy), RequireOnStage: body.Gates.RequireOnStage}

	switch env.Tier {
	case TierStage, TierProduction:
	default:
		return Environment{}, fmt.Errorf("environments.yaml: %s: tier is %q, and a tier is stage or production", name, body.Tier)
	}
	if env.Project == "" {
		return Environment{}, fmt.Errorf("environments.yaml: %s: no project", name)
	}
	switch env.Deploy {
	case "":
		env.Deploy = OnPush
	case OnPush, OnRequest:
	default:
		return Environment{}, fmt.Errorf("environments.yaml: %s: deploy is %q, and a deploy is on-push or on-request", name, body.Deploy)
	}

	release, sources, err := readSources(name, body.Sources)
	if err != nil {
		return Environment{}, err
	}
	env.Release, env.Sources = release, sources

	// The rule that matters: production deploys only what an approved release
	// tag lists, so `sources: release` is the only value it may have — and a
	// stage may not claim it, because nothing approves a stage.
	if env.Tier == TierProduction && !env.Release {
		return Environment{}, fmt.Errorf("environments.yaml: %s: a production environment's sources are `release`, and nothing else", name)
	}
	if env.Tier == TierStage && env.Release {
		return Environment{}, fmt.Errorf("environments.yaml: %s: `sources: release` belongs to a production environment", name)
	}
	if env.Tier == TierStage && len(env.Sources) == 0 {
		return Environment{}, fmt.Errorf("environments.yaml: %s: a stage names at least one source branch", name)
	}
	return env, nil
}

// readSources takes either the scalar `release` or a list of branches.
func readSources(name string, node yaml.Node) (bool, []string, error) {
	switch node.Kind {
	case 0:
		return false, nil, nil
	case yaml.ScalarNode:
		if node.Value == SourceRelease {
			return true, nil, nil
		}
		return false, nil, fmt.Errorf("environments.yaml: %s: sources is `release` or a list of branches, not %q", name, node.Value)
	case yaml.SequenceNode:
		var branches []string
		if err := node.Decode(&branches); err != nil {
			return false, nil, fmt.Errorf("environments.yaml: %s: sources: %w", name, err)
		}
		seen := map[string]bool{}
		for _, b := range branches {
			if strings.TrimSpace(b) == "" {
				return false, nil, fmt.Errorf("environments.yaml: %s: an empty source branch", name)
			}
			if b == SourceRelease {
				return false, nil, fmt.Errorf("environments.yaml: %s: `release` is not a branch", name)
			}
			if seen[b] {
				return false, nil, fmt.Errorf("environments.yaml: %s: the source %s is listed twice", name, b)
			}
			seen[b] = true
		}
		return false, branches, nil
	default:
		return false, nil, fmt.Errorf("environments.yaml: %s: sources is `release` or a list of branches", name)
	}
}

// ---------------------------------------------------------------------------
// The tier's import.yaml
// ---------------------------------------------------------------------------

// Recipe is a tier's import.yaml: the services the tier creates, in the order
// the platform builds them.
type Recipe struct {
	Services []RecipeService
}

// RecipeService is one service of a tier. Raw is the whole mapping as written,
// which is what a recipe delta re-imports after converting the two git fields.
type RecipeService struct {
	Hostname string
	// BuildFromGit is the canonical clone URL of the service's repository —
	// the pair that maps a hostname to `{slug}/{name}`.
	BuildFromGit string
	// ZeropsSetup is the setup block of that repository's zerops.yaml a
	// build-and-deploy names.
	ZeropsSetup string
	// Priority is the platform's: higher is built first, which is the order a
	// deploy of a whole environment follows.
	Priority int
	// Raw is the service's mapping node, kept so the delta import can re-emit
	// it with only the two git fields changed.
	Raw *yaml.Node
}

// Repository is the `{owner}/{name}` a service's buildFromGit names. A service
// with no buildFromGit — a managed database, a service already converted to
// startWithoutCode — has none.
func (s RecipeService) Repository() (owner, name string, ok bool) {
	url := strings.TrimSuffix(strings.TrimSpace(s.BuildFromGit), "/")
	if url == "" {
		return "", "", false
	}
	// A canonical clone URL never carries a .git suffix (the platform's clone
	// preflight fails on one), but a hand-written recipe might.
	url = strings.TrimSuffix(url, ".git")
	parts := strings.Split(url, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	owner, name = parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

// Runtime reports whether a service is one the broker deploys: it names both a
// repository and a setup. Everything else in a tier — a database, a storage —
// is created by the import and never deployed to.
func (s RecipeService) Runtime() bool {
	_, _, ok := s.Repository()
	return ok && s.ZeropsSetup != ""
}

// ParseRecipe reads a tier's import.yaml.
func ParseRecipe(raw []byte) (Recipe, error) {
	var doc struct {
		Services []yaml.Node `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Recipe{}, fmt.Errorf("import.yaml: %w", err)
	}
	var out Recipe
	seen := map[string]bool{}
	for i := range doc.Services {
		node := doc.Services[i]
		var body struct {
			Hostname     string `yaml:"hostname"`
			BuildFromGit string `yaml:"buildFromGit"`
			ZeropsSetup  string `yaml:"zeropsSetup"`
			Priority     int    `yaml:"priority"`
		}
		if err := node.Decode(&body); err != nil {
			return Recipe{}, fmt.Errorf("import.yaml: service %d: %w", i+1, err)
		}
		if body.Hostname == "" {
			return Recipe{}, fmt.Errorf("import.yaml: service %d has no hostname", i+1)
		}
		if seen[body.Hostname] {
			return Recipe{}, fmt.Errorf("import.yaml: the hostname %s is used twice", body.Hostname)
		}
		seen[body.Hostname] = true
		copied := node
		out.Services = append(out.Services, RecipeService{
			Hostname: body.Hostname, BuildFromGit: body.BuildFromGit,
			ZeropsSetup: body.ZeropsSetup, Priority: body.Priority, Raw: &copied,
		})
	}
	if len(out.Services) == 0 {
		return Recipe{}, fmt.Errorf("import.yaml: no services")
	}
	return out, nil
}

// Service finds one service by hostname.
func (r Recipe) Service(hostname string) (RecipeService, bool) {
	for _, s := range r.Services {
		if s.Hostname == hostname {
			return s, true
		}
	}
	return RecipeService{}, false
}

// Order is the tier's priority order: the platform builds a higher priority
// first, and ties keep the file's order. A deploy of a whole environment walks
// this list.
func (r Recipe) Order() []RecipeService {
	out := append([]RecipeService(nil), r.Services...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// Runtimes is [Recipe.Order] with only the services the broker deploys.
func (r Recipe) Runtimes() []RecipeService {
	var out []RecipeService
	for _, s := range r.Order() {
		if s.Runtime() {
			out = append(out, s)
		}
	}
	return out
}
