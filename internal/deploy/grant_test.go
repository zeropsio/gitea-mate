package deploy_test

import (
	"testing"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
)

func TestTokenVariable(t *testing.T) {
	t.Parallel()
	// The app spells the same name (deployToken.ts); the two are pinned to one
	// vector so neither can drift.
	got := deploy.TokenVariable("hadSu0iZ-uCG_Ic1hicN4Q")
	want := "MATE_DEPLOY_TOKEN_686164537530695A2D7543475F4963316869634E3451"
	if got != want {
		t.Fatalf("TokenVariable = %q, want %q", got, want)
	}
}

func TestTrustedRun(t *testing.T) {
	t.Parallel()
	repo := gitea.Repo{FullName: "acme/api", DefaultBranch: "main"}
	tests := []struct {
		name string
		run  gitea.Run
		want bool
	}{
		{"a push to the default branch", gitea.Run{Event: "push", HeadBranch: "main", Repository: repo, HeadRepository: repo}, true},
		{"a dispatch on the default branch", gitea.Run{Event: "workflow_dispatch", HeadBranch: "main", Repository: repo}, true},
		{"a Mate's branch with a workflow of its own", gitea.Run{Event: "push", HeadBranch: "mate/mate-p1", Repository: repo, HeadRepository: repo}, false},
		{"a pull request", gitea.Run{Event: "pull_request", HeadBranch: "main", Repository: repo, HeadRepository: repo}, false},
		{"a fork whose branch is called main", gitea.Run{Event: "push", HeadBranch: "main", Repository: repo, HeadRepository: gitea.Repo{FullName: "mate-p1/api", DefaultBranch: "main"}}, false},
		{"a tag", gitea.Run{Event: "push", HeadBranch: "v1.0.0", Repository: repo, HeadRepository: repo}, false},
		{"a run that names no default branch", gitea.Run{Event: "push", HeadBranch: "main"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deploy.TrustedRun(tt.run); got != tt.want {
				t.Fatalf("TrustedRun = %v, want %v", got, tt.want)
			}
		})
	}
}
