package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The grant (D27). A job deploys with `zcli push`; this is where the broker
// decides whether it may and hands it the environment's key. The order of the
// checks is the contract (docs/broker-api.md § POST /deploy/grant): nothing
// below the first refusal is read, and the key is the last thing touched.

// candidate is one environment a job could deploy, and the service of it the
// job's repository builds.
type candidate struct {
	env     environments.Environment
	service string
}

// Grant answers a proved job.
func (p *Pipeline) Grant(ctx context.Context, req deploy.GrantRequest) (deploy.Grant, error) {
	if req.Sha == "" {
		return deploy.Grant{}, deploy.Refuse(http.StatusBadRequest, "invalid_request", "a job says which commit it holds")
	}

	// The ref. A job's own token proved which run it belongs to; the run says
	// whose workflow file it executes.
	run, err := p.Gitea.GetRun(ctx, req.Owner, req.Repo, req.RunID)
	if err != nil {
		return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "the job's run could not be read")
	}
	if !deploy.TrustedRun(run) {
		return deploy.Grant{}, deploy.Refuse(http.StatusForbidden, "untrusted_ref",
			"only the default branch's own workflow deploys; this job was started by %q on %q", run.Event, run.HeadBranch)
	}

	state, err := p.State(ctx)
	if err != nil {
		return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "the account could not be read")
	}
	group, known := state.Registry.Group(req.Owner)
	if !known {
		return deploy.Grant{}, deploy.Refuse(http.StatusNotFound, "unknown_group", "%s is not a registered project", req.Owner)
	}
	plan, err := p.Plan(ctx, group.Slug)
	if err != nil {
		p.log().Error("a group's plan could not be read", "group", group.Slug, "err", err.Error())
		return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "the group's environments could not be read")
	}

	candidates, refusal := p.candidates(plan, req, run.Repository.DefaultBranch)
	if refusal != nil {
		return deploy.Grant{}, refusal
	}

	var waiting, superseded, live []string
	for _, c := range candidates {
		targets, problems, err := p.Resolver.Resolve(ctx, plan, c.env, c.service)
		for _, problem := range problems {
			p.log().Warn("an environment could not be fully resolved", "group", plan.Slug, "problem", problem)
		}
		switch {
		case errors.Is(err, deploy.ErrNoRelease):
			continue
		case err != nil:
			return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "%s could not be resolved", c.env.Name)
		}
		for _, target := range targets {
			// A job deploys its own repository's commit and no other's.
			if target.Owner != req.Owner || target.Repo != req.Repo {
				continue
			}
			where := c.env.Name + "/" + target.Service
			if target.Sha != req.Sha {
				superseded = append(superseded, fmt.Sprintf("%s wants %s", where, target.Sha))
				continue
			}
			service, err := p.serviceOf(ctx, c.env.Project, target.Service)
			if err != nil {
				return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "%s: %v", where, err)
			}
			if service.DeployedSha() == target.Sha {
				live = append(live, where)
				continue
			}
			if met, why := deploy.GateMet(ctx, p.Zerops, p.ClientID, target); !met {
				return deploy.Grant{}, deploy.Refuse(http.StatusConflict, "gate_not_met", "%s", why)
			}
			status, has, err := deploy.LatestStatus(ctx, p.Gitea, target, c.env.Name)
			if err != nil {
				return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "%s's statuses could not be read", where)
			}
			if has && status.State == "pending" && strings.HasPrefix(status.Description, deploy.DescriptionDeploying) &&
				p.now().Sub(status.CreatedAt) < p.patience() {
				waiting = append(waiting, where)
				continue
			}
			return p.handOver(ctx, req, group, c.env, target, service)
		}
	}

	switch {
	case len(waiting) > 0:
		return deploy.Grant{Status: deploy.GrantInProgress, Sha: req.Sha,
			Message: "another job is deploying this commit to " + strings.Join(waiting, ", ")}, nil
	case len(superseded) > 0:
		return deploy.Grant{Status: deploy.GrantSuperseded, Sha: req.Sha,
			Message: "a newer commit is wanted: " + strings.Join(superseded, ", ")}, nil
	case len(live) > 0:
		return deploy.Grant{Status: deploy.GrantLive, Sha: req.Sha,
			Message: "already live on " + strings.Join(live, ", ")}, nil
	}
	return deploy.Grant{Status: deploy.GrantNothing, Sha: req.Sha,
		Message: req.Owner + "/" + req.Repo + " feeds no environment yet"}, nil
}

// candidates is what a job could deploy: the environment it named, or — for a
// job started by a push, which names none — every environment its branch
// feeds on push.
func (p *Pipeline) candidates(plan deploy.Plan, req deploy.GrantRequest, branch string) ([]candidate, *deploy.Refusal) {
	var envs []environments.Environment
	if req.Environment != "" {
		env, refusal := environmentNamed(plan, req.Owner, req.Environment)
		if refusal != nil {
			return nil, refusal
		}
		envs = append(envs, env)
	} else {
		for _, env := range plan.File.Environments {
			if env.Deploy == environments.OnPush && !env.Release && feeds(env, branch) {
				envs = append(envs, env)
			}
		}
	}

	var out []candidate
	for _, env := range envs {
		recipe, has := plan.Recipes[env.Tier]
		if !has {
			if req.Environment != "" {
				return nil, deploy.Refuse(http.StatusBadGateway, "upstream", "the group's %s tier could not be read", env.Tier)
			}
			continue
		}
		found := false
		for _, service := range recipe.Runtimes() {
			owner, repo, ok := service.Repository()
			if !ok || owner != req.Owner || repo != req.Repo {
				continue
			}
			if req.Service != "" && service.Hostname != req.Service {
				continue
			}
			out = append(out, candidate{env: env, service: service.Hostname})
			found = true
		}
		if !found && req.Environment != "" {
			what := req.Owner + "/" + req.Repo
			if req.Service != "" {
				what = req.Service
			}
			return nil, deploy.Refuse(http.StatusNotFound, "unknown_service",
				"%s is not a runtime service of %s built from this repository", what, env.Name)
		}
	}
	return out, nil
}

// environmentNamed resolves what a job called an environment: a declared name,
// or a tier's name for the group's only environment of that tier — the
// workflow zcp writes names the tier, while the app names an environment after
// its group (`todo-stage`), measured 2026-09-17.
func environmentNamed(plan deploy.Plan, slug, name string) (environments.Environment, *deploy.Refusal) {
	if env, declared := plan.File.Environment(name); declared {
		return env, nil
	}
	switch ofTier := plan.File.OfTier(environments.Tier(name)); len(ofTier) {
	case 0:
		return environments.Environment{}, deploy.Refuse(http.StatusNotFound, "unknown_environment",
			"%s is not an environment of %s", name, slug)
	case 1:
		return ofTier[0], nil
	default:
		names := make([]string, 0, len(ofTier))
		for _, e := range ofTier {
			names = append(names, e.Name)
		}
		return environments.Environment{}, deploy.Refuse(http.StatusNotFound, "unknown_environment",
			"%s names %d environments of %s (%s); ask for one by name", name, len(ofTier), slug, strings.Join(names, ", "))
	}
}

// handOver is the last step: the runner, the key, the record, the status. The
// key is read only once everything else has said yes.
func (p *Pipeline) handOver(ctx context.Context, req deploy.GrantRequest, group registry.Group,
	env environments.Environment, target deploy.Target, service zerops.ServiceDetail) (deploy.Grant, error) {
	runner, trusted, why, err := p.runnerTrust(ctx, group.Slug)
	if err != nil {
		p.log().Warn("a runner's trust could not be read, so no key was handed over", "group", group.Slug, "err", err.Error())
		return deploy.Grant{}, deploy.Refuse(http.StatusServiceUnavailable, "runner_unknown", "the group's runner could not be read")
	}
	if !trusted {
		p.log().Warn("a group's runner ran unreviewed code; it is replaced before any key goes near it",
			"group", group.Slug, "why", why)
		deploy.WriteStatus(ctx, p.Gitea, p.log(), target, env.Name, "failure",
			"the runner ran a branch's own workflow and is being replaced; the deploy is started again on the new one")
		p.replaceRunner(group.Slug, runner)
		return deploy.Grant{}, deploy.Refuse(http.StatusServiceUnavailable, "runner_tainted",
			"%s; the runner is replaced and the deploy is started again", why)
	}

	token, err := p.deployToken(ctx, env.Project)
	if err != nil {
		return deploy.Grant{}, deploy.Refuse(http.StatusBadGateway, "upstream", "the environment's deploy token could not be read")
	}
	if token == "" {
		return deploy.Grant{}, deploy.Refuse(http.StatusFailedDependency, "no_deploy_token",
			"%s has no deploy token yet; an admin who opens the projects page in Zerops Mate mints it", env.Name)
	}

	record := p.Records.New(deploy.Record{
		Environment: env.Name, Service: target.Service, Sha: target.Sha,
		Repository: req.Owner + "/" + req.Repo, ProjectID: env.Project, ServiceID: service.ID,
	})
	deploy.WriteStatus(ctx, p.Gitea, p.log(), target, env.Name, "pending", deploy.DescriptionDeploying+" · job "+req.TaskID)
	p.log().Info("a deploy was granted", "group", group.Slug, "environment", env.Name,
		"service", target.Service, "sha", target.Sha, "job", req.TaskID, "run", strconv.FormatInt(req.RunID, 10))
	return deploy.Grant{
		ID: record.ID, Status: deploy.GrantGranted,
		Environment: env.Name, Service: target.Service, Sha: target.Sha,
		Token: token, ProjectID: env.Project, ServiceID: service.ID,
		Setup: target.Setup, VersionName: target.VersionName,
	}, nil
}

// Result is a job's report on a grant (POST /deploy/{id}/result). A report of
// success is checked against what the service runs: the status says what is
// true, not what a job said.
func (p *Pipeline) Result(ctx context.Context, id, repository, outcome, message string) error {
	record, known := p.Records.Get(id)
	if !known {
		return deploy.Refuse(http.StatusNotFound, "unknown_deploy", "the broker no longer holds %s; the next pass writes the commit's status", id)
	}
	if record.Repository != repository {
		return deploy.Refuse(http.StatusForbidden, "wrong_repository", "that deploy belongs to another repository")
	}
	owner, repo, _ := cut(record.Repository)
	target := deploy.Target{Service: record.Service, Owner: owner, Repo: repo, Sha: record.Sha}

	if outcome != "success" {
		if message == "" {
			message = "the job's zcli push failed"
		}
		deploy.WriteStatus(ctx, p.Gitea, p.log(), target, record.Environment, "failure", deploy.DescriptionFailed+": "+message)
		p.Records.Update(id, func(r *deploy.Record) { r.Status, r.Message = deploy.StatusFailed, message })
		return nil
	}

	service, err := p.Zerops.Service(ctx, record.ServiceID)
	if err != nil {
		return deploy.Refuse(http.StatusBadGateway, "upstream", "the service could not be read")
	}
	if service.Deploying() && zerops.VersionSha(service.DeployedName()) == record.Sha {
		// The platform names the reported commit's version but does not run
		// it yet: not a verdict. The status stays pending, and the pass writes
		// live once the service runs the commit. A service building another
		// commit has answered: it will not run this one.
		p.log().Info("a job reported success before its version went live",
			"environment", record.Environment, "service", record.Service, "sha", record.Sha)
		return nil
	}
	if service.DeployedSha() != record.Sha {
		why := fmt.Sprintf("the job reported success, but %s runs %q", record.Service, service.DeployedSha())
		if service.Deploying() {
			why = fmt.Sprintf("the job reported success, but %s builds %q", record.Service, zerops.VersionSha(service.DeployedName()))
		}
		deploy.WriteStatus(ctx, p.Gitea, p.log(), target, record.Environment, "failure", deploy.DescriptionFailed+": "+why)
		p.Records.Update(id, func(r *deploy.Record) { r.Status, r.Message = deploy.StatusFailed, why })
		return nil
	}
	// Public access is a post-deploy call: before a service has code the
	// platform refuses it, and it is meaningless on one that serves no HTTP.
	// A refusal here never fails a deploy that landed.
	if service.HTTP() && !service.SubdomainAccess {
		if err := p.Zerops.EnableSubdomainAccess(ctx, service.ID); err != nil {
			p.log().Warn("public access could not be turned on", "service", record.Service, "err", err.Error())
		}
	}
	deploy.WriteStatus(ctx, p.Gitea, p.log(), target, record.Environment, "success", "live")
	p.Records.Update(id, func(r *deploy.Record) { r.Status, r.Message = deploy.StatusSucceeded, "" })
	p.log().Info("deployed", "environment", record.Environment, "service", record.Service, "sha", record.Sha)
	return nil
}

// serviceOf reads one service of a project by its hostname, directly: what is
// deployed lives in the service's own environment.
func (p *Pipeline) serviceOf(ctx context.Context, projectID, hostname string) (zerops.ServiceDetail, error) {
	services, err := p.Zerops.Services(ctx, p.ClientID, projectID)
	if err != nil {
		return zerops.ServiceDetail{}, fmt.Errorf("the project's services could not be read")
	}
	for _, s := range services {
		if s.Name == hostname {
			return p.Zerops.Service(ctx, s.ID)
		}
	}
	return zerops.ServiceDetail{}, fmt.Errorf("the environment's project has no service %s", hostname)
}

// deployToken reads one environment's key off the broker's own service. Empty
// means there is none yet.
func (p *Pipeline) deployToken(ctx context.Context, projectID string) (string, error) {
	services, err := p.Zerops.Services(ctx, p.ClientID, p.GiteaProjectID)
	if err != nil {
		return "", err
	}
	for _, s := range services {
		if s.Name != registry.BrokerHostname {
			continue
		}
		entries, err := p.Zerops.UserData(ctx, s.ID)
		if err != nil {
			return "", err
		}
		want := deploy.TokenVariable(projectID)
		for _, entry := range entries {
			if entry.Key == want {
				return strings.TrimSpace(entry.Content), nil
			}
		}
		return "", nil
	}
	return "", errors.New("the Gitea project has no broker service")
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pipeline) patience() time.Duration {
	if p.Patience > 0 {
		return p.Patience
	}
	return deploy.DefaultPatience
}
