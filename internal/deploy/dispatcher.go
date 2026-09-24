package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// WorkflowFile is the workflow zcp writes into every service repository
// (`.gitea/workflows/zerops.yml`): the one the broker dispatches.
const WorkflowFile = "zerops.yml"

// DefaultPatience is how long a deploy that was dispatched, or that holds a
// grant, is left alone before a pass dispatches it again. A Zerops build of a
// small app settled in 59 s when it was measured, a group's first job waits
// about a minute and a half for its runner; twenty minutes is well past both
// and short enough that a job which died silent is not mourned for an hour.
const DefaultPatience = 20 * time.Minute

// Dispatcher performs a Job the only way the broker does since D27: it starts
// the service repository's own workflow, whose job checks the commit out and
// runs `zcli push`. The broker decides what is deployed and never moves code.
type Dispatcher struct {
	Zerops *zerops.Client
	Gitea  *gitea.Client
	Log    *slog.Logger
	// ClientID is the Zerops org.
	ClientID string
	// Patience is [DefaultPatience] when zero. Now is time.Now when nil.
	Patience time.Duration
	Now      func() time.Time
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *Dispatcher) patience() time.Duration {
	if d.Patience > 0 {
		return d.Patience
	}
	return DefaultPatience
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Run dispatches one job: each service in turn, in the order the targets were
// built. A service that cannot be dispatched does not stop the ones behind it
// — each has its own commit status, and the next pass re-plans from whatever
// is true then.
func (d *Dispatcher) Run(ctx context.Context, job Job) {
	services, err := d.Zerops.Services(ctx, d.ClientID, job.Environment.Project)
	if err != nil {
		d.log().Warn("an environment's services could not be read, so nothing was dispatched",
			"environment", job.Environment.Name, "err", err.Error())
		return
	}
	byName := map[string]zerops.Service{}
	for _, s := range services {
		byName[s.Name] = s
	}
	for _, target := range job.Targets {
		service, known := byName[target.Service]
		if !known {
			d.fail(ctx, job, target, "the environment's project has no service "+target.Service)
			continue
		}
		d.one(ctx, job, target, service.ID)
	}
}

// one decides whether a single service needs a job, and starts it. It reads
// the service directly rather than trusting the search's copy: what is
// deployed lives in the service's own environment, and a stale read there
// would dispatch a commit already live.
func (d *Dispatcher) one(ctx context.Context, job Job, target Target, serviceID string) {
	log := d.log().With("environment", job.Environment.Name, "service", target.Service, "sha", target.Sha)

	service, err := d.Zerops.Service(ctx, serviceID)
	if err != nil {
		d.fail(ctx, job, target, fmt.Sprintf("the service could not be read: %v", err))
		return
	}
	history, err := ReadHistory(ctx, d.Gitea, target, job.Environment.Name)
	if err != nil {
		log.Warn("a commit's statuses could not be read, so nothing was dispatched", "err", err.Error())
		return
	}
	status, has := history.Latest, history.Has
	if service.DeployedSha() == target.Sha {
		// Where almost every pass ends. A job that died after its push landed
		// never reported, and its status would say "deploying" for ever; one
		// that reported a failure the platform then settled would say failed.
		MarkLive(ctx, d.Gitea, d.log(), target, job.Environment.Name, history)
		return
	}
	if history.Failed() && !job.Requested {
		log.Info("a deploy of this commit failed, and only a person starts it again", "status", status.Description)
		return
	}
	if has && status.State == "pending" && d.now().Sub(status.CreatedAt) < d.patience() {
		log.Debug("a deploy of this commit is already on its way", "status", status.Description)
		return
	}
	if met, why := GateMet(ctx, d.Zerops, d.ClientID, target); !met {
		d.fail(ctx, job, target, why)
		return
	}

	repo, err := d.Gitea.GetRepo(ctx, target.Owner, target.Repo)
	if err != nil {
		d.fail(ctx, job, target, fmt.Sprintf("the repository could not be read: %v", err))
		return
	}
	err = d.Gitea.DispatchWorkflow(ctx, target.Owner, target.Repo, WorkflowFile, repo.DefaultBranch, map[string]string{
		"environment": job.Environment.Name,
		"service":     target.Service,
		"sha":         target.Sha,
	})
	if err != nil {
		// A workflow from before D27 has no `workflow_dispatch`, and a
		// repository with none at all has nothing to dispatch: both wait for
		// the Mate's next pull request, which carries the current file.
		d.fail(ctx, job, target, fmt.Sprintf(
			"%s/%s has no workflow %s the broker can start on %s (%s) — merge the Mate's pull request that updates it",
			target.Owner, target.Repo, WorkflowFile, repo.DefaultBranch, short(err)))
		return
	}
	WriteStatus(ctx, d.Gitea, d.log(), target, job.Environment.Name, "pending", DescriptionDispatched)
	log.Info("a deploy job was dispatched", "repository", target.Owner+"/"+target.Repo, "ref", repo.DefaultBranch)
}

// MarkLive closes the status of a commit the service verifiably runs: whatever
// a job or a pass wrote before — pending, or a failure the platform then
// settled — the commit is live.
func MarkLive(ctx context.Context, g *gitea.Client, log *slog.Logger, target Target, environment string, history History) {
	if history.Has && history.Latest.State != "success" {
		WriteStatus(ctx, g, log, target, environment, "success", "live")
	}
}

// fail records why one service got no job: the commit status and a log line.
// It is never fatal to the rest — the services behind this one still get
// their turn.
func (d *Dispatcher) fail(ctx context.Context, job Job, target Target, message string) {
	d.log().Warn("a deploy could not be dispatched",
		"environment", job.Environment.Name, "service", target.Service, "sha", target.Sha, "reason", message)
	if target.Repo != "" {
		WriteStatus(ctx, d.Gitea, d.log(), target, job.Environment.Name, "failure", message)
	}
}

func short(err error) string {
	text := err.Error()
	if len(text) > 120 {
		text = text[:117] + "..."
	}
	return strings.TrimSpace(text)
}
