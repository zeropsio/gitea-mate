package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/deploy"
)

// Deploys is the deploy side of the broker, as the two endpoints need it. It is
// an interface so the routes are testable without the pipeline's whole world.
type Deploys interface {
	// Grant decides whether a proved job may deploy, and hands it the
	// environment's key when it may. A refusal is a *deploy.Refusal.
	Grant(ctx context.Context, req deploy.GrantRequest) (deploy.Grant, error)
	// Result is the job's report on a grant, by a job of repository.
	Result(ctx context.Context, id, repository, outcome, message string) error
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
	RunID      int64
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
		TaskID: taskID, RunID: details.RunID, Repository: claimed, Owner: owner, Repo: repo,
		HeadSHA: details.HeadSHA, HeadBranch: details.HeadBranch,
	}, true
}

// ---------------------------------------------------------------------------
// POST /deploy/grant
// ---------------------------------------------------------------------------

type grantRequest struct {
	Repository  string `json:"repository"`
	Sha         string `json:"sha"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
}

// handleDeployGrant is docs/broker-api.md § POST /deploy/grant (D27). The job
// says which commit it holds and, when it was dispatched, which environment;
// it never picks a commit — the one it holds is deployed only if protected
// state wants exactly that one.
func (s *Server) handleDeployGrant(w http.ResponseWriter, r *http.Request) {
	var body grantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request",
			`the body is {"repository": "owner/name", "sha": "…", "environment": "…", "service": "…"}`)
		return
	}
	if body.Repository == "" || body.Sha == "" {
		WriteError(w, http.StatusBadRequest, "invalid_request", "the job's repository and the commit it holds are both required")
		return
	}
	caller, proved := s.proveJob(w, r, body.Repository)
	if !proved {
		return
	}
	grant, err := s.deps.Deploys.Grant(r.Context(), deploy.GrantRequest{
		Owner: caller.Owner, Repo: caller.Repo, RunID: caller.RunID, TaskID: caller.TaskID,
		Sha: body.Sha, Environment: body.Environment, Service: body.Service,
	})
	if err != nil {
		writeRefusal(w, s, err)
		return
	}
	// The one answer that carries a key: never cached by anything between.
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusOK, grant)
}

// ---------------------------------------------------------------------------
// POST /deploy/{id}/result
// ---------------------------------------------------------------------------

type resultRequest struct {
	Repository string `json:"repository"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

// handleDeployResult is docs/broker-api.md § POST /deploy/{id}/result: the job
// says how its `zcli push` ended, and the broker writes what is true.
func (s *Server) handleDeployResult(w http.ResponseWriter, r *http.Request) {
	var body resultRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil ||
		body.Repository == "" || (body.Status != "success" && body.Status != "failure") {
		WriteError(w, http.StatusBadRequest, "invalid_request",
			`the body is {"repository": "owner/name", "status": "success" | "failure", "message": "…"}`)
		return
	}
	caller, proved := s.proveJob(w, r, body.Repository)
	if !proved {
		return
	}
	if err := s.deps.Deploys.Result(r.Context(), r.PathValue("id"), caller.Repository, body.Status, body.Message); err != nil {
		writeRefusal(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRefusal answers a *deploy.Refusal as it is, and anything else as a
// fault of the broker's.
func writeRefusal(w http.ResponseWriter, s *Server, err error) {
	var refusal *deploy.Refusal
	if errors.As(err, &refusal) {
		WriteError(w, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	s.log.Error("a deploy request failed", "err", err.Error())
	WriteError(w, http.StatusInternalServerError, "internal", "the broker could not answer")
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
