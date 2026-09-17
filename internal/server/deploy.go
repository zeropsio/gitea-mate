package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
)

// Deploys is the deploy side of the broker, as the two endpoints need it. It is
// an interface so the routes are testable without the pipeline's whole world.
type Deploys interface {
	// Plan reads one group's declarations and tiers from its group repo.
	Plan(ctx context.Context, slug string) (deploy.Plan, error)
	// Deploy resolves an environment and queues the work. Nothing the caller
	// passes ever reaches a commit.
	Deploy(ctx context.Context, plan deploy.Plan, env environments.Environment, service string, records []string) error
}

// RunnerImporter imports a group's Actions runner the first time one of its
// workflows queues a job.
type RunnerImporter interface {
	EnsureRunner(ctx context.Context, org string) error
}

// actionsLogin is the synthetic user a job's token resolves to (measured
// 2026-09-16): `login: gitea-actions`, `login_name: @gitea-actions/{taskId}`.
const actionsLogin = "gitea-actions"

// actionsPrefix is what the task id follows in login_name.
const actionsPrefix = "@gitea-actions/"

// job is a proved caller of the deploy endpoints.
type job struct {
	TaskID     string
	Repository string
	Owner      string
	Repo       string
	HeadSHA    string
	HeadBranch string
}

// proveJob is how the broker knows the caller of /deploy. Being inside the
// project proves nothing — the runners share its network — so the token is
// resolved against Gitea twice: once to learn which task it is, and once
// against the repository it claims. `GET /repos/{claimed}` alone proves
// nothing: it is 200 for any public repository.
func (s *Server) proveJob(w http.ResponseWriter, r *http.Request, claimed string) (job, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "token "))
	if token == "" {
		WriteError(w, http.StatusUnauthorized, "not_a_job", "this endpoint takes a workflow job's token")
		return job{}, false
	}
	asJob := s.deps.Gitea.AsToken(token)

	who, err := asJob.WhoAmI(r.Context())
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "not_a_job", "Gitea does not know that token")
		return job{}, false
	}
	taskID, isJob := strings.CutPrefix(who.LoginName, actionsPrefix)
	if who.Login != actionsLogin || !isJob || taskID == "" {
		WriteError(w, http.StatusUnauthorized, "not_a_job", "that token does not belong to a workflow job")
		return job{}, false
	}

	owner, repo, ok := strings.Cut(claimed, "/")
	if !ok || owner == "" || repo == "" {
		WriteError(w, http.StatusForbidden, "wrong_repository", "repository is {owner}/{name}")
		return job{}, false
	}
	// 404 for every repository but the job's own, public ones included.
	details, err := asJob.GetJob(r.Context(), owner, repo, taskID)
	if err != nil {
		WriteError(w, http.StatusForbidden, "wrong_repository", "that job does not run in "+claimed)
		return job{}, false
	}
	return job{
		TaskID: taskID, Repository: claimed, Owner: owner, Repo: repo,
		HeadSHA: details.HeadSHA, HeadBranch: details.HeadBranch,
	}, true
}

// ---------------------------------------------------------------------------
// POST /deploy
// ---------------------------------------------------------------------------

type deployRequest struct {
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Repository  string `json:"repository"`
}

// handleDeploy is docs/broker-api.md § POST /deploy. The caller picks the
// environment and the service, never a commit and never a ref.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var body deployRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request",
			`the body is {"environment": "…", "service": "…", "repository": "owner/name"}`)
		return
	}
	if body.Environment == "" || body.Service == "" || body.Repository == "" {
		WriteError(w, http.StatusBadRequest, "invalid_request",
			"an environment, a service and the job's repository are all required")
		return
	}

	caller, proved := s.proveJob(w, r, body.Repository)
	if !proved {
		return
	}

	// The repository's org names the group.
	plan, err := s.deps.Deploys.Plan(r.Context(), caller.Owner)
	if err != nil {
		s.log.Error("a group's plan could not be read", "group", caller.Owner, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "the group's environments could not be read")
		return
	}
	env, declared := plan.File.Environment(body.Environment)
	if !declared {
		// The workflow zcp writes asks for a tier's name — `stage` — and the
		// app names an environment after its group (`todo-stage`): every
		// job's deploy answered 404 on the owner's run of 2026-09-17 and the
		// catch-up pass did the work. A tier's name is its only environment;
		// several of a tier need naming.
		switch ofTier := plan.File.OfTier(environments.Tier(body.Environment)); len(ofTier) {
		case 0:
			WriteError(w, http.StatusNotFound, "unknown_environment",
				body.Environment+" is not an environment of "+caller.Owner)
			return
		case 1:
			env, declared = ofTier[0], true
		default:
			names := make([]string, 0, len(ofTier))
			for _, e := range ofTier {
				names = append(names, e.Name)
			}
			WriteError(w, http.StatusNotFound, "unknown_environment",
				body.Environment+" names "+strconv.Itoa(len(ofTier))+" environments of "+caller.Owner+" ("+strings.Join(names, ", ")+"); ask for one by name")
			return
		}
	}

	recipe, hasTier := plan.Recipes[env.Tier]
	if !hasTier {
		WriteError(w, http.StatusBadGateway, "upstream", "the group's "+string(env.Tier)+" tier could not be read")
		return
	}
	service, known := recipe.Service(body.Service)
	if !known || !service.Runtime() {
		WriteError(w, http.StatusNotFound, "unknown_service",
			body.Service+" is not a runtime service of "+body.Environment)
		return
	}

	record := s.deps.Records.New(env.Name, service.Hostname, body.Repository, "")
	if err := s.deps.Deploys.Deploy(r.Context(), plan, env, service.Hostname, []string{record.ID}); err != nil {
		s.log.Error("a deploy could not be queued",
			"group", caller.Owner, "environment", env.Name, "err", err.Error())
	}
	// The record carries the sha the resolver decided, and a refusal if the
	// environment resolved to nothing.
	answer, _ := s.deps.Records.Get(record.ID)
	WriteJSON(w, http.StatusAccepted, answer)
}

// ---------------------------------------------------------------------------
// GET /deploy/{id}
// ---------------------------------------------------------------------------

// handleDeployStatus is docs/broker-api.md § GET /deploy/{id}: the same job, or
// any job of the same repository. A restart forgets, and the action then reads
// the commit status the broker wrote on every outcome instead.
func (s *Server) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	record, known := s.deps.Records.Get(id)
	if !known {
		// Refused before the caller is proved: an id the broker does not hold
		// belongs to no repository, so there is nothing to prove against.
		WriteError(w, http.StatusNotFound, "unknown_deploy",
			"this broker holds no deploy "+id+"; read the commit status instead")
		return
	}
	if _, proved := s.proveJob(w, r, record.Repository); !proved {
		return
	}
	WriteJSON(w, http.StatusOK, record)
}

// ---------------------------------------------------------------------------

// ensureRunner is what the runner pool calls when a group's runner service does
// not exist yet.
func (s *Server) ensureRunner(ctx context.Context, org string) error {
	if s.deps.Runners == nil {
		return errors.New("this broker imports no runners")
	}
	return s.deps.Runners.EnsureRunner(ctx, org)
}
