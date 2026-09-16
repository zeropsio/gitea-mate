package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// Target is one service of one environment and the commit it should run.
// Everything a caller could try to influence is already decided: the sha comes
// from protected state (decide.go), never from a request.
type Target struct {
	// Service is the hostname, in both the recipe and the Zerops project.
	Service string
	// Owner and Repo are the service's repository — where the archive comes
	// from and where the commit status is written.
	Owner string
	Repo  string
	// Sha is the commit.
	Sha string
	// VersionName is what the Zerops app version is called: the sha, and for
	// production "{sha} {tag} {tagger}". The sha is always the first token.
	VersionName string
	// Setup is the target tier's zeropsSetup for this service.
	Setup string
	// PromoteFrom is the stage this production deploy may promote from. Nil
	// for a stage, and for a production environment with no stage beside it.
	PromoteFrom *PromoteSource
	// Gate is environments.yaml's `requireOnStage`: this commit must already
	// be live on that stage. Nil when the environment declares none.
	Gate *Gate
}

// Gate is the stage a commit must already be live on before it reaches
// production (docs/group-repo.md, environments.yaml `gates`).
type Gate struct {
	Environment string
	Project     string
}

// PromoteSource is where a promotion looks for an already-built artifact.
type PromoteSource struct {
	// Project is the stage's Zerops project.
	Project string
	// Setup is the stage tier's zeropsSetup for this service, which is what
	// the build sections are compared through.
	Setup string
}

// Job is one deploy of one environment: everything the executor performs
// without asking anyone else a question.
type Job struct {
	// Slug is the group's Gitea org.
	Slug string
	// Environment is the target.
	Environment environments.Environment
	// Targets are its services, in the tier's priority order.
	Targets []Target
	// Records are the deploy records this job answers for, if any. A push or a
	// catch-up pass has none; a POST /deploy has one.
	Records []string
}

// Key is the queue this job belongs to: one per environment.
func (j Job) Key() string { return j.Slug + "/" + j.Environment.Name }

// Sha is what the job is for, for a log line. A job with several targets is
// named by its first.
func (j Job) Sha() string {
	if len(j.Targets) == 0 {
		return ""
	}
	return j.Targets[0].Sha
}

// Executor performs jobs.
type Executor struct {
	Zerops *zerops.Client
	Gitea  *gitea.Client
	Log    *slog.Logger
	// ClientID is the Zerops org.
	ClientID string
	// Records is the memory GET /deploy/{id} reads.
	Records *Records
	// PollInterval is how often a running platform process is asked where it
	// is; Timeout bounds one service's deploy.
	PollInterval time.Duration
	Timeout      time.Duration
}

// DefaultTimeout bounds one service's build and deploy. A Zerops build of a
// small app settled in 59 s when it was measured; fifteen minutes is the
// action's own patience, and the broker does not outlast it.
const DefaultTimeout = 15 * time.Minute

func (e *Executor) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Executor) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return DefaultTimeout
}

// StatusContext is the commit status a deploy writes on the service
// repository's commit (docs/vocabulary.md).
func StatusContext(environment, service string) string {
	return "mate/deploy/" + environment + "/" + service
}

// Run performs one job: each service in turn, in the order the targets were
// built. A service that fails does not stop the ones behind it — each has its
// own commit status, and the next pass re-plans from whatever is true then.
func (e *Executor) Run(ctx context.Context, job Job) {
	e.Records.UpdateAll(job.Records, func(r *Record) {
		r.Status = StatusRunning
		if sha := job.Sha(); sha != "" {
			r.Sha = sha
		}
	})

	services, err := e.Zerops.Services(ctx, e.ClientID, job.Environment.Project)
	if err != nil {
		e.fail(ctx, job, Target{}, fmt.Sprintf("the project's services could not be read: %v", err))
		return
	}
	byName := map[string]zerops.Service{}
	for _, s := range services {
		byName[s.Name] = s
	}

	for _, target := range job.Targets {
		service, known := byName[target.Service]
		if !known {
			e.fail(ctx, job, target, "the environment's project has no service "+target.Service)
			continue
		}
		e.one(ctx, job, target, service)
	}
}

// one deploys a single service.
func (e *Executor) one(ctx context.Context, job Job, target Target, service zerops.Service) {
	log := e.log().With("environment", job.Environment.Name, "service", target.Service, "sha", target.Sha)

	active, live, err := e.Zerops.ActiveAppVersion(ctx, service.ID)
	if err != nil {
		e.fail(ctx, job, target, fmt.Sprintf("the service's app versions could not be read: %v", err))
		return
	}
	if live && active.Sha() == target.Sha {
		// Nothing to do. The catch-up pass reaches here on every environment
		// that is already where it should be, which is almost every pass.
		log.Debug("the service already runs this commit")
		e.Records.UpdateAll(job.Records, func(r *Record) {
			r.Status = StatusActive
			r.VersionID = active.ID
			r.Message = "already live"
		})
		return
	}

	if met, why := e.gate(ctx, target); !met {
		e.fail(ctx, job, target, why)
		return
	}

	e.status(ctx, target, job.Environment.Name, "pending", "deploying "+target.Sha)

	zeropsYaml, err := e.zeropsYaml(ctx, target)
	if err != nil {
		e.fail(ctx, job, target, err.Error())
		return
	}
	if !HasSetup(zeropsYaml, target.Setup) {
		e.fail(ctx, job, target, fmt.Sprintf("%s/%s@%s has no setup %q, which the tier names", target.Owner, target.Repo, target.Sha, target.Setup))
		return
	}

	option := e.promoteOption(ctx, target, job.Environment.Tier, zeropsYaml)
	var archive []byte
	switch option.Choose() {
	case Promote:
		url, err := e.Zerops.AppCodeURL(ctx, option.StageVersionID)
		if err != nil {
			e.fail(ctx, job, target, fmt.Sprintf("the stage version's app code could not be read: %v", err))
			return
		}
		if archive, err = e.Zerops.Download(ctx, url); err != nil {
			e.fail(ctx, job, target, fmt.Sprintf("the stage version's app code could not be downloaded: %v", err))
			return
		}
		log.Info("promoting the stage artifact", "from", option.StageVersionID)
	default:
		if archive, err = e.Gitea.Archive(ctx, target.Owner, target.Repo, target.Sha); err != nil {
			e.fail(ctx, job, target, fmt.Sprintf("the commit archive could not be read: %v", err))
			return
		}
	}

	version, err := e.Zerops.CreateAppVersion(ctx, service.ID, target.VersionName)
	if err != nil {
		e.fail(ctx, job, target, fmt.Sprintf("the app version could not be created: %v", err))
		return
	}
	e.Records.UpdateAll(job.Records, func(r *Record) { r.VersionID = version.ID })

	if err := e.Zerops.UploadAppVersion(ctx, version.ID, archive); err != nil {
		e.fail(ctx, job, target, fmt.Sprintf("the archive could not be uploaded: %v", err))
		return
	}
	process, err := e.Zerops.BuildAndDeploy(ctx, version.ID, string(zeropsYaml), target.Setup)
	if err != nil {
		e.fail(ctx, job, target, fmt.Sprintf("the deploy could not be started: %v", err))
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, e.timeout())
	final, err := e.Zerops.AwaitProcess(waitCtx, process.ID, e.PollInterval)
	cancel()
	var expired *zerops.ErrProcessTimeout
	switch {
	case errors.As(err, &expired):
		e.fail(ctx, job, target, "the platform was still working when the broker stopped waiting; the next pass reads what it settled on")
		return
	case err != nil:
		e.fail(ctx, job, target, fmt.Sprintf("the deploy could not be followed: %v", err))
		return
	case final.Status != zerops.ProcessFinished:
		e.fail(ctx, job, target, "the platform ended the deploy as "+final.Status)
		return
	}

	// Public access is a post-deploy call: before a service has code the
	// platform refuses it, and it is meaningless on one that serves no HTTP.
	// A refusal here never fails a deploy that landed.
	if service.HTTP() && !service.SubdomainAccess {
		if err := e.Zerops.EnableSubdomainAccess(ctx, service.ID); err != nil {
			log.Warn("public access could not be turned on", "err", err.Error())
		}
	}

	e.status(ctx, target, job.Environment.Name, "success", version.ID)
	e.Records.UpdateAll(job.Records, func(r *Record) {
		r.Status = StatusActive
		r.VersionID = version.ID
		r.Message = ""
	})
	log.Info("deployed", "versionId", version.ID)
}

// gate enforces environments.yaml's `requireOnStage`: production runs nothing
// a named stage is not already running. A stage the broker cannot read is a
// gate that is not met — an unreadable gate must never read as an open one.
func (e *Executor) gate(ctx context.Context, target Target) (bool, string) {
	if target.Gate == nil {
		return true, ""
	}
	services, err := e.Zerops.Services(ctx, e.ClientID, target.Gate.Project)
	if err != nil {
		return false, fmt.Sprintf("the gate %s could not be read: %v", target.Gate.Environment, err)
	}
	for _, s := range services {
		if s.Name != target.Service {
			continue
		}
		active, live, err := e.Zerops.ActiveAppVersion(ctx, s.ID)
		if err != nil {
			return false, fmt.Sprintf("the gate %s could not be read: %v", target.Gate.Environment, err)
		}
		if live && active.Sha() == target.Sha {
			return true, ""
		}
		return false, fmt.Sprintf("%s is not live on %s, which this environment gates on", target.Sha, target.Gate.Environment)
	}
	return false, fmt.Sprintf("the gate %s has no service %s", target.Gate.Environment, target.Service)
}

// zeropsYaml reads the commit's own build file. Both the archive path and the
// promote path send it to build-and-deploy: the platform does not reuse a
// stored one.
func (e *Executor) zeropsYaml(ctx context.Context, target Target) ([]byte, error) {
	var last error
	for _, name := range ZeropsYamlNames {
		raw, err := e.Gitea.File(ctx, target.Owner, target.Repo, name, target.Sha)
		if err == nil {
			return raw, nil
		}
		if !gitea.IsNotFound(err) {
			return nil, fmt.Errorf("%s/%s@%s: %s: %w", target.Owner, target.Repo, target.Sha, name, err)
		}
		last = err
	}
	_ = last
	return nil, fmt.Errorf("%s/%s@%s carries no zerops.yaml", target.Owner, target.Repo, target.Sha)
}

// promoteOption gathers what the choice depends on. Any read that does not
// answer leaves the option empty, which means "upload the archive" — the path
// that always works.
func (e *Executor) promoteOption(ctx context.Context, target Target, tier environments.Tier, zeropsYaml []byte) PromoteOption {
	option := PromoteOption{Tier: tier}
	if tier != environments.TierProduction || target.PromoteFrom == nil {
		return option
	}

	services, err := e.Zerops.Services(ctx, e.ClientID, target.PromoteFrom.Project)
	if err != nil {
		e.log().Warn("the stage's services could not be read, so the commit is rebuilt", "err", err.Error())
		return option
	}
	var stageID string
	for _, s := range services {
		if s.Name == target.Service {
			stageID = s.ID
		}
	}
	if stageID == "" {
		return option
	}

	versions, err := e.Zerops.AppVersions(ctx, stageID)
	if err != nil {
		e.log().Warn("the stage's app versions could not be read, so the commit is rebuilt", "err", err.Error())
		return option
	}
	for _, v := range versions {
		// A version that failed to build is not an artifact.
		if v.Sha() == target.Sha && (v.Status == zerops.AppVersionActive || v.Status == "BACKUP") {
			option.StageVersionID = v.ID
			break
		}
	}
	if option.StageVersionID == "" {
		return option
	}

	same, err := SameBuild(zeropsYaml, target.PromoteFrom.Setup, target.Setup)
	if err != nil {
		e.log().Warn("the two setups could not be compared, so the commit is rebuilt", "err", err.Error())
		return option
	}
	option.SameBuild = same
	return option
}

// fail records one service's failure: the commit status, the deploy records
// and a log line. It is never fatal to the job — the services behind this one
// still get their turn.
func (e *Executor) fail(ctx context.Context, job Job, target Target, message string) {
	e.log().Warn("a deploy failed",
		"environment", job.Environment.Name, "service", target.Service, "sha", target.Sha, "reason", message)
	if target.Repo != "" {
		e.status(ctx, target, job.Environment.Name, "failure", message)
	}
	e.Records.UpdateAll(job.Records, func(r *Record) {
		r.Status = StatusFailed
		r.Message = message
	})
}

// status writes the deploy's outcome where it survives a restart. Gitea caps a
// description, so a long platform error is cut rather than refused.
func (e *Executor) status(ctx context.Context, target Target, environment, state, description string) {
	if target.Owner == "" || target.Repo == "" || target.Sha == "" {
		return
	}
	if len(description) > 240 {
		description = description[:237] + "..."
	}
	_, err := e.Gitea.CreateStatus(ctx, target.Owner, target.Repo, target.Sha, gitea.NewStatus{
		Context:     StatusContext(environment, target.Service),
		State:       state,
		Description: strings.TrimSpace(description),
	})
	if err != nil {
		e.log().Warn("a deploy status could not be written",
			"repo", target.Owner+"/"+target.Repo, "sha", target.Sha, "err", err.Error())
	}
}
