package environments

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// MainBranch is the group repo's protected branch: the one the broker reads
// both files from, and the only ref a recipe or a declaration is taken from.
const MainBranch = "main"

// EnvironmentsFile is where the declarations live.
const EnvironmentsFile = "environments.yaml"

// RecipeFile is what each tier directory holds.
const RecipeFile = "import.yaml"

// Read reads and parses `environments.yaml` from the group repo's `main`.
// A group that has declared none yet answers an empty File and no error — that
// is a group with no stage, not a group the broker could not read.
func Read(ctx context.Context, g *gitea.Client, owner, repo string) (File, error) {
	raw, err := g.File(ctx, owner, repo, EnvironmentsFile, MainBranch)
	if gitea.IsNotFound(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("%s/%s: %w", owner, repo, err)
	}
	return Parse(raw)
}

// TierDir finds a tier's directory on the group repo's `main`. The published
// layout numbers them — `3 — Stage`, `4 — Small Production` — and the number
// is part of the name, so the directory is found by its title, never guessed
// by index.
func TierDir(ctx context.Context, g *gitea.Client, owner, repo string, tier Tier) (string, error) {
	entries, err := g.Dir(ctx, owner, repo, "", MainBranch)
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", owner, repo, err)
	}
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		if tierOfDir(e.Name) == tier {
			return e.Name, nil
		}
	}
	return "", fmt.Errorf("%s/%s has no %s tier directory", owner, repo, tier)
}

// tierOfDir reads a tier out of a published directory name: the index and the
// em dash are dropped, and what is left names the tier.
func tierOfDir(name string) Tier {
	title := name
	if _, rest, ok := strings.Cut(name, "—"); ok {
		title = rest
	}
	title = strings.ToLower(strings.TrimSpace(title))
	switch {
	case title == "stage":
		return TierStage
	case strings.HasSuffix(title, "production"):
		return TierProduction
	default:
		return ""
	}
}

// ReadRecipe reads a tier's import.yaml from the group repo's `main` and
// returns it with the directory it came from — the path a recipe delta
// compares against the sha the last pass saw.
func ReadRecipe(ctx context.Context, g *gitea.Client, owner, repo string, tier Tier) (Recipe, string, error) {
	dir, err := TierDir(ctx, g, owner, repo, tier)
	if err != nil {
		return Recipe{}, "", err
	}
	path := dir + "/" + RecipeFile
	raw, err := g.File(ctx, owner, repo, path, MainBranch)
	if err != nil {
		return Recipe{}, path, fmt.Errorf("%s/%s:%s: %w", owner, repo, path, err)
	}
	recipe, err := ParseRecipe(raw)
	if err != nil {
		return Recipe{}, path, fmt.Errorf("%s/%s:%s: %w", owner, repo, path, err)
	}
	return recipe, path, nil
}
