package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// Hooks is everything a Gitea webhook asks for that this brief does not build:
// deploys, release approval and recipe deltas. The broker answers 204 as soon
// as the signature checks out and hands the payload here, after the response.
//
// [NoopHooks] is the implementation until the next brief fills it in.
type Hooks interface {
	// Push is a push to any branch of any repository of a group's org.
	Push(ctx context.Context, org string, payload []byte) error
	// Create is a branch or tag created — a v* tag on the group repo is a
	// release.
	Create(ctx context.Context, org string, payload []byte) error
	// PullRequest is a pull request opened, closed or merged.
	PullRequest(ctx context.Context, org string, payload []byte) error
	// Other is every event neither this brief nor the list above claims.
	Other(ctx context.Context, org, event string, payload []byte) error
}

// NoopHooks takes every event and does nothing with it.
type NoopHooks struct{}

// Push does nothing.
func (NoopHooks) Push(context.Context, string, []byte) error { return nil }

// Create does nothing.
func (NoopHooks) Create(context.Context, string, []byte) error { return nil }

// PullRequest does nothing.
func (NoopHooks) PullRequest(context.Context, string, []byte) error { return nil }

// Other does nothing.
func (NoopHooks) Other(context.Context, string, string, []byte) error { return nil }

// maxHookBody bounds what a webhook may post. Gitea's push payloads carry every
// commit of the push.
const maxHookBody = 8 << 20

// handleGiteaHook is docs/broker-api.md § POST /hooks/gitea. Nothing but the
// signature is trusted: a runner in the same project can reach this port.
func (s *Server) handleGiteaHook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", "the body could not be read")
		return
	}
	if !validSignature(raw, r.Header.Get("X-Gitea-Signature"), s.cfg.GiteaWebhookSecret.Reveal()) {
		WriteError(w, http.StatusUnauthorized, "bad_signature", "that delivery is not signed by this account's Gitea")
		return
	}

	event := r.Header.Get("X-Gitea-Event")
	org := orgOfPayload(raw)

	// Always 204 once the signature is good, whatever the payload; the work
	// runs after the response.
	w.WriteHeader(http.StatusNoContent)

	go s.dispatchHook(event, org, raw)
}

func (s *Server) dispatchHook(event, org string, raw []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var err error
	switch event {
	case "workflow_job":
		s.runners.handle(ctx, org, raw)
	case "push":
		err = s.deps.Hooks.Push(ctx, org, raw)
	case "create":
		err = s.deps.Hooks.Create(ctx, org, raw)
	case "pull_request":
		err = s.deps.Hooks.PullRequest(ctx, org, raw)
	default:
		err = s.deps.Hooks.Other(ctx, org, event, raw)
	}
	if err != nil {
		s.log.Error("a webhook could not be handled", "event", event, "org", org, "err", err.Error())
	}
}

// validSignature is the hex HMAC-SHA256 of the raw body with
// GITEA_WEBHOOK_SECRET, compared without leaking where it differs.
func validSignature(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}
	sent, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sent, mac.Sum(nil))
}

// orgOfPayload reads the Gitea org out of a delivery. Every event the broker
// takes carries a repository, and the org is the group.
func orgOfPayload(raw []byte) string {
	var payload struct {
		Repository struct {
			Owner struct {
				Login    string `json:"login"`
				Username string `json:"username"`
			} `json:"owner"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		Organization struct {
			Username string `json:"username"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	for _, candidate := range []string{
		payload.Repository.Owner.Username,
		payload.Repository.Owner.Login,
		payload.Organization.Username,
	} {
		if candidate != "" {
			return candidate
		}
	}
	if owner, _, ok := strings.Cut(payload.Repository.FullName, "/"); ok {
		return owner
	}
	return ""
}

// ---------------------------------------------------------------------------
// The runner pool
// ---------------------------------------------------------------------------

// workflowJobPayload is the part of a workflow_job delivery the pool reads.
type workflowJobPayload struct {
	Action string `json:"action"`
}

// runnerPool wakes a group's runner service when a job queues and puts it back
// to sleep after a quiet spell.
//
// Gitea's own runner status is not the truth — the API says `online` after the
// process is gone (ledger 2026-09-16) — so the pool tracks only what it did
// itself.
type runnerPool struct {
	zerops    *zerops.Client
	log       *slog.Logger
	clientID  string
	projectID string
	quiet     time.Duration

	mu     sync.Mutex
	timers map[string]*time.Timer
}

func newRunnerPool(z *zerops.Client, log *slog.Logger, clientID, projectID string, quiet time.Duration) *runnerPool {
	if quiet <= 0 {
		quiet = 15 * time.Minute
	}
	return &runnerPool{
		zerops: z, log: log, clientID: clientID, projectID: projectID,
		quiet: quiet, timers: map[string]*time.Timer{},
	}
}

func (p *runnerPool) handle(ctx context.Context, org string, raw []byte) {
	if org == "" {
		return
	}
	var payload workflowJobPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	switch payload.Action {
	case "queued":
		p.wake(ctx, org)
	case "completed":
		p.sleepAfterQuietSpell(org)
	}
}

// wake starts the group's runner service if it is stopped. A job that queues
// with no runner online waits: a runner registered afterwards takes it within a
// second of starting (ledger 2026-09-16).
func (p *runnerPool) wake(ctx context.Context, org string) {
	p.cancelSleep(org)

	service, ok := p.service(ctx, org)
	if !ok {
		return
	}
	if service.Status == "ACTIVE" || service.Status == "STARTING" {
		return
	}
	if err := p.zerops.StartService(ctx, service.ID); err != nil {
		p.log.Error("the group's runner could not be started", "org", org, "err", err.Error())
		return
	}
	p.log.Info("the group's runner was started", "org", org)
}

// sleepAfterQuietSpell arms a timer; another job within the spell cancels it.
func (p *runnerPool) sleepAfterQuietSpell(org string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if timer, armed := p.timers[org]; armed {
		timer.Stop()
	}
	p.timers[org] = time.AfterFunc(p.quiet, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		p.sleep(ctx, org)
	})
}

func (p *runnerPool) cancelSleep(org string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if timer, armed := p.timers[org]; armed {
		timer.Stop()
		delete(p.timers, org)
	}
}

func (p *runnerPool) sleep(ctx context.Context, org string) {
	p.mu.Lock()
	delete(p.timers, org)
	p.mu.Unlock()

	service, ok := p.service(ctx, org)
	if !ok || service.Status == "STOPPED" || service.Status == "STOPPING" {
		return
	}
	if err := p.zerops.StopService(ctx, service.ID); err != nil {
		p.log.Error("the group's runner could not be stopped", "org", org, "err", err.Error())
		return
	}
	p.log.Info("the group's runner was stopped after a quiet spell", "org", org)
}

func (p *runnerPool) service(ctx context.Context, org string) (zerops.Service, bool) {
	hostname := registry.RunnerHostname(org)
	services, err := p.zerops.Services(ctx, p.clientID, p.projectID)
	if err != nil {
		p.log.Error("the Gitea project's services could not be read", "err", err.Error())
		return zerops.Service{}, false
	}
	for _, s := range services {
		if s.Name == hostname {
			return s, true
		}
	}
	// A group whose first workflow has not run yet has no runner service; the
	// next brief imports one here.
	p.log.Info("the group has no runner service yet", "org", org, "hostname", hostname)
	return zerops.Service{}, false
}

// stop cancels every armed timer. The broker calls it on shutdown so a pending
// sleep does not outlive the process it belongs to.
func (p *runnerPool) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for org, timer := range p.timers {
		timer.Stop()
		delete(p.timers, org)
	}
}
