// Package server is the broker's HTTP surface: the routes of
// docs/broker-api.md and nothing else.
//
// No endpoint takes a Zerops key, and being inside the project proves nothing
// — the runners share its network — so every route that changes state proves
// its caller for itself.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/zeropsio/gitea-mate/internal/config"
)

// Server holds the broker's dependencies and builds its router.
type Server struct {
	cfg *config.Config
	log *slog.Logger
}

// New builds a server. log may be nil, in which case the default logger is
// used.
func New(cfg *config.Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log}
}

// Handler returns the router, wrapped in request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return logRequests(s.log, mux)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// WriteJSON writes v as the response body. A marshalling failure is logged by
// the caller's absence of output, never by a half-written body: v is marshalled
// before the header is sent.
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
