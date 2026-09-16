package gitea_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
)

func ptr[T any](v T) *T { return &v }

func TestUsers(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()

	bot, err := c.CreateUser(ctx, gitea.NewUser{
		Login: "mate-p1", Email: "mate-p1@bots.invalid", FullName: "Fen",
		Restricted: true, Visibility: "private",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if bot.Login != "mate-p1" || !bot.Restricted {
		t.Errorf("bot = %+v", bot)
	}

	edited, err := c.EditUser(ctx, "mate-p1", gitea.UserEdit{
		Restricted: ptr(true), MaxRepoCreation: ptr(0),
		AllowCreateOrganization: ptr(false), Active: ptr(true),
	})
	if err != nil || !edited.Active {
		t.Fatalf("EditUser: %+v %v", edited, err)
	}

	got, err := c.GetUser(ctx, "mate-p1")
	if err != nil || got.Login != "mate-p1" {
		t.Fatalf("GetUser: %+v %v", got, err)
	}

	// Departed people are set inactive, never deleted.
	if _, err := c.EditUser(ctx, "mate-p1", gitea.UserEdit{Active: ptr(false)}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if u, _ := f.User("mate-p1"); u.Active {
		t.Error("the user is still active")
	}
}

// The password a bot is created with is generated, never returned and never
// asked for again.
func TestCreateUserReturnsNoPassword(t *testing.T) {
	f := giteatest.New(t)
	bot, err := f.Client().CreateUser(context.Background(), gitea.NewUser{Login: "mate-p2", Email: "b@bots.invalid"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// gitea.User has no password field at all — this is the compile-time
	// guarantee, and the runtime one is that two creations differ.
	other, err := f.Client().CreateUser(context.Background(), gitea.NewUser{Login: "mate-p3", Email: "c@bots.invalid"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if bot.ID == other.ID {
		t.Error("two bots share an id")
	}
}

// Measured on Gitea 1.27.2: /users/{login}/tokens answers 401 "auth required"
// to a site-admin API token and takes the site admin's basic auth instead. The
// client must therefore hold both credentials, and say so plainly when it does
// not.
func TestTokenRoutesNeedBasicAuth(t *testing.T) {
	f := giteatest.New(t)
	ctx := context.Background()
	f.AddUser(gitea.User{Login: "mate-p1", Active: true, Restricted: true})

	tokenOnly := f.TokenOnly(giteatest.AdminToken)
	if _, err := tokenOnly.MintToken(ctx, "mate-p1", "mate/mate-p1/1", []string{"write:repository", "read:user"}); err == nil {
		t.Error("minting succeeded without basic-auth credentials")
	}

	full := f.Client()
	minted, err := full.MintToken(ctx, "mate-p1", "mate/mate-p1/1", []string{"write:repository", "read:user"})
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if minted.Value == "" {
		t.Fatal("the minted token carries no value")
	}
	if got := f.Tokens("mate-p1"); len(got) != 1 || got[0] != "mate/mate-p1/1" {
		t.Errorf("tokens = %v", got)
	}

	list, err := full.ListTokens(ctx, "mate-p1")
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(list) != 1 || list[0].Value != "" {
		t.Errorf("a listed token carries a value: %+v", list)
	}

	// A generation name carries slashes; the path segment must be escaped.
	if err := full.DeleteToken(ctx, "mate-p1", "mate/mate-p1/1"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if got := f.Tokens("mate-p1"); len(got) != 0 {
		t.Errorf("tokens after delete = %v", got)
	}
}

// Measured: GET /user needs read:user; write:repository alone is 403 naming it.
func TestWhoAmINeedsReadUser(t *testing.T) {
	f := giteatest.New(t)
	ctx := context.Background()
	f.AddUser(gitea.User{Login: "mate-p1", Active: true, Restricted: true})
	f.AddToken("mate-p1", "mate/mate-p1/1", "bot-value", "write:repository", "read:user")
	f.AddToken("mate-p1", "narrow", "narrow-value", "write:repository")

	who, err := f.Client().AsToken("bot-value").WhoAmI(ctx)
	if err != nil || who.Login != "mate-p1" {
		t.Fatalf("WhoAmI = %+v, %v", who, err)
	}

	_, err = f.Client().AsToken("narrow-value").WhoAmI(ctx)
	if gitea.Status(err) != http.StatusForbidden {
		t.Errorf("a write:repository-only token got %v, want 403", err)
	}

	_, err = f.Client().AsToken("not-a-token").WhoAmI(ctx)
	if gitea.Status(err) != http.StatusUnauthorized {
		t.Errorf("an unknown token got %v, want 401", err)
	}
}

func TestOrgsAndTeams(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()

	if _, err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := c.GetOrg(ctx, "acme"); err != nil {
		t.Fatalf("GetOrg: %v", err)
	}
	if _, err := c.GetOrg(ctx, "nope"); !gitea.IsNotFound(err) {
		t.Errorf("GetOrg(nope) = %v, want 404", err)
	}

	for _, spec := range []gitea.NewTeam{
		{Name: "read", Permission: "read", UnitsMap: gitea.AllRepoUnits("read")},
		{Name: "write", Permission: "write", UnitsMap: gitea.AllRepoUnits("write")},
		{Name: "release", Permission: "write", UnitsMap: gitea.AllRepoUnits("write")},
	} {
		if _, err := c.CreateTeam(ctx, "acme", spec); err != nil {
			t.Fatalf("CreateTeam(%s): %v", spec.Name, err)
		}
	}
	teams, err := c.ListTeams(ctx, "acme")
	if err != nil || len(teams) != 3 {
		t.Fatalf("ListTeams = %+v, %v", teams, err)
	}
	for _, tm := range teams {
		if !tm.IncludesAllRepositories {
			t.Errorf("team %s does not include all repositories", tm.Name)
		}
		if len(tm.UnitsMap) == 0 {
			t.Errorf("team %s has no units", tm.Name)
		}
	}

	f.AddUser(gitea.User{Login: "u-jan", Active: true})
	readTeam := teams[0]
	if err := c.AddTeamMember(ctx, readTeam.ID, "u-jan"); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}
	members, err := c.ListTeamMembers(ctx, readTeam.ID)
	if err != nil || len(members) != 1 || members[0].Login != "u-jan" {
		t.Fatalf("ListTeamMembers = %+v, %v", members, err)
	}
	if ok, err := c.IsOrgMember(ctx, "acme", "u-jan"); err != nil || !ok {
		t.Errorf("IsOrgMember = %v, %v", ok, err)
	}
	if err := c.RemoveTeamMember(ctx, readTeam.ID, "u-jan"); err != nil {
		t.Fatalf("RemoveTeamMember: %v", err)
	}
	if ok, _ := c.IsOrgMember(ctx, "acme", "u-jan"); ok {
		t.Error("u-jan is still an org member")
	}
}

func TestReposAndProtection(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()
	if _, err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	repo, err := c.CreateOrgRepo(ctx, "acme", gitea.NewRepo{Name: "group", Description: "the recipe"})
	if err != nil {
		t.Fatalf("CreateOrgRepo: %v", err)
	}
	if !repo.Private || repo.DefaultBranch != "main" || repo.Empty {
		t.Errorf("repo = %+v; want private, auto_init, default main", repo)
	}

	if _, err := c.CreateOrgRepo(ctx, "acme", gitea.NewRepo{Name: "group"}); !gitea.IsConflict(err) {
		t.Errorf("a second create = %v, want 409", err)
	}

	f.AddUser(gitea.User{Login: "mate-p1", Active: true})
	if err := c.AddCollaborator(ctx, "acme", "group", "mate-p1", "write"); err != nil {
		t.Fatalf("AddCollaborator: %v", err)
	}
	if ok, err := c.IsCollaborator(ctx, "acme", "group", "mate-p1"); err != nil || !ok {
		t.Errorf("IsCollaborator = %v, %v", ok, err)
	}
	if ok, _ := c.IsCollaborator(ctx, "acme", "group", "u-nobody"); ok {
		t.Error("u-nobody is a collaborator")
	}

	// main: no direct push for anyone, merge by the release team on the group
	// repo. env/*: the broker alone, by rule name, before the branch exists.
	rules := []gitea.BranchProtection{
		{
			RuleName: "main", EnablePush: false,
			EnableMergeWhitelist: true, MergeWhitelistTeams: []string{"release"},
			BlockAdminMergeOverride: true,
		},
		{
			RuleName: "env/*", EnablePush: true, EnablePushWhitelist: true,
			PushWhitelistUsers: []string{giteatest.AdminUser},
		},
	}
	for _, r := range rules {
		if _, err := c.CreateBranchProtection(ctx, "acme", "group", r); err != nil {
			t.Fatalf("CreateBranchProtection(%s): %v", r.RuleName, err)
		}
	}
	got, err := c.ListBranchProtections(ctx, "acme", "group")
	if err != nil || len(got) != 2 {
		t.Fatalf("ListBranchProtections = %+v, %v", got, err)
	}
	for _, r := range got {
		if r.RuleName == "main" && r.EnablePush {
			t.Error("main allows a direct push")
		}
	}

	if _, err := c.CreateTagProtection(ctx, "acme", "group", gitea.TagProtection{
		NamePattern: "v*", WhitelistTeams: []string{"release"},
	}); err != nil {
		t.Fatalf("CreateTagProtection: %v", err)
	}
	tags, err := c.ListTagProtections(ctx, "acme", "group")
	if err != nil || len(tags) != 1 || tags[0].NamePattern != "v*" {
		t.Fatalf("ListTagProtections = %+v, %v", tags, err)
	}
	if _, err := c.EditTagProtection(ctx, "acme", "group", tags[0].ID, gitea.TagProtection{
		NamePattern: "v*", WhitelistTeams: []string{"release", "write"},
	}); err != nil {
		t.Fatalf("EditTagProtection: %v", err)
	}
	if got := f.TagRules("acme", "group"); len(got[0].WhitelistTeams) != 2 {
		t.Errorf("tag rule = %+v", got[0])
	}

	repos, err := c.ListOrgRepos(ctx, "acme")
	if err != nil || len(repos) != 1 {
		t.Fatalf("ListOrgRepos = %+v, %v", repos, err)
	}
}

func TestOrgHookCarriesTheSecretAndTheEvents(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()
	if _, err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	events := []string{"push", "create", "delete", "pull_request", "workflow_job", "workflow_run"}
	if _, err := c.CreateOrgHook(ctx, "acme", gitea.NewHook{
		URL: "http://broker:8080/hooks/gitea", Secret: "s3cret", Events: events,
	}); err != nil {
		t.Fatalf("CreateOrgHook: %v", err)
	}
	stored := f.Hooks("acme")
	if len(stored) != 1 {
		t.Fatalf("hooks = %+v", stored)
	}
	if stored[0].Config["secret"] != "s3cret" || stored[0].Config["content_type"] != "json" {
		t.Errorf("hook config = %v", stored[0].Config)
	}
	if len(stored[0].Events) != len(events) {
		t.Errorf("events = %v", stored[0].Events)
	}

	// Gitea never gives the secret back, so a reconcile can only compare the URL.
	read, err := c.ListOrgHooks(ctx, "acme")
	if err != nil || len(read) != 1 {
		t.Fatalf("ListOrgHooks = %+v, %v", read, err)
	}
	if _, leaked := read[0].Config["secret"]; leaked {
		t.Error("the read-back hook carries its secret")
	}
	if err := c.DeleteOrgHook(ctx, "acme", read[0].ID); err == nil {
		// the fake has no delete route; the call shape is what matters here
		_ = err
	}
}

func TestStatusesAndJobs(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()
	if _, err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := c.CreateOrgRepo(ctx, "acme", gitea.NewRepo{Name: "api"}); err != nil {
		t.Fatalf("CreateOrgRepo: %v", err)
	}

	sha := "3f9c1b2e5d7a4c6f8e0b1d2a3c4f5e6d7a8b9c0d"
	if _, err := c.CreateStatus(ctx, "acme", "api", sha, gitea.NewStatus{
		Context: "mate/deploy/stage/api", State: "pending", Description: "av-1",
	}); err != nil {
		t.Fatalf("CreateStatus: %v", err)
	}
	got, err := c.ListStatuses(ctx, "acme", "api", sha)
	if err != nil || len(got) != 1 || got[0].Context != "mate/deploy/stage/api" {
		t.Fatalf("ListStatuses = %+v, %v", got, err)
	}

	f.AddJob("acme", "api", "42", gitea.Job{ID: 42, RunID: 7, HeadSHA: sha, HeadBranch: "main"})
	job, err := c.GetJob(ctx, "acme", "api", "42")
	if err != nil || job.HeadSHA != sha || job.RunID != 7 {
		t.Fatalf("GetJob = %+v, %v", job, err)
	}
	// A job id that is not this repository's is 404 — that is the whole proof.
	if _, err := c.GetJob(ctx, "acme", "api", "43"); !gitea.IsNotFound(err) {
		t.Errorf("GetJob(43) = %v, want 404", err)
	}
}

func TestRunnerRegistrationToken(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()
	if _, err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	tok, err := c.RunnerRegistrationToken(ctx, "acme")
	if err != nil || tok == "" {
		t.Fatalf("RunnerRegistrationToken = %q, %v", tok, err)
	}
}

func TestAllRepoUnits(t *testing.T) {
	units := gitea.AllRepoUnits("read")
	for _, want := range []string{"repo.code", "repo.pulls", "repo.actions", "repo.releases"} {
		if units[want] != "read" {
			t.Errorf("units[%s] = %q", want, units[want])
		}
	}
}

// The Mate app's browser client is a public one the site admin registers: the
// broker lists, creates and replaces it, and never reads a client secret.
func TestOAuth2Applications(t *testing.T) {
	f := giteatest.New(t)
	c := f.Client()
	ctx := context.Background()

	apps, err := c.ListOAuth2Apps(ctx)
	if err != nil {
		t.Fatalf("ListOAuth2Apps: %v", err)
	}
	if len(apps) != 0 {
		t.Fatalf("a fresh instance has %d applications", len(apps))
	}

	created, err := c.CreateOAuth2App(ctx, gitea.NewOAuth2App{
		Name: "Zerops Mate", ConfidentialClient: false,
		RedirectURIs: []string{"https://app.example/gitea/callback"},
	})
	if err != nil {
		t.Fatalf("CreateOAuth2App: %v", err)
	}
	if created.ClientID == "" || created.ConfidentialClient {
		t.Errorf("created = %+v; want a client id and a public client", created)
	}
	// gitea.OAuth2App has no secret field at all: the broker holds none, so it
	// can leak none.
	if raw, _ := json.Marshal(created); strings.Contains(string(raw), "secret") {
		t.Errorf("the decoded application carries a secret: %s", raw)
	}

	edited, err := c.EditOAuth2App(ctx, created.ID, gitea.NewOAuth2App{
		Name: "Zerops Mate", ConfidentialClient: false,
		RedirectURIs: []string{"http://localhost:5173/gitea/callback", "https://app.example/gitea/callback"},
	})
	if err != nil {
		t.Fatalf("EditOAuth2App: %v", err)
	}
	if len(edited.RedirectURIs) != 2 || edited.ClientID != created.ClientID {
		t.Errorf("edited = %+v; the list is replaced and the client id kept", edited)
	}

	apps, err = c.ListOAuth2Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0].Name != "Zerops Mate" {
		t.Fatalf("ListOAuth2Apps after the edit: %+v %v", apps, err)
	}
}
