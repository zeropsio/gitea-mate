// Package server is the broker's HTTP surface: the routes of
// docs/broker-api.md and nothing else.
//
// No endpoint takes a Zerops key, and being inside the project proves nothing
// — the runners share its network — so every route that changes state proves
// its caller for itself: /mate/repository a Mate's Gitea token, /hooks/gitea
// the HMAC, and the OIDC routes the client secret (and /oidc/complete a
// throwaway). A Mate's own Gitea access is no route at all: the rights loop
// delivers it.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/oidc"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// repoNamePattern is what a service repository may be called
// (docs/vocabulary.md).
var repoNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Deps are what the broker's routes need. Everything is an explicit
// dependency, so a test drives the whole router against two fakes.
type Deps struct {
	Zerops *zerops.Client
	Gitea  *gitea.Client
	OIDC   *oidc.Provider
	// Hooks takes every webhook the routes do not handle themselves.
	Hooks Hooks
	// Deploys is what POST /deploy/grant and POST /deploy/{id}/result drive.
	Deploys Deploys
	// Runners imports a group's Actions runner the first time one of its
	// workflows queues a job.
	Runners RunnerImporter
	// Throwaway proves a person for POST /person/token; nil means the route
	// is not served. Rights reads what they may do, and Pass runs the rights
	// loop once so a fresh account is in its teams before the route answers.
	Throwaway PersonProver
	Rights    RightsReader
	Pass      PassFunc
}

// Server holds the broker's dependencies and builds its router.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	deps    Deps
	runners *runnerPool
}

// New builds a server. log may be nil, in which case the default logger is
// used; Hooks may be nil, in which case nothing happens to the events the next
// brief claims.
func New(cfg *config.Config, log *slog.Logger, deps Deps) *Server {
	if log == nil {
		log = slog.Default()
	}
	if deps.Hooks == nil {
		deps.Hooks = NoopHooks{}
	}
	s := &Server{cfg: cfg, log: log, deps: deps}
	if deps.Zerops != nil {
		s.runners = newRunnerPool(deps.Zerops, log, cfg.ZeropsClientID, cfg.ZeropsProjectID, cfg.RunnerQuietPeriod, s.ensureRunner)
	}
	return s
}

// Close stops anything the server armed — a pending runner sleep, above all.
func (s *Server) Close() {
	if s.runners != nil {
		s.runners.stop()
	}
}

// Handler returns the router, wrapped in request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return logRequests(s.log, mux)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	if s.deps.Gitea != nil {
		mux.HandleFunc("POST /mate/repository", s.handleRepository)
	}
	mux.HandleFunc("POST /hooks/gitea", s.handleGiteaHook)
	if s.deps.Deploys != nil {
		mux.HandleFunc("POST /deploy/grant", s.handleDeployGrant)
		mux.HandleFunc("POST /deploy/{id}/result", s.handleDeployResult)
	}
	if s.deps.Throwaway != nil && s.deps.Rights != nil && s.deps.Gitea != nil {
		mux.HandleFunc("POST /person/token", s.handlePersonToken)
		mux.HandleFunc("OPTIONS /person/token", s.handlePersonTokenPreflight)
	}
	if s.deps.OIDC != nil {
		s.deps.OIDC.Routes(mux)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// WriteJSON writes v as the response body. v is marshalled before the header is
// sent, so a failure never leaves a half-written body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal","message":"the answer could not be encoded"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ErrorBody is the shape of every refusal: docs/broker-api.md says errors are
// {"error": "<code>", "message": "<plain words>"}. Some refusals carry a
// reason as well (the throwaway check).
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

// WriteError answers with a coded refusal.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message})
}

// WriteErrorReason answers with a coded refusal that names a reason.
func WriteErrorReason(w http.ResponseWriter, status int, code, message, reason string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message, Reason: reason})
}

// statusRecorder remembers the status a handler wrote, so the log line can
// carry it. It records nothing else about the exchange.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// logRequests logs one line per request: the method, the path, the status and
// how long it took. Never a body, never a header — an Authorization header
// carries a Zerops throwaway, a Gitea token or a job's token, and a webhook
// body carries the repository's contents.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"ms", time.Since(start).Milliseconds(),
		)
	})
}
