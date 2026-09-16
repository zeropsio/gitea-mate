package deploy_test

import (
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
)

func TestChooseBetweenPromoteAndArchive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		option deploy.PromoteOption
		want   deploy.Choice
	}{
		{
			"production, a stage version of this commit, one build section: promote",
			deploy.PromoteOption{Tier: environments.TierProduction, StageVersionID: "ver-1", SameBuild: true},
			deploy.Promote,
		},
		{
			"a stage is where a commit is built in the first place",
			deploy.PromoteOption{Tier: environments.TierStage, StageVersionID: "ver-1", SameBuild: true},
			deploy.Archive,
		},
		{
			"a commit that never reached a stage is built",
			deploy.PromoteOption{Tier: environments.TierProduction, SameBuild: true},
			deploy.Archive,
		},
		{
			"two setups that build differently are not one artifact",
			deploy.PromoteOption{Tier: environments.TierProduction, StageVersionID: "ver-1"},
			deploy.Archive,
		},
		{
			"nothing known at all is an archive",
			deploy.PromoteOption{},
			deploy.Archive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.option.Choose(); got != tc.want {
				t.Fatalf("Choose() = %q, want %q", got, tc.want)
			}
		})
	}
}

const twoSetups = `
zerops:
  - setup: api
    build:
      base: nodejs@22
      buildCommands:
        - npm ci
        - npm run build
      deployFiles: ./
    run:
      base: nodejs@22
      start: npm start

  - setup: api-prod
    build:
      buildCommands:
        - npm ci
        - npm run build
      base: nodejs@22
      deployFiles: ./
    run:
      base: nodejs@22
      start: npm start
      envVariables:
        NODE_ENV: production

  - setup: web
    build:
      base: nodejs@22
      buildCommands:
        - npm ci
      deployFiles: ./dist

  - setup: nobuild
    run:
      base: static
`

func TestSameBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		a    string
		b    string
		want bool
		err  string
	}{
		{
			name: "one setup is trivially itself",
			a:    "api", b: "api", want: true,
		},
		{
			name: "a production setup that only runs differently shares its build",
			a:    "api", b: "api-prod", want: true,
		},
		{
			name: "keys written in another order are still the same build",
			a:    "api-prod", b: "api", want: true,
		},
		{
			name: "a different build command is a different build",
			a:    "api", b: "web", want: false,
		},
		{
			name: "a setup with no build section differs from one that has one",
			a:    "api", b: "nobuild", want: false,
		},
		{
			name: "a setup the repository does not carry is said so, not assumed",
			a:    "api", b: "ghost", err: "no setup \"ghost\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deploy.SameBuild([]byte(twoSetups), tc.a, tc.b)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("SameBuild = %v, %v, want an error mentioning %q", got, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SameBuild: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SameBuild(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSameBuildRefusesADocumentItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"not yaml at all", "zerops: [\n", "zerops.yaml:"},
		{"no setups", "zerops: []\n", "carries no setups"},
		{"a setup with no name", "zerops:\n  - build: {base: x}\n", "a setup with no name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := deploy.SameBuild([]byte(tc.body), "a", "b"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SameBuild = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestHasSetup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		setup string
		want  bool
	}{
		{"api", true},
		{"api-prod", true},
		{"ghost", false},
		{"", false},
	} {
		t.Run(tc.setup, func(t *testing.T) {
			if got := deploy.HasSetup([]byte(twoSetups), tc.setup); got != tc.want {
				t.Fatalf("HasSetup(%q) = %v, want %v", tc.setup, got, tc.want)
			}
		})
	}
}
