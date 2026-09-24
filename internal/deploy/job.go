package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// Target is one service of one environment and the commit it should run.
// Everything a caller could try to influence is already decided: the sha comes
// from protected state (resolve.go), never from a request.
type Target struct {
	// Service is the hostname, in both the recipe and the Zerops project.
	Service string
	// Owner and Repo are the service's repository — whose workflow deploys it
	// and where the commit status is written.
	Owner string
	Repo  string
	// Sha is the commit.
	Sha string
	// VersionName is what the Zerops app version is called: the sha, and for
	// production "{sha} {tag} {tagger}". The sha is always the first token.
	VersionName string
	// Setup is the target tier's zeropsSetup for this service.
	Setup string
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

// Job is one environment's deploy: the targets a pass, a push or a release
// found behind. Since D27 nothing here deploys — a job of the repository's own
// workflow does, with `zcli push` — so performing a Job means dispatching
// those workflows.
type Job struct {
	// Slug is the group's Gitea org.
	Slug string
	// Environment is the target.
	Environment environments.Environment
	// Targets are its services, in the tier's priority order.
	Targets []Target
	// Requested is a person's explicit ask — a newly approved release tag. It
	// dispatches once even over a deploy of the same commit that failed.
	Requested bool
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

// StatusContext is the commit status a deploy writes on the service
// repository's commit (docs/vocabulary.md).
func StatusContext(environment, service string) string {
	return "mate/deploy/" + environment + "/" + service
}

// The descriptions the broker gives a deploy status. The two pending ones are
// how a pass tells a job it started from one that already holds the key, and
// both from a status nobody is behind any more; the failed one is how it tells
// a job's own report from the broker's refusals.
const (
	// DescriptionDispatched is written when the broker dispatched the job.
	DescriptionDispatched = "dispatched"
	// DescriptionDeploying prefixes the status a grant writes.
	DescriptionDeploying = "deploying"
	// DescriptionFailed, then ": ", prefixes a failure a job reported on its
	// grant, or a success the service it deployed contradicts. The colon keeps
	// it apart from a refusal that opens with an owner named "failed…".
	DescriptionFailed = "failed"
)

// WriteStatus writes a deploy's state where it survives a restart. Gitea caps
// a description, so a long message is cut rather than refused.
func WriteStatus(ctx context.Context, g *gitea.Client, log *slog.Logger, target Target, environment, state, description string) {
	if target.Owner == "" || target.Repo == "" || target.Sha == "" {
		return
	}
	if len(description) > 240 {
		description = description[:237] + "..."
	}
	_, err := g.CreateStatus(ctx, target.Owner, target.Repo, target.Sha, gitea.NewStatus{
		Context:     StatusContext(environment, target.Service),
		State:       state,
		Description: strings.TrimSpace(description),
	})
	if err != nil && log != nil {
		log.Warn("a deploy status could not be written",
			"repo", target.Owner+"/"+target.Repo, "sha", target.Sha, "err", err.Error())
	}
}

// LatestStatus is the newest status of one context on a commit, and whether
// there is one.
func LatestStatus(ctx context.Context, g *gitea.Client, target Target, environment string) (gitea.CommitStatus, bool, error) {
	history, err := ReadHistory(ctx, g, target, environment)
	return history.Latest, history.Has, err
}

// History is what a commit's statuses say about one service of one
// environment: the newest.
type History struct {
	Latest gitea.CommitStatus
	Has    bool
}

// Failed reports a deploy that ran and failed: the newest status is a job's
// own failure report ([DescriptionFailed]). It is final for that commit on
// that service: no pass starts it again, only a person does. Every other
// failure is the broker's own refusal — a tainted runner, a gate not met yet,
// a read that failed — and a pass retries it.
func (h History) Failed() bool {
	return h.Has && h.Latest.State == "failure" && strings.HasPrefix(h.Latest.Description, DescriptionFailed+": ")
}

// ReadHistory reads one context's statuses on a commit. Gitea lists them in an
// order its API lets a caller change, so the newest is found by id.
func ReadHistory(ctx context.Context, g *gitea.Client, target Target, environment string) (History, error) {
	statuses, err := g.ListStatuses(ctx, target.Owner, target.Repo, target.Sha)
	if err != nil {
		return History{}, err
	}
	want := StatusContext(environment, target.Service)
	var out History
	for _, status := range statuses {
		if status.Context == want && (!out.Has || status.ID > out.Latest.ID) {
			out.Latest, out.Has = status, true
		}
	}
	return out, nil
}

// GateMet enforces environments.yaml's `requireOnStage`: production runs
// nothing a named stage is not already running. A stage the broker cannot read
// is a gate that is not met — an unreadable gate must never read as an open
// one.
func GateMet(ctx context.Context, z *zerops.Client, clientID string, target Target) (bool, string) {
	if target.Gate == nil {
		return true, ""
	}
	services, err := z.Services(ctx, clientID, target.Gate.Project)
	if err != nil {
		return false, fmt.Sprintf("the gate %s could not be read: %v", target.Gate.Environment, err)
	}
	for _, s := range services {
		if s.Name != target.Service {
			continue
		}
		staged, err := z.Service(ctx, s.ID)
		if err != nil {
			return false, fmt.Sprintf("the gate %s could not be read: %v", target.Gate.Environment, err)
		}
		if staged.DeployedSha() == target.Sha {
			return true, ""
		}
		return false, fmt.Sprintf("%s is not live on %s, which this environment gates on", target.Sha, target.Gate.Environment)
	}
	return false, fmt.Sprintf("the gate %s has no service %s", target.Gate.Environment, target.Service)
}
