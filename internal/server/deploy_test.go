package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// fakeDeploys stands in for the pipeline: it records what the routes proved
// and passed on, and answers what a test armed it with — so the routes are
// judged on the proof, the pass-through and how a refusal is spelled, not on a
// whole account (internal/pipeline/grant_test.go judges the decision).
type fakeDeploys struct {
	grant    deploy.Grant
	err      error
	asked    []deploy.GrantRequest
	reported []string
}

func (f *fakeDeploys) Grant(_ context.Context, req deploy.GrantRequest) (deploy.Grant, error) {
	f.asked = append(f.asked, req)
	return f.grant, f.err
}

func (f *fakeDeploys) Result(_ context.Context, id, repository, outcome, message string) error {
	f.reported = append(f.reported, id+" "+repository+" "+outcome+" "+message)
	return f.err
}

type deployRig struct {
	gitea   *giteatest.Fake
	deploys *fakeDeploys
	handler http.Handler
}

// newDeployRig is a Gitea holding one running job in acme/api (task 42 of run
// 7), and a person's token that is no job at all.
func newDeployRig(t *testing.T) *deployRig {
	t.Helper()
	g := giteatest.New(t)
	g.AddRepo("acme/api", "main")
	g.AddRepo("acme/other", "main")

	// The synthetic user a job's token resolves to, measured 2026-09-16.
	g.AddUser(gitea.User{ID: -2, Login: "gitea-actions", LoginName: "@gitea-actions/42", Active: true})
	g.AddToken("gitea-actions", "job-42", "the-job-token", "all")
	// The job is 200 only for the repository it really runs in.
	g.AddJob("acme", "api", "42", gitea.Job{ID: 42, RunID: 7, HeadSHA: "3f9c", HeadBranch: "main"})
	g.AddUser(gitea.User{ID: 5, Login: "u-jan", Active: true})
	g.AddToken("u-jan", "personal", "a-persons-token", "all")

	deploys := &fakeDeploys{grant: deploy.Grant{
		ID: "d_1", Status: deploy.GrantGranted, Environment: "stage", Service: "api", Sha: "3f9c",
		Token: "the-stage-key", ProjectID: "p-stage", ServiceID: "svc-api", Setup: "api", VersionName: "3f9c",
	}}
	cfg := &config.Config{
		ZeropsClientID: clientID, ZeropsProjectID: giteaPrj,
		GiteaURL: g.URL(), GiteaPublicURL: "https://" + giteaHost,
		GiteaWebhookSecret: "the-webhook-secret",
		BrokerPublicURL:    "https://broker.example",
		MateAppURL:         "https://app.example",
	}
	s := New(cfg, slog.New(slog.DiscardHandler), Deps{Gitea: g.Client(), Deploys: deploys})
	t.Cleanup(s.Close)
	return &deployRig{gitea: g, deploys: deploys, handler: s.Handler()}
}

func (r *deployRig) post(path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	rr := httptest.NewRecorder()
	r.handler.ServeHTTP(rr, req)
	return rr
}

func errorOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not a refusal: %s", rr.Body.String())
	}
	return body.Error
}

const grantBody = `{"repository":"acme/api","sha":"3f9c","environment":"stage","service":"api"}`

// TestAGrantPassesOnWhatTheJobProvedNotWhatItSaid — the owner, the repository,
// the run and the task come from Gitea's answer to the job's own token; the
// commit and the environment are the job's word, which the pipeline judges.
func TestAGrantPassesOnWhatTheJobProvedNotWhatItSaid(t *testing.T) {
	r := newDeployRig(t)
	rr := r.post("/deploy/grant", "the-job-token", grantBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	want := deploy.GrantRequest{Owner: "acme", Repo: "api", RunID: 7, TaskID: "42",
		Sha: "3f9c", Environment: "stage", Service: "api"}
	if len(r.deploys.asked) != 1 || r.deploys.asked[0] != want {
		t.Fatalf("the pipeline was asked %+v, want %+v", r.deploys.asked, want)
	}
	var grant deploy.Grant
	if err := json.Unmarshal(rr.Body.Bytes(), &grant); err != nil || grant != r.deploys.grant {
		t.Fatalf("answered %s", rr.Body.String())
	}
	// The one answer that carries a key.
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestAGrantsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		token  string
		body   string
		err    error
		status int
		code   string
	}{
		{name: "no token at all", body: grantBody, status: http.StatusUnauthorized, code: "not_a_job"},
		{name: "a person's token", token: "a-persons-token", body: grantBody, status: http.StatusUnauthorized, code: "not_a_job"},
		{
			name: "a job claiming another repository", token: "the-job-token",
			body:   `{"repository":"acme/other","sha":"3f9c"}`,
			status: http.StatusForbidden, code: "wrong_repository",
		},
		{
			name: "a job that names no commit", token: "the-job-token",
			body:   `{"repository":"acme/api"}`,
			status: http.StatusBadRequest, code: "invalid_request",
		},
		{name: "a body that is not one", token: "the-job-token", body: `{`, status: http.StatusBadRequest, code: "invalid_request"},
		{
			name: "what the pipeline refuses, as it spelled it", token: "the-job-token", body: grantBody,
			err:    deploy.Refuse(http.StatusForbidden, "untrusted_ref", "only the default branch's own workflow deploys"),
			status: http.StatusForbidden, code: "untrusted_ref",
		},
		{
			name: "a fault of the broker's own", token: "the-job-token", body: grantBody,
			err:    errors.New("boom"),
			status: http.StatusInternalServerError, code: "internal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.deploys.err = tc.err
			rr := r.post("/deploy/grant", tc.token, tc.body)
			if rr.Code != tc.status || errorOf(t, rr) != tc.code {
				t.Fatalf("answered %d %s, want %d %s", rr.Code, rr.Body.String(), tc.status, tc.code)
			}
			if strings.Contains(rr.Body.String(), "the-stage-key") {
				t.Fatal("a refusal carried the key")
			}
		})
	}
}

// TestAJobsReport — the report goes through the same proof, and carries the
// repository Gitea proved rather than any the job could name for another's
// deploy.
func TestAJobsReport(t *testing.T) {
	r := newDeployRig(t)
	rr := r.post("/deploy/d_1/result", "the-job-token", `{"repository":"acme/api","status":"success","message":""}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if len(r.deploys.reported) != 1 || r.deploys.reported[0] != "d_1 acme/api success " {
		t.Fatalf("reported %v", r.deploys.reported)
	}

	for _, tc := range []struct {
		name   string
		token  string
		body   string
		err    error
		status int
		code   string
	}{
		{name: "a status that is neither", token: "the-job-token", body: `{"repository":"acme/api","status":"maybe"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "a person's token", token: "a-persons-token", body: `{"repository":"acme/api","status":"success"}`, status: http.StatusUnauthorized, code: "not_a_job"},
		{
			name: "a grant the broker no longer holds", token: "the-job-token",
			body:   `{"repository":"acme/api","status":"success"}`,
			err:    deploy.Refuse(http.StatusNotFound, "unknown_deploy", "the broker no longer holds d_1"),
			status: http.StatusNotFound, code: "unknown_deploy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.deploys.err = tc.err
			rr := r.post("/deploy/d_1/result", tc.token, tc.body)
			if rr.Code != tc.status || errorOf(t, rr) != tc.code {
				t.Fatalf("answered %d %s, want %d %s", rr.Code, rr.Body.String(), tc.status, tc.code)
			}
		})
	}
}

// fakeRunners records which groups were asked for a runner.
type fakeRunners struct {
	asked []string
	err   error
}

func (f *fakeRunners) EnsureRunner(_ context.Context, org string) error {
	f.asked = append(f.asked, org)
	return f.err
}

// TestAQueuedJobImportsTheGroupsFirstRunner is guide 1.6: a group whose first
// workflow appears gets its runner service imported, and one that already has
// one is only started.
func TestAQueuedJobImportsTheGroupsFirstRunner(t *testing.T) {
	r := newRig(t)
	runners := &fakeRunners{}
	r.server.deps.Runners = runners
	r.server.runners.importRunner = r.server.ensureRunner

	r.server.runners.handle(context.Background(), "acme", []byte(`{"action":"queued"}`))
	if len(runners.asked) != 1 || runners.asked[0] != "acme" {
		t.Fatalf("the importer was asked for %v, want acme once", runners.asked)
	}

	// With the service in the project, the pool starts it and imports nothing.
	r.zerops.SetServices(giteaPrj, zerops.Service{
		ID: "svc-runner", ProjectID: giteaPrj, Name: registry.RunnerHostname("acme"), Status: "STOPPED",
	})
	r.server.runners.handle(context.Background(), "acme", []byte(`{"action":"queued"}`))
	if len(runners.asked) != 1 {
		t.Fatalf("a group that already has a runner was imported again: %v", runners.asked)
	}
	if !r.zerops.Started("svc-runner") {
		t.Fatal("the existing runner was not started")
	}
}
