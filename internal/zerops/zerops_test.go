package zerops_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

const org = "org-1"

func newFake(t *testing.T) (*zeropstest.Fake, *zerops.Client) {
	t.Helper()
	f := zeropstest.New(t, org)
	f.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: org})
	f.AddToken(zerops.Token{ID: "tok-broker", Name: "mate-broker", RoleCode: "READ_ONLY", Created: f.Now})
	return f, f.Client("broker")
}

func TestUserInfo(t *testing.T) {
	f, c := newFake(t)
	f.Now = time.Date(2026, 9, 16, 9, 30, 0, 0, time.UTC)

	got, err := c.UserInfo(context.Background())
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if got.ID != "tok-broker" {
		t.Errorf("id = %q", got.ID)
	}
	if len(got.Clients) != 1 || got.Clients[0].ClientID != org {
		t.Errorf("clientUserList = %+v", got.Clients)
	}
	// Every call captures the API's clock — the throwaway check reads it and
	// never the container's.
	if !got.APIDate.Equal(f.Now) {
		t.Errorf("APIDate = %v, want %v", got.APIDate, f.Now)
	}
}

func TestIntegrationTokenIsOrgScoped(t *testing.T) {
	f, c := newFake(t)
	f.AddToken(zerops.Token{ID: "tok-throw", Name: "gitea-signin:git.example:abc", RoleCode: "NO_ACCESS", Created: f.Now})

	got, err := c.IntegrationToken(context.Background(), org, "tok-throw")
	if err != nil {
		t.Fatalf("IntegrationToken: %v", err)
	}
	if got.Name != "gitea-signin:git.example:abc" || !got.APIDate.Equal(f.Now) {
		t.Errorf("token = %+v", got)
	}

	// A token of another org reading this org's tokens is the wrong_org step.
	f.AddIdentity("stranger", zeropstest.Identity{UserInfoID: "tok-x", TokenID: "tok-x", ClientID: "org-2"})
	_, err = f.Client("stranger").IntegrationToken(context.Background(), org, "tok-throw")
	if zerops.Status(err) != http.StatusForbidden {
		t.Errorf("stranger got %v, want 403", err)
	}
}

func TestTokenLifecycle(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()

	minted, err := c.CreateIntegrationToken(ctx, org, zerops.TokenSpec{
		Name: "probe", RoleCode: "NO_ACCESS",
		Projects: []zerops.ProjectAccess{{ProjectID: "p-1", RoleCode: "BASIC_USER"}},
	})
	if err != nil {
		t.Fatalf("CreateIntegrationToken: %v", err)
	}
	if minted.Value == "" {
		t.Fatal("a minted token carries no value")
	}

	if _, err := c.UpdateIntegrationToken(ctx, org, minted.ID, zerops.TokenSpec{
		Name: "probe", RoleCode: "NO_ACCESS",
		Projects: []zerops.ProjectAccess{
			{ProjectID: "p-1", RoleCode: "BASIC_USER"},
			{ProjectID: "p-2", RoleCode: "BASIC_USER"},
		},
	}); err != nil {
		t.Fatalf("UpdateIntegrationToken: %v", err)
	}
	after, err := c.IntegrationToken(ctx, org, minted.ID)
	if err != nil || len(after.Projects) != 2 {
		t.Fatalf("after update: %+v %v", after, err)
	}

	if err := c.DeleteIntegrationToken(ctx, org, minted.ID); err != nil {
		t.Fatalf("DeleteIntegrationToken: %v", err)
	}
	if _, err := c.IntegrationToken(ctx, org, minted.ID); zerops.Status(err) != http.StatusNotFound {
		t.Errorf("after delete: %v, want 404", err)
	}
	if got := f.Deleted; len(got) != 1 || got[0] != minted.ID {
		t.Errorf("deleted = %v", got)
	}
}

func TestMembers(t *testing.T) {
	f, c := newFake(t)
	f.AddMember(zerops.Member{ID: "cu-1", UserID: "u-jan", Status: "ACTIVE", RoleCode: "READ_ONLY", CanCreateProjects: true})
	f.AddMember(zerops.Member{ID: "cu-2", UserID: "u-ana", Status: "INVITED", RoleCode: "ADMIN"})

	got, err := c.Members(context.Background(), org)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got.Members) != 2 || got.Members[0].UserID != "u-jan" || got.Members[1].Status != "INVITED" {
		t.Errorf("members = %+v", got.Members)
	}
}

func TestSearchProjectsRefusesAPartialPage(t *testing.T) {
	f, c := newFake(t)
	f.SetProjects(
		zerops.Project{ID: "p-1", Name: "Fen", TagList: []string{"mate:g:g-acme"}},
		zerops.Project{ID: "p-2", Name: "Gitea", TagList: []string{"mate:tool:gitea"}},
	)

	got, err := c.SearchProjects(context.Background(), org)
	if err != nil {
		t.Fatalf("SearchProjects: %v", err)
	}
	if len(got.Projects) != 2 || got.Total != 2 {
		t.Fatalf("projects = %d of %d", len(got.Projects), got.Total)
	}

	// A page short of its declared total is never "the org shrank".
	f.TruncateProjectSearch = true
	_, err = c.SearchProjects(context.Background(), org)
	if !zerops.IsPartial(err) {
		t.Errorf("truncated search: %v, want a partial read", err)
	}
}

func TestProjectAndTagWrite(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	f.SetProjects(zerops.Project{
		ID: "p-gitea", Name: "gitea", TagList: []string{"mate:tool:gitea"},
		UserRoles: []zerops.UserRole{{ClientUserID: "cu-1", RoleCode: "OWNER"}},
	})

	got, err := c.Project(ctx, "p-gitea")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(got.UserRoles) != 1 || got.UserRoles[0].RoleCode != "OWNER" {
		t.Errorf("userRoles = %+v", got.UserRoles)
	}

	// PUT /project/{id} sends name, description, tagList, publicIpV4Shared and
	// maxCreditLimit — never userRoles, which would rewrite the overrides.
	if err := c.UpdateProject(ctx, "p-gitea", zerops.ProjectUpdate{
		Name:    "gitea",
		TagList: []string{"mate:tool:gitea", "mate:gn:g-acme:acme"},
	}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	after, _ := f.Project("p-gitea")
	if len(after.TagList) != 2 {
		t.Errorf("tagList = %v", after.TagList)
	}
}

func TestUpdateProjectNeverSendsUserRoles(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		body = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := zerops.New(srv.URL, "t", srv.Client())
	if err := c.UpdateProject(context.Background(), "p-1", zerops.ProjectUpdate{Name: "x"}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if strings.Contains(body, "userRoles") {
		t.Errorf("the body carries userRoles: %s", body)
	}
	for _, want := range []string{"name", "description", "tagList", "publicIpV4Shared", "maxCreditLimit"} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lacks %q: %s", want, body)
		}
	}
}

func TestProjectEnvs(t *testing.T) {
	f, c := newFake(t)
	f.SetProjects(zerops.Project{ID: "p-1", EnvList: []zerops.ProjectEnv{
		{ID: "env-1", Key: "ZCP_API_KEY", Sensitive: true},
	}})
	got, err := c.ProjectEnvs(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("ProjectEnvs: %v", err)
	}
	if len(got) != 1 || got[0].ID != "env-1" {
		t.Errorf("envs = %+v", got)
	}
}

func TestServicesAndStopStart(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	f.SetServices("p-gitea",
		zerops.Service{ID: "s-web", ProjectID: "p-gitea", Name: "web", Status: "ACTIVE"},
		zerops.Service{ID: "s-runner", ProjectID: "p-gitea", Name: "runneracme", Status: "STOPPED"},
	)

	got, err := c.Services(ctx, org, "p-gitea")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("services = %+v", got)
	}

	if err := c.StartService(ctx, "s-runner"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	if f.IsStopped("s-runner") {
		t.Error("s-runner is still stopped")
	}
	if err := c.StopService(ctx, "s-runner"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
	if !f.IsStopped("s-runner") {
		t.Error("s-runner was not stopped")
	}
}

func TestImportServices(t *testing.T) {
	f, c := newFake(t)
	yaml := "services:\n  - hostname: runneracme\n"
	if _, err := c.ImportServices(context.Background(), "p-gitea", yaml); err != nil {
		t.Fatalf("ImportServices: %v", err)
	}
	if len(f.Imports) != 1 || f.Imports[0].ProjectID != "p-gitea" || f.Imports[0].Yaml != yaml {
		t.Errorf("imports = %+v", f.Imports)
	}
}

func TestAPIErrorCarriesTheCode(t *testing.T) {
	f, c := newFake(t)
	f.Fail["GET /client/"+org+"/user/list"] = http.StatusInternalServerError

	_, err := c.Members(context.Background(), org)
	if zerops.Status(err) != 500 || zerops.Code(err) != "forced" {
		t.Errorf("err = %v (status %d, code %q)", err, zerops.Status(err), zerops.Code(err))
	}
}

// The platform wraps a refusal in an error envelope (measured 2026-09-24 on
// GET /project/{id} of a deleted project); a gateway's page is not JSON.
func TestAPIErrorReadsThePlatformsEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, message string
		status                    int
	}{
		{
			name:    "a deleted project",
			status:  http.StatusBadRequest,
			body:    `{"error":{"code":"projectNotFound","message":"Project not found.","meta":[{"error":"Project not found.","code":"projectNotFound","metadata":null}]}}`,
			code:    "projectNotFound",
			message: "Project not found.",
		},
		{
			name:   "a gateway's page",
			status: http.StatusBadGateway,
			body:   `<html><body>502 Bad Gateway</body></html>`,
			code:   "http_502",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			_, err := zerops.New(srv.URL, "the-token", srv.Client()).Project(context.Background(), "p-1")
			if zerops.Status(err) != tc.status || zerops.Code(err) != tc.code {
				t.Errorf("err = %v (status %d, code %q), want %d %q", err, zerops.Status(err), zerops.Code(err), tc.status, tc.code)
			}
			var apiErr *zerops.APIError
			if errors.As(err, &apiErr) && apiErr.Message != tc.message {
				t.Errorf("message = %q, want %q", apiErr.Message, tc.message)
			}
		})
	}
}

func TestUnknownTokenIs401(t *testing.T) {
	f := zeropstest.New(t, org)
	_, err := f.Client("nobody").UserInfo(context.Background())
	if zerops.Status(err) != http.StatusUnauthorized {
		t.Errorf("err = %v, want 401", err)
	}
}

func TestDoSetsTheBearerAndNothingElse(t *testing.T) {
	var seen http.Header
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		path = r.URL.Path
		w.Header().Set("Date", "Wed, 16 Sep 2026 12:00:00 GMT")
		writeOK(w)
	}))
	defer srv.Close()

	c := zerops.New(srv.URL, "the-token", srv.Client())
	got, err := c.UserInfo(context.Background())
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if path != "/api/rest/public/user/info" {
		t.Errorf("path = %q", path)
	}
	if seen.Get("Authorization") != "Bearer the-token" {
		t.Errorf("Authorization = %q", seen.Get("Authorization"))
	}
	want := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if !got.APIDate.Equal(want) {
		t.Errorf("APIDate = %v, want %v", got.APIDate, want)
	}
}

func TestMissingDateHeaderIsTheZeroTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
		writeOK(w)
	}))
	defer srv.Close()

	got, err := zerops.New(srv.URL, "t", srv.Client()).UserInfo(context.Background())
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if !got.APIDate.IsZero() {
		t.Errorf("APIDate = %v, want the zero time", got.APIDate)
	}
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = fmt.Fprint(w, `{"id":"tok-1","clientUserList":[]}`)
}
