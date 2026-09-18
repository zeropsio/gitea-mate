package deploy

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// A grant is how a deploy happens since D27: a job of the repository's own
// workflow asks, and the broker answers the environment's deploy token — or
// says why there is nothing for this job to do, which is not a failure.

// GrantRequest is what a proved job asks (docs/broker-api.md § POST
// /deploy/grant). Owner, Repo, RunID and TaskID come from the proof; the rest
// is the job's word, and the only part of it that is believed is the sha — and
// that only because it must equal the one protected state wants.
type GrantRequest struct {
	Owner  string
	Repo   string
	RunID  int64
	TaskID string
	// Sha is the commit the job checked out and will push.
	Sha string
	// Environment and Service are optional. A job started by a push names
	// neither and is granted whatever its branch feeds, one at a time.
	Environment string
	Service     string
}

// What a grant answers. Only GrantGranted carries a token; the others end the
// job green, saying why it deployed nothing.
const (
	GrantGranted    = "granted"
	GrantLive       = "live"
	GrantNothing    = "nothing"
	GrantInProgress = "in_progress"
	GrantSuperseded = "superseded"
)

// Grant is the answer.
type Grant struct {
	ID          string `json:"id,omitempty"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	Environment string `json:"environment,omitempty"`
	Service     string `json:"service,omitempty"`
	Sha         string `json:"sha,omitempty"`
	// Token is the environment's deploy token. It is in this answer and
	// nowhere else the broker writes: not a log, not a status, not a record.
	Token       string `json:"token,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	ServiceID   string `json:"serviceId,omitempty"`
	Setup       string `json:"setup,omitempty"`
	VersionName string `json:"versionName,omitempty"`
}

// Refusal is a grant the broker will not give, as the endpoint answers it.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Refuse builds one.
func Refuse(status int, code, format string, args ...any) *Refusal {
	return &Refusal{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// TokenVariablePrefix starts the name of every deploy token's variable on the
// broker's service (docs/vocabulary.md).
const TokenVariablePrefix = "MATE_DEPLOY_TOKEN_"

// TokenVariable is the secret variable on the broker's service that holds one
// environment's deploy token: the prefix and the project id in upper-case
// hex. A project id is base64url — it may carry `-`, which a variable's name
// may not — and hex is the one spelling the app and the broker cannot
// disagree on.
func TokenVariable(projectID string) string {
	return TokenVariablePrefix + strings.ToUpper(hex.EncodeToString([]byte(projectID)))
}

// TrustedRun reports whether a run was the repository's own default branch
// running its own reviewed workflow: started by a push or a dispatch, on the
// default branch, from the repository itself. Anything else — a branch's own
// workflow file, a pull request, a fork — is code nobody with rights has
// merged, and neither gets a key nor may share a runner with a job that does.
func TrustedRun(run gitea.Run) bool {
	if run.Event != "push" && run.Event != "workflow_dispatch" {
		return false
	}
	if run.Repository.DefaultBranch == "" || run.HeadBranch != run.Repository.DefaultBranch {
		return false
	}
	if run.HeadRepository.FullName != "" && run.HeadRepository.FullName != run.Repository.FullName {
		return false
	}
	return true
}
