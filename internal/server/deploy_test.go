package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// fakeDeploys stands in for the pipeline: it answers with a fixed plan and
// records what it was asked to deploy, so the routes are judged on their
// refusals and their answer, not on a whole account.
type fakeDeploys struct {
	plan  deploy.Plan
	asked []string
}

func (f *fakeDeploys) Plan(context.Context, string) (deploy.Plan, error) { return f.plan, nil }

func (f *fakeDeploys) Deploy(_ context.Context, _ deploy.Plan, env environments.Environment, service string, records []string) error {
	f.asked = append(f.asked, env.Name+"/"+service)
	return nil
}

const deployEnvironments = `
version: 1
environments:
  stage:
    tier: stage
    project: p-stage
    sources: [main]
`

const deployTier = `
services:
  - hostname: db
    type: postgresql@18
  - hostname: api
    type: nodejs@22
    buildFromGit: https://git.example/acme/api
    zeropsSetup: api
`

type deployRig struct {
	gitea   *giteatest.Fake
	records *deploy.Records
	deploys *fakeDeploys
	handler http.Handler
}

// newDeployRig is one group with one stage, and a Gitea holding one running
// job in acme/api.
func newDeployRig(t *testing.T) *deployRig {
	t.Helper()
	return newDeployRigWith(t, deployEnvironments)
}

// newDeployRigWith is the rig over one environments document.
func newDeployRigWith(t *testing.T, environmentsYAML string) *deployRig {
	t.Helper()
	g := giteatest.New(t)
	g.AddRepo("acme/api", "main")
	g.AddRepo("acme/other", "main")

	// The synthetic user a job's token resolves to, measured 2026-09-16.
	g.AddUser(gitea.User{ID: -2, Login: "gitea-actions", LoginName: "@gitea-actions/42", Active: true})
	g.AddToken("gitea-actions", "job-42", "the-job-token", "all")
	// The job is 200 only for the repository it really runs in.
	g.AddJob("acme", "api", "42", gitea.Job{ID: 42, RunID: 7, HeadSHA: "3f9c", HeadBranch: "main"})
	// A person's token, which is not a job at all.
	g.AddUser(gitea.User{ID: 5, Login: "u-jan", Active: true})
	g.AddToken("u-jan", "personal", "a-persons-token", "all")

	file, err := environments.Parse([]byte(environmentsYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	recipe, err := environments.ParseRecipe([]byte(deployTier))
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}

	deploys := &fakeDeploys{plan: deploy.Plan{
		Slug: "acme", File: file,
		Recipes: map[environments.Tier]environments.Recipe{environments.TierStage: recipe},
	}}
	records := deploy.NewRecords(0)
	cfg := &config.Config{
		ZeropsClientID: clientID, ZeropsProjectID: giteaPrj,
		GiteaURL: g.URL(), GiteaPublicURL: "https://" + giteaHost,
		GiteaWebhookSecret: "the-webhook-secret",
		BrokerPublicURL:    "https://broker.example",
		MateAppURL:         "https://app.example",
	}
	s := New(cfg, slog.New(slog.DiscardHandler), Deps{
		Gitea: g.Client(), Deploys: deploys, Records: records,
	})
	t.Cleanup(s.Close)
	return &deployRig{gitea: g, records: records, deploys: deploys, handler: s.Handler()}
}

func (r *deployRig) post(token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	rr := httptest.NewRecorder()
	r.handler.ServeHTTP(rr, req)
	return rr
}

func (r *deployRig) get(token, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/deploy/"+id, nil)
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

func TestDeployQueuesAndAnswersTheRecord(t *testing.T) {
	r := newDeployRig(t)

	rr := r.post("the-job-token", `{"environment":"stage","service":"api","repository":"acme/api"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /deploy = %d %s, want 202", rr.Code, rr.Body.String())
	}
	var record deploy.Record
	if err := json.Unmarshal(rr.Body.Bytes(), &record); err != nil {
		t.Fatalf("the answer is not a record: %s", rr.Body.String())
	}
	if !strings.HasPrefix(record.ID, "d_") || record.Environment != "stage" || record.Service != "api" {
		t.Fatalf("the answer is %+v", record)
	}
	if record.Status != deploy.StatusQueued {
		t.Fatalf("status = %q, want queued", record.Status)
	}
	if len(r.deploys.asked) != 1 || r.deploys.asked[0] != "stage/api" {
		t.Fatalf("the pipeline was asked for %v", r.deploys.asked)
	}

	// The same job can read it back.
	rr = r.get("the-job-token", record.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /deploy/{id} = %d %s, want 200", rr.Code, rr.Body.String())
	}
}

func TestDeployRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		token  string
		body   string
		status int
		code   string
	}{
		{
			"no token at all",
			"", `{"environment":"stage","service":"api","repository":"acme/api"}`,
			http.StatusUnauthorized, "not_a_job",
		},
		{
			"a token Gitea does not know",
			"nonsense", `{"environment":"stage","service":"api","repository":"acme/api"}`,
			http.StatusUnauthorized, "not_a_job",
		},
		{
			"a person's token, not a job's",
			"a-persons-token", `{"environment":"stage","service":"api","repository":"acme/api"}`,
			http.StatusUnauthorized, "not_a_job",
		},
		{
			"a job claiming a repository it does not run in",
			"the-job-token", `{"environment":"stage","service":"api","repository":"acme/other"}`,
			http.StatusForbidden, "wrong_repository",
		},
		{
			"an environment the group does not declare",
			"the-job-token", `{"environment":"canary","service":"api","repository":"acme/api"}`,
			http.StatusNotFound, "unknown_environment",
		},
		{
			"a service the tier does not carry",
			"the-job-token", `{"environment":"stage","service":"ghost","repository":"acme/api"}`,
			http.StatusNotFound, "unknown_service",
		},
		{
			"a managed service, which is never deployed to",
			"the-job-token", `{"environment":"stage","service":"db","repository":"acme/api"}`,
			http.StatusNotFound, "unknown_service",
		},
		{
			"a body missing the repository",
			"the-job-token", `{"environment":"stage","service":"api"}`,
			http.StatusBadRequest, "invalid_request",
		},
		{
			"a body that is not json",
			"the-job-token", `{`,
			http.StatusBadRequest, "invalid_request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			rr := r.post(tc.token, tc.body)
			if rr.Code != tc.status {
				t.Fatalf("POST /deploy = %d %s, want %d", rr.Code, rr.Body.String(), tc.status)
			}
			if got := errorOf(t, rr); got != tc.code {
				t.Fatalf("the refusal is %q, want %q", got, tc.code)
			}
			if len(r.deploys.asked) != 0 {
				t.Fatalf("a refused request still queued %v", r.deploys.asked)
			}
		})
	}
}

// TestDeployStatusAfterARestart is the contract's fallback: the broker keeps
// deploys in memory, so an id it no longer holds is 404 and the action reads
// the commit status instead.
func TestDeployStatusAfterARestart(t *testing.T) {
	r := newDeployRig(t)
	rr := r.post("the-job-token", `{"environment":"stage","service":"api","repository":"acme/api"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /deploy = %d", rr.Code)
	}
	var record deploy.Record
	_ = json.Unmarshal(rr.Body.Bytes(), &record)

	// A restart: a fresh rig holds nothing, and the same id is gone.
	restarted := newDeployRig(t)
	rr = restarted.get("the-job-token", record.ID)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /deploy/{id} after a restart = %d %s, want 404", rr.Code, rr.Body.String())
	}
	if got := errorOf(t, rr); got != "unknown_deploy" {
		t.Fatalf("the refusal is %q", got)
	}
}

func TestDeployStatusProvesItsCaller(t *testing.T) {
	r := newDeployRig(t)
	rr := r.post("the-job-token", `{"environment":"stage","service":"api","repository":"acme/api"}`)
	var record deploy.Record
	_ = json.Unmarshal(rr.Body.Bytes(), &record)

	for _, tc := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{"no token", "", http.StatusUnauthorized, "not_a_job"},
		{"a person's token", "a-persons-token", http.StatusUnauthorized, "not_a_job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := r.get(tc.token, record.ID)
			if rr.Code != tc.status {
				t.Fatalf("GET /deploy/{id} = %d %s, want %d", rr.Code, rr.Body.String(), tc.status)
			}
			if got := errorOf(t, rr); got != tc.code {
				t.Fatalf("the refusal is %q, want %q", got, tc.code)
			}
		})
	}
}

// fakeRunners records the groups whose runner was asked for.
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

// A workflow asks for a tier's name (zcp writes `environment: stage`) while
// the app names the environment after the group (`todo-stage`): on the owner's
// run of 2026-09-17 every job's deploy answered 404 and only the catch-up pass
// deployed. A tier's name is its only environment; two of a tier need naming.
func TestDeployTakesATiersNameForItsOnlyEnvironment(t *testing.T) {
	const oneStage = `
version: 1
environments:
  acme-stage:
    tier: stage
    project: p-stage
    sources: [main]
`
	const twoStages = oneStage + `  acme-stage-2:
    tier: stage
    project: p-stage-2
    sources: [main]
`
	t.Run("the tier's only environment", func(t *testing.T) {
		r := newDeployRigWith(t, oneStage)
		rr := r.post("the-job-token", `{"environment":"stage","service":"api","repository":"acme/api"}`)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("POST /deploy = %d %s", rr.Code, rr.Body.String())
		}
		if len(r.deploys.asked) != 1 || r.deploys.asked[0] != "acme-stage/api" {
			t.Fatalf("asked = %v, want acme-stage/api", r.deploys.asked)
		}
	})
	t.Run("two environments of the tier", func(t *testing.T) {
		r := newDeployRigWith(t, twoStages)
		rr := r.post("the-job-token", `{"environment":"stage","service":"api","repository":"acme/api"}`)
		if rr.Code != http.StatusNotFound || errorOf(t, rr) != "unknown_environment" {
			t.Fatalf("POST /deploy = %d %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "acme-stage-2") || len(r.deploys.asked) != 0 {
			t.Fatalf("the refusal names both and queues nothing: %s %v", rr.Body.String(), r.deploys.asked)
		}
	})
}
