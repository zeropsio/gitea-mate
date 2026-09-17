package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

const (
	clientID  = "org-1"
	giteaPrj  = "p-gitea"
	giteaHost = "git.example"
)

var now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

type rig struct {
	zerops  *zeropstest.Fake
	gitea   *giteatest.Fake
	server  *Server
	handler http.Handler
}

// newRig is one org whose Gitea already carries the group the rights loop would
// have built: the acme org, its three teams, and nothing else.
func newRig(t *testing.T) *rig {
	t.Helper()
	z := zeropstest.New(t, clientID)
	z.Now = now
	z.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: clientID})
	z.AddToken(zerops.Token{ID: "tok-broker", Name: "mate-broker", RoleCode: "READ_ONLY"})

	// Jan owns the Mate; Olga owns the org; Vera reads only.
	for _, m := range []zerops.Member{
		{ID: "cu-jan", UserID: "u-jan", Status: "ACTIVE", RoleCode: "READ_ONLY", CanCreateProjects: true,
			User: zerops.UserLight{ID: "u-jan", Email: "jan@example", FullName: "Jan"}},
		{ID: "cu-olga", UserID: "u-olga", Status: "ACTIVE", RoleCode: "OWNER",
			User: zerops.UserLight{ID: "u-olga", Email: "olga@example", FullName: "Olga"}},
		{ID: "cu-vera", UserID: "u-vera", Status: "ACTIVE", RoleCode: "READ_ONLY",
			User: zerops.UserLight{ID: "u-vera", Email: "vera@example", FullName: "Vera"}},
	} {
		z.AddMember(m)
	}
	z.SetProjects(
		zerops.Project{ID: giteaPrj, Name: "gitea", TagList: []string{
			"mate:tool:gitea",
			"mate:gn:g-acme:acme",
			"mate:gm:g-acme:p-fen:mate",
			"mate:gm:g-acme:p-prod:production",
		}},
		zerops.Project{ID: "p-fen", Name: "Fen", UserRoles: []zerops.UserRole{{ClientUserID: "cu-jan", RoleCode: "OWNER"}}},
		zerops.Project{ID: "p-prod", Name: "Acme production"},
		zerops.Project{ID: "p-stray", Name: "Something else"},
	)

	g := giteatest.New(t)
	gc := g.Client()
	ctx := context.Background()
	if _, err := gc.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	for _, spec := range []gitea.NewTeam{
		{Name: "read", Permission: "read", UnitsMap: gitea.AllRepoUnits("read")},
		{Name: "write", Permission: "write", UnitsMap: gitea.AllRepoUnits("write")},
		{Name: "release", Permission: "write", UnitsMap: gitea.AllRepoUnits("write")},
	} {
		if _, err := gc.CreateTeam(ctx, "acme", spec); err != nil {
			t.Fatalf("CreateTeam(%s): %v", spec.Name, err)
		}
	}

	cfg := &config.Config{
		ZeropsClientID: clientID, ZeropsProjectID: giteaPrj,
		GiteaURL: g.URL(), GiteaPublicURL: "https://" + giteaHost,
		GiteaWebhookSecret: "the-webhook-secret",
		BrokerPublicURL:    "https://broker.example",
		MateAppURL:         "https://app.example",
		RunnerQuietPeriod:  50 * time.Millisecond,
	}
	s := New(cfg, slog.New(slog.DiscardHandler), Deps{
		Zerops: z.Client("broker"),
		Gitea:  gc,
	})
	t.Cleanup(s.Close)
	return &rig{zerops: z, gitea: g, server: s, handler: s.Handler()}
}

func (r *rig) do(req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	r.handler.ServeHTTP(rr, req)
	return rr
}

// A Mate's Gitea access is no route: the rights loop delivers it (D20). The
// old POST /mate/credential answers 404 like any path the broker never had.
func TestThereIsNoCredentialRoute(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest(http.MethodPost, "/mate/credential", strings.NewReader(`{"project":"p-fen"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer anything")
	if rr := r.do(req); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body)
	}
}

// ---------------------------------------------------------------------------
// POST /mate/repository
// ---------------------------------------------------------------------------

func (r *rig) repository(token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mate/repository", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	return r.do(req)
}

// botToken seeds Fen's bot and a live token the way a pass of the rights loop
// leaves them, and returns the token's value.
func (r *rig) botToken(t *testing.T) string {
	t.Helper()
	const value = "value-mate-p-fen-1"
	r.gitea.AddUser(gitea.User{Login: "mate-p-fen", Active: true, Restricted: true})
	r.gitea.AddToken("mate-p-fen", mirror.TokenName("mate-p-fen", 1), value, mirror.BotScopes...)
	return value
}

func TestRepositoryIsIdempotent(t *testing.T) {
	r := newRig(t)
	token := r.botToken(t)

	rr := r.repository(token, `{"name":"api"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	var first repositoryResponse
	decode(t, rr, &first)
	if !first.Created || first.FullName != "acme/api" || first.DefaultBranch != "main" {
		t.Fatalf("first = %+v", first)
	}
	// The clone URL never carries a .git suffix: the platform's clone preflight
	// fails on one.
	if first.CloneURL != "https://"+giteaHost+"/acme/api" || strings.HasSuffix(first.CloneURL, ".git") {
		t.Errorf("cloneUrl = %q", first.CloneURL)
	}
	// main is protected before anything is pushed.
	rules := r.gitea.BranchRules("acme", "api")
	if len(rules) != 1 || rules[0].RuleName != "main" || rules[0].EnablePush {
		t.Errorf("rules = %+v", rules)
	}
	if len(rules) == 1 && (len(rules[0].MergeWhitelistTeams) != 1 || rules[0].MergeWhitelistTeams[0] != "write") {
		t.Errorf("merges by = %v; a service repository takes merges from the write team", rules[0].MergeWhitelistTeams)
	}

	// A repository the bot already collaborates on answers 200 with created:false.
	rr = r.repository(token, `{"name":"api"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second status = %d: %s", rr.Code, rr.Body)
	}
	var second repositoryResponse
	decode(t, rr, &second)
	if second.Created || second.FullName != "acme/api" {
		t.Errorf("second = %+v", second)
	}
}

func TestRepositoryRefusals(t *testing.T) {
	t.Run("a repository the bot does not collaborate on is taken", func(t *testing.T) {
		r := newRig(t)
		token := r.botToken(t)
		if _, err := r.gitea.Client().CreateOrgRepo(context.Background(), "acme", gitea.NewRepo{Name: "group"}); err != nil {
			t.Fatalf("CreateOrgRepo: %v", err)
		}
		rr := r.repository(token, `{"name":"group"}`)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body)
		}
		var body ErrorBody
		decode(t, rr, &body)
		if body.Error != "taken" {
			t.Errorf("error = %q", body.Error)
		}
	})

	t.Run("a token Gitea does not know", func(t *testing.T) {
		r := newRig(t)
		rr := r.repository("nobody", `{"name":"api"}`)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body)
		}
		var body ErrorBody
		decode(t, rr, &body)
		if body.Error != "not_a_bot" {
			t.Errorf("error = %q", body.Error)
		}
	})

	t.Run("no token at all", func(t *testing.T) {
		r := newRig(t)
		rr := r.repository("", `{"name":"api"}`)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rr.Code)
		}
	})

	t.Run("a person's token, not a bot's", func(t *testing.T) {
		r := newRig(t)
		r.gitea.AddUser(gitea.User{Login: "u-ujan", Active: true})
		r.gitea.AddToken("u-ujan", "personal", "jan-gitea-token", "write:repository", "read:user")
		rr := r.repository("jan-gitea-token", `{"name":"api"}`)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body)
		}
		var body ErrorBody
		decode(t, rr, &body)
		if body.Error != "not_a_bot" {
			t.Errorf("error = %q", body.Error)
		}
	})

	t.Run("a bot whose project is not registered", func(t *testing.T) {
		r := newRig(t)
		r.gitea.AddUser(gitea.User{Login: "mate-p-stray", Active: true, Restricted: true})
		r.gitea.AddToken("mate-p-stray", "mate/mate-p-stray/1", "stray-token", "write:repository", "read:user")
		rr := r.repository("stray-token", `{"name":"api"}`)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body)
		}
		var body ErrorBody
		decode(t, rr, &body)
		if body.Error != "not_registered" {
			t.Errorf("error = %q", body.Error)
		}
	})

	t.Run("a name no service repository may have", func(t *testing.T) {
		r := newRig(t)
		token := r.botToken(t)
		for _, name := range []string{"API", "1api", "-api", "a/b", strings.Repeat("a", 41), ""} {
			rr := r.repository(token, `{"name":"`+name+`"}`)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("name %q = %d, want 400", name, rr.Code)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// POST /hooks/gitea
// ---------------------------------------------------------------------------

func (r *rig) hook(t *testing.T, event, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/gitea", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", event)
	if signature == "" {
		mac := hmac.New(sha256.New, []byte("the-webhook-secret"))
		mac.Write([]byte(body))
		signature = hex.EncodeToString(mac.Sum(nil))
	}
	req.Header.Set("X-Gitea-Signature", signature)
	return r.do(req)
}

func TestHookSignature(t *testing.T) {
	r := newRig(t)
	body := `{"action":"queued","repository":{"full_name":"acme/api","owner":{"username":"acme"}}}`

	for _, tc := range []struct {
		name, signature string
		wantStatus      int
	}{
		{"a good signature", "", http.StatusNoContent},
		{"a wrong signature", strings.Repeat("ab", 32), http.StatusUnauthorized},
		{"no signature", "-", http.StatusUnauthorized},
		{"a signature that is not hex", "not-hex", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signature := tc.signature
			if signature == "-" {
				signature = ""
			}
			req := httptest.NewRequest(http.MethodPost, "/hooks/gitea", strings.NewReader(body))
			req.Header.Set("X-Gitea-Event", "workflow_job")
			if tc.signature != "" {
				req.Header.Set("X-Gitea-Signature", signature)
			} else {
				mac := hmac.New(sha256.New, []byte("the-webhook-secret"))
				mac.Write([]byte(body))
				req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
			}
			rr := r.do(req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body)
			}
			if tc.wantStatus == http.StatusUnauthorized {
				var body ErrorBody
				decode(t, rr, &body)
				if body.Error != "bad_signature" {
					t.Errorf("error = %q", body.Error)
				}
			}
		})
	}
}

// Whatever the payload, a signed delivery is 204 and the work runs after the
// response.
func TestHookAlwaysAnswers204(t *testing.T) {
	r := newRig(t)
	for _, event := range []string{"push", "create", "delete", "pull_request", "workflow_run", "issues", ""} {
		rr := r.hook(t, event, `{"repository":{"owner":{"username":"acme"}}}`, "")
		if rr.Code != http.StatusNoContent {
			t.Errorf("event %q = %d, want 204", event, rr.Code)
		}
		if rr.Body.Len() != 0 {
			t.Errorf("event %q answered a body: %s", event, rr.Body)
		}
	}
}

func TestWorkflowJobWakesAndSleepsTheRunner(t *testing.T) {
	r := newRig(t)
	r.zerops.SetServices(giteaPrj,
		zerops.Service{ID: "s-web", ProjectID: giteaPrj, Name: "web", Status: "ACTIVE"},
		zerops.Service{ID: "s-runner", ProjectID: giteaPrj, Name: "runneracme", Status: "STOPPED"},
	)

	body := `{"action":"queued","repository":{"full_name":"acme/api","owner":{"username":"acme"}}}`
	if rr := r.hook(t, "workflow_job", body, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("queued = %d", rr.Code)
	}
	waitFor(t, func() bool { return !r.zerops.IsStopped("s-runner") && r.zerops.Started("s-runner") })

	// A completed job arms the quiet spell; the rig's is 50 ms.
	done := `{"action":"completed","repository":{"full_name":"acme/api","owner":{"username":"acme"}}}`
	if rr := r.hook(t, "workflow_job", done, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("completed = %d", rr.Code)
	}
	waitFor(t, func() bool { return r.zerops.IsStopped("s-runner") })
}

// A group with no runner service yet is simply noted; nothing is started and
// nothing fails.
func TestWorkflowJobWithNoRunnerService(t *testing.T) {
	r := newRig(t)
	body := `{"action":"queued","repository":{"full_name":"acme/api","owner":{"username":"acme"}}}`
	if rr := r.hook(t, "workflow_job", body, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("queued = %d", rr.Code)
	}
	time.Sleep(50 * time.Millisecond)
}

// The OIDC routes are on the same router when a provider is wired.
func TestOIDCRoutesAreNotServedWithoutAProvider(t *testing.T) {
	r := newRig(t)
	rr := r.do(httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no provider is wired", rr.Code)
	}
}

// ---------------------------------------------------------------------------

func decode(t *testing.T, rr *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), into); err != nil {
		t.Fatalf("body %q: %v", rr.Body.String(), err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the condition never held")
}
