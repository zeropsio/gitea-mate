package gitea_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// The lab test runs against a real Gitea. It is skipped unless GITEA_LAB_URL
// and GITEA_LAB_ADMIN_TOKEN are set; the credentials reach it only through the
// environment, never a file or a fixture.
//
//	GITEA_LAB_URL=http://127.0.0.1:3301 \
//	GITEA_LAB_ADMIN_TOKEN=… \
//	GITEA_LAB_ADMIN_USER=… GITEA_LAB_ADMIN_PASSWORD=… \
//	go test ./internal/gitea/ -run TestLab -v
//
// The token routes need the two basic-auth variables as well; without them
// those sub-tests skip and say so.
func labClient(t *testing.T) (*gitea.Client, bool) {
	t.Helper()
	base := os.Getenv("GITEA_LAB_URL")
	token := os.Getenv("GITEA_LAB_ADMIN_TOKEN")
	if base == "" || token == "" {
		t.Skip("set GITEA_LAB_URL and GITEA_LAB_ADMIN_TOKEN to run the lab test")
	}
	user, password := os.Getenv("GITEA_LAB_ADMIN_USER"), os.Getenv("GITEA_LAB_ADMIN_PASSWORD")
	return gitea.New(gitea.Config{
		BaseURL: base, AdminToken: token,
		AdminUser: user, AdminPassword: password,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}), user != "" && password != ""
}

func TestLab(t *testing.T) {
	c, haveBasic := labClient(t)
	ctx := context.Background()

	stamp := fmt.Sprint(time.Now().UnixNano() % 1e9)
	org := "probe-mate" + stamp
	bot := "mate-p" + stamp
	repo := "svc"

	// The broker never deletes an org, a repository or a person — it disables
	// people instead — so the tidy-up is the test's own plain DELETE.
	t.Cleanup(func() {
		labDelete(t, "/repos/"+org+"/"+repo)
		labDelete(t, "/orgs/"+org)
		labDelete(t, "/admin/users/"+bot+"?purge=true")
	})

	// 1. A restricted bot, made through the API and shaped by the flags
	//    docs/vocabulary.md asks for.
	created, err := c.CreateUser(ctx, gitea.NewUser{
		Login: bot, Email: bot + "@bots.invalid", FullName: "Probe Mate",
		Restricted: true, Visibility: "private",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if !created.Restricted {
		t.Errorf("the bot is not restricted: %+v", created)
	}
	if _, err := c.EditUser(ctx, bot, gitea.UserEdit{
		Restricted: ptr(true), MaxRepoCreation: ptr(0), AllowCreateOrganization: ptr(false), Active: ptr(true),
	}); err != nil {
		t.Fatalf("EditUser: %v", err)
	}

	// 2/3. A token minted for the bot BY THE ADMIN, and what its scopes reach.
	var botToken string
	t.Run("the admin mints a token for the bot", func(t *testing.T) {
		if !haveBasic {
			t.Skip("set GITEA_LAB_ADMIN_USER and GITEA_LAB_ADMIN_PASSWORD: the token routes refuse an API token")
		}
		// Pin the measured behaviour: an API token, however privileged, is 401
		// on this route.
		tokenOnly := gitea.New(gitea.Config{
			BaseURL: os.Getenv("GITEA_LAB_URL"), AdminToken: os.Getenv("GITEA_LAB_ADMIN_TOKEN"),
			HTTP: &http.Client{Timeout: 30 * time.Second},
		})
		if _, err := tokenOnly.MintToken(ctx, bot, "probe/api-token", []string{"write:repository"}); err == nil {
			t.Error("the API token minted a token; Gitea 1.27.2 refuses that route with 401")
		}

		minted, err := c.MintToken(ctx, bot, "mate/"+bot+"/1", []string{"write:repository", "read:user"})
		if err != nil {
			t.Fatalf("MintToken: %v", err)
		}
		if minted.Value == "" {
			t.Fatal("the minted token carries no value")
		}
		botToken = minted.Value

		who, err := c.AsToken(botToken).WhoAmI(ctx)
		if err != nil {
			t.Fatalf("GET /user as write:repository,read:user: %v", err)
		}
		if who.Login != bot {
			t.Errorf("GET /user answered %q, want %q", who.Login, bot)
		}

		narrow, err := c.MintToken(ctx, bot, "probe/narrow", []string{"write:repository"})
		if err != nil {
			t.Fatalf("MintToken(narrow): %v", err)
		}
		_, err = c.AsToken(narrow.Value).WhoAmI(ctx)
		if gitea.Status(err) != http.StatusForbidden {
			t.Errorf("GET /user as write:repository alone = %v, want 403 — read:user is load-bearing", err)
		} else {
			t.Logf("write:repository alone on GET /user: %v", err)
		}
		if err := c.DeleteToken(ctx, bot, "probe/narrow"); err != nil {
			t.Errorf("DeleteToken by name: %v", err)
		}
		names, err := c.ListTokens(ctx, bot)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		for _, n := range names {
			if n.Name == "probe/narrow" {
				t.Errorf("probe/narrow survived the delete")
			}
			if n.Value != "" {
				t.Errorf("a listed token carries a value")
			}
		}
	})

	// 4. main protected before any push: a direct push is refused, a branch
	//    push is taken.
	if _, err := c.CreateOrg(ctx, org, "Probe "+stamp); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// The teams come first: a branch protection naming a team that does not
	// exist is 422 "team does not exist" (measured on 1.27.2), so the mirror's
	// plan must order teams before the group repo's rules.
	for _, spec := range []gitea.NewTeam{
		{Name: "read", Permission: "read", UnitsMap: gitea.AllRepoUnits("read")},
		{Name: "write", Permission: "write", UnitsMap: gitea.AllRepoUnits("write")},
		{Name: "release", Permission: "write", UnitsMap: gitea.AllRepoUnits("write")},
	} {
		if _, err := c.CreateTeam(ctx, org, spec); err != nil {
			t.Fatalf("CreateTeam(%s): %v", spec.Name, err)
		}
	}

	created2, err := c.CreateOrgRepo(ctx, org, gitea.NewRepo{Name: repo, Description: "probe"})
	if err != nil {
		t.Fatalf("CreateOrgRepo: %v", err)
	}
	if created2.DefaultBranch != "main" || created2.Empty {
		t.Fatalf("repo = %+v; want auto_init on main", created2)
	}
	if _, err := c.CreateBranchProtection(ctx, org, repo, gitea.BranchProtection{
		RuleName: "main", EnablePush: false,
		EnableMergeWhitelist: true, MergeWhitelistTeams: []string{"write"},
		BlockAdminMergeOverride: true,
	}); err != nil {
		t.Fatalf("CreateBranchProtection(main): %v", err)
	}
	// env/* names a branch that does not exist yet — the rule still takes.
	if _, err := c.CreateBranchProtection(ctx, org, repo, gitea.BranchProtection{
		RuleName: "env/*", EnablePush: true, EnablePushWhitelist: true,
		PushWhitelistUsers: []string{os.Getenv("GITEA_LAB_ADMIN_USER")},
	}); err != nil {
		t.Fatalf("CreateBranchProtection(env/*): %v", err)
	}
	if err := c.AddCollaborator(ctx, org, repo, bot, "write"); err != nil {
		t.Fatalf("AddCollaborator: %v", err)
	}

	t.Run("main refuses a direct push and takes a branch", func(t *testing.T) {
		if botToken == "" {
			t.Skip("no bot token (the basic-auth variables are unset)")
		}
		git, err := exec.LookPath("git")
		if err != nil {
			t.Skip("no git on PATH")
		}
		dir := t.TempDir()
		cloneURL := strings.TrimSuffix(os.Getenv("GITEA_LAB_URL"), "/") + "/" + org + "/" + repo + ".git"

		// The credential never touches argv or a file: git reads it from the
		// environment as an http.extraHeader.
		basic := base64.StdEncoding.EncodeToString([]byte(bot + ":" + botToken))
		env := append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=3",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
			"GIT_CONFIG_KEY_1=user.name", "GIT_CONFIG_VALUE_1=Probe Mate",
			"GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2="+bot+"@bots.invalid",
		)
		run := func(wd string, args ...string) (string, error) {
			cmd := exec.Command(git, args...)
			cmd.Dir = wd
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			return string(out), err
		}

		if out, err := run(dir, "clone", cloneURL, "wc"); err != nil {
			t.Fatalf("clone: %v\n%s", err, out)
		}
		wc := filepath.Join(dir, "wc")
		if err := os.WriteFile(filepath.Join(wc, "probe.txt"), []byte("probe\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if out, err := run(wc, "add", "probe.txt"); err != nil {
			t.Fatalf("add: %v\n%s", err, out)
		}
		if out, err := run(wc, "commit", "-m", "probe"); err != nil {
			t.Fatalf("commit: %v\n%s", err, out)
		}

		out, err := run(wc, "push", "origin", "HEAD:main")
		if err == nil {
			t.Errorf("a direct push to protected main succeeded:\n%s", out)
		} else {
			t.Logf("direct push to main refused, as it must be:\n%s", strings.TrimSpace(out))
		}

		if out, err := run(wc, "push", "origin", "HEAD:refs/heads/mate/"+bot); err != nil {
			t.Errorf("a branch push was refused:\n%s", out)
		}
	})

	// 5. An org-scope runner registration token.
	t.Run("an org-scope runner registration token", func(t *testing.T) {
		tok, err := c.RunnerRegistrationToken(ctx, org)
		if err != nil {
			t.Fatalf("RunnerRegistrationToken: %v", err)
		}
		if tok == "" {
			t.Fatal("the registration token is empty")
		}
		t.Logf("registration token: %d characters, never logged in full", len(tok))
	})
}

// labDelete issues one DELETE as the lab admin token. Failures are logged, not
// fatal: a tidy-up must never mask the result of the test itself.
func labDelete(t *testing.T, path string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, strings.TrimSuffix(os.Getenv("GITEA_LAB_URL"), "/")+"/api/v1"+path, nil)
	if err != nil {
		t.Logf("tidy-up %s: %v", path, err)
		return
	}
	req.Header.Set("Authorization", "token "+os.Getenv("GITEA_LAB_ADMIN_TOKEN"))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Logf("tidy-up %s: %v", path, err)
		return
	}
	_ = resp.Body.Close()
	t.Logf("tidy-up DELETE %s -> %d", path, resp.StatusCode)
}
