package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
)

// The webhook side. Every handler here runs after the broker has already
// answered 204 — the signature was the only thing trusted, and nothing in a
// payload is taken as fact: a push names a branch and the broker reads the
// branch, a tag names a pusher and the broker re-checks that pusher in Zerops.

// pushPayload is the part of a push delivery the broker reads. The commits are
// deliberately not: the head of the branch is read from Gitea.
type pushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// createPayload is a branch or tag creation. Gitea sends only these five keys.
//
// SHA is deliberately not used to address the commit: for an annotated tag it
// is the peeled commit when the tag was created through
// POST /repos/{o}/{r}/tags, and the tag object's sha when the tag was pushed by
// git (measured on 1.27.2, 2026-09-16). The broker resolves the ref instead,
// which answers the same commit either way.
type createPayload struct {
	Ref     string `json:"ref"`
	RefType string `json:"ref_type"`
	SHA     string `json:"sha"`
	Repo    struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login    string `json:"login"`
		UserName string `json:"username"`
	} `json:"sender"`
}

func (c createPayload) pusher() string {
	if c.Sender.Login != "" {
		return c.Sender.Login
	}
	return c.Sender.UserName
}

// Push is a push to any branch of a group's org.
func (p *Pipeline) Push(ctx context.Context, org string, payload []byte) error {
	var body pushPayload
	if err := json.Unmarshal(payload, &body); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	branch, isBranch := strings.CutPrefix(body.Ref, "refs/heads/")
	if !isBranch {
		return nil
	}
	// `env/*` is what the broker itself wrote when it merged a mixed stage;
	// reacting to it would be reacting to its own footsteps.
	if strings.HasPrefix(branch, "env/") {
		return nil
	}

	owner, repo, err := fullName(body.Repository.FullName)
	if err != nil {
		return err
	}
	state, err := p.State(ctx)
	if err != nil {
		return err
	}
	group, known := state.Registry.Group(org)
	if !known {
		return nil
	}
	plan, err := p.Plan(ctx, group.Slug)
	if err != nil {
		return err
	}

	// A push to the group repo changes the recipe or the declarations, not a
	// service's code.
	if repo == registry.GroupRepo {
		if branch == environments.MainBranch {
			return p.reconcileRecipes(ctx, plan)
		}
		return nil
	}

	for _, env := range plan.File.Environments {
		if env.Deploy != environments.OnPush || env.Release {
			continue
		}
		if !feeds(env, branch) {
			continue
		}
		recipe, has := plan.Recipes[env.Tier]
		if !has {
			continue
		}
		service, found := ServiceOfRepository(recipe, owner, repo)
		if !found {
			continue
		}
		if err := p.Deploy(ctx, plan, env, service.Hostname, nil); err != nil {
			p.log().Warn("a push could not be deployed",
				"group", group.Slug, "environment", env.Name, "err", err.Error())
		}
	}
	return nil
}

// feeds reports whether a branch is one of an environment's sources.
func feeds(env environments.Environment, branch string) bool {
	for _, source := range env.Sources {
		if source == branch {
			return true
		}
	}
	return false
}

// Create is a branch or tag created. A `v*` tag on the group repo is a
// release, and the only thing this handler judges.
func (p *Pipeline) Create(ctx context.Context, org string, payload []byte) error {
	var body createPayload
	if err := json.Unmarshal(payload, &body); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if body.RefType != "tag" || !strings.HasPrefix(body.Ref, deploy.TagPrefix) {
		return nil
	}
	owner, repo, err := fullName(body.Repo.FullName)
	if err != nil {
		return err
	}
	if repo != registry.GroupRepo {
		return nil
	}
	// The verdict has to land on the commit, and a payload's sha is one of two
	// things depending on how the tag was made, so the ref is peeled instead.
	commit, err := p.tagCommit(ctx, owner, repo, body)
	if err != nil {
		return err
	}

	// A re-delivered webhook for a tag that already has a verdict changes
	// nothing: a refused tag stays refused, for ever.
	if _, judged, err := deploy.Judged(ctx, p.Gitea, owner, repo, commit, body.Ref); err != nil {
		return err
	} else if judged {
		p.log().Info("a release was already judged", "group", org, "tag", body.Ref)
		return nil
	}

	state, err := p.State(ctx)
	if err != nil {
		return err
	}
	group, known := state.Registry.Group(org)
	if !known {
		return fmt.Errorf("the org %s is not a registered group", org)
	}

	approved, why := MayRelease(state, group, body.pusher())
	verdict := deploy.ReleaseRefused
	if approved {
		verdict = deploy.ReleaseApproved
	}
	if _, err := p.Gitea.CreateStatus(ctx, owner, repo, commit, gitea.NewStatus{
		Context: deploy.ReleaseContext(body.Ref), State: verdict, Description: why,
	}); err != nil {
		return fmt.Errorf("the verdict on %s could not be written: %w", body.Ref, err)
	}
	p.log().Info("a release was judged", "group", group.Slug, "tag", body.Ref, "verdict", verdict, "why", why)
	if !approved {
		return nil
	}

	plan, err := p.Plan(ctx, group.Slug)
	if err != nil {
		return err
	}
	for _, env := range plan.File.OfTier(environments.TierProduction) {
		if err := p.Deploy(ctx, plan, env, "", nil); err != nil {
			p.log().Warn("an approved release could not be deployed",
				"group", group.Slug, "environment", env.Name, "err", err.Error())
		}
	}
	return nil
}

// tagCommit peels the tag to the commit the verdict belongs on. The ref is the
// authority; the payload's sha is only the fallback for a tag whose ref has
// already moved or gone, and it is peeled too — it may itself be a tag object.
func (p *Pipeline) tagCommit(ctx context.Context, owner, repo string, body createPayload) (string, error) {
	tag := strings.TrimPrefix(body.Ref, "refs/tags/")
	commit, err := p.Gitea.TagCommit(ctx, owner, repo, tag)
	if err == nil {
		return commit, nil
	}
	if body.SHA == "" {
		return "", fmt.Errorf("the tag %s could not be resolved and names no commit: %w", body.Ref, err)
	}
	p.log().Warn("a release tag's ref could not be read, so its payload sha is peeled instead",
		"group", owner, "tag", body.Ref, "err", err.Error())

	peeled, peelErr := p.Gitea.PeelToCommit(ctx, owner, repo, gitea.GitObject{Type: "tag", SHA: body.SHA})
	if peelErr == nil {
		return peeled, nil
	}
	if gitea.IsNotFound(peelErr) {
		// No tag object behind it, so the payload's sha is the commit.
		return body.SHA, nil
	}
	return "", peelErr
}

// MayRelease re-checks a tag's pusher in Zerops — the mirror lags a role
// change by minutes, so Gitea's team is not the last word. It answers whether
// the tag is approved and the description the verdict carries, which names the
// pusher either way.
func MayRelease(state mirror.State, group registry.Group, pusher string) (bool, string) {
	if pusher == "" {
		return false, "the delivery names no pusher"
	}

	// A Mate's bot: mate-{projectId}. D8 — a bot releases only while the
	// group's switch is on, and only for its own group.
	if projectID, isBot := strings.CutPrefix(pusher, "mate-"); isBot && projectID != "" {
		owning, kind, registered := state.Registry.GroupOfProject(projectID)
		switch {
		case !registered || kind != roles.KindMate:
			return false, "refused: " + pusher + " is not a registered Mate"
		case owning.ID != group.ID:
			return false, "refused: " + pusher + " belongs to another group"
		case !group.MatesMayRelease:
			return false, "refused: " + pusher + " is a Mate, and this group's Mate release switch is off"
		}
		return true, "approved: " + pusher + ", by this group's Mate release switch"
	}

	// A person: their Gitea login is derived from their Zerops user id, so the
	// id is found by deriving every member's login and matching.
	for _, member := range state.Members {
		if roles.Login(member.UserID) != pusher {
			continue
		}
		rights, found := state.RightsFor(member.UserID)
		if !found {
			return false, "refused: " + pusher + " is not in the organisation's member list"
		}
		if !rights.Groups[group.Slug].Release {
			return false, "refused: " + pusher + " does not hold production rights on " + group.Slug
		}
		return true, "approved: " + pusher
	}
	return false, "refused: " + pusher + " is not a person in this Zerops organisation"
}

// PullRequest is a pull request opened, closed or merged. A merge into the
// group repo's `main` is a recipe or a declaration change; a Mate's bot
// opening one there is a recipe proposal the rights loop merges (D23), so the
// loop is nudged rather than left to its next tick.
func (p *Pipeline) PullRequest(ctx context.Context, org string, payload []byte) error {
	var body struct {
		Action      string `json:"action"`
		PullRequest struct {
			Merged bool `json:"merged"`
			Base   struct {
				Ref string `json:"ref"`
			} `json:"base"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return fmt.Errorf("pull_request: %w", err)
	}
	if body.PullRequest.Base.Ref != environments.MainBranch {
		return nil
	}
	_, repo, err := fullName(body.Repository.FullName)
	if err != nil {
		return err
	}
	if repo != registry.GroupRepo {
		return nil
	}
	if !body.PullRequest.Merged {
		if p.Nudge != nil && strings.HasPrefix(body.PullRequest.User.Login, "mate-") {
			p.Nudge()
		}
		return nil
	}

	state, err := p.State(ctx)
	if err != nil {
		return err
	}
	group, known := state.Registry.Group(org)
	if !known {
		return nil
	}
	plan, err := p.Plan(ctx, group.Slug)
	if err != nil {
		return err
	}
	return p.reconcileRecipes(ctx, plan)
}

// Other is every event the deploy side does not claim. The runner pool takes
// workflow_job in the server; everything else is ignored on purpose.
func (p *Pipeline) Other(ctx context.Context, org, event string, payload []byte) error {
	return nil
}
