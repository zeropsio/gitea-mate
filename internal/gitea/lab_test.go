package gitea_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// 6. D31: a registered Mate's bot writes the group repo as the rights loop
	//    leaves it — in the read team and a collaborator with write, under the
	//    group repo's rules: main takes no direct push and merges from anyone
	//    with write, env/* is the site admin's, v* the release team's. The bot
	//    pushes a branch, opens a pull request and merges it, and is refused
	//    main, an env/* branch and a v* tag.
	t.Run("a bot writing the group repo merges a pull request and pushes nothing protected", func(t *testing.T) {
		if botToken == "" {
			t.Skip("no bot token (the basic-auth variables are unset)")
		}
		const group = "group"
		t.Cleanup(func() { labDelete(t, "/repos/"+org+"/"+group) })
		if _, err := c.CreateOrgRepo(ctx, org, gitea.NewRepo{Name: group, Description: "probe"}); err != nil {
			t.Fatalf("CreateOrgRepo(%s): %v", group, err)
		}
		for _, rule := range []gitea.BranchProtection{
			{RuleName: "main", EnablePush: false},
			{
				RuleName: "env/*", EnablePush: true, EnablePushWhitelist: true,
				PushWhitelistUsers: []string{os.Getenv("GITEA_LAB_ADMIN_USER")},
			},
		} {
			if _, err := c.CreateBranchProtection(ctx, org, group, rule); err != nil {
				t.Fatalf("CreateBranchProtection(%s): %v", rule.RuleName, err)
			}
		}
		if _, err := c.CreateTagProtection(ctx, org, group, gitea.TagProtection{
			NamePattern: "v*", WhitelistTeams: []string{"release"},
		}); err != nil {
			t.Fatalf("CreateTagProtection: %v", err)
		}
		teams, err := c.ListTeams(ctx, org)
		if err != nil {
			t.Fatalf("ListTeams: %v", err)
		}
		for _, team := range teams {
			if team.Name == "read" {
				if err := c.AddTeamMember(ctx, team.ID, bot); err != nil {
					t.Fatalf("AddTeamMember(read): %v", err)
				}
			}
		}
		if err := c.AddCollaborator(ctx, org, group, bot, "write"); err != nil {
			t.Fatalf("AddCollaborator(%s): %v", group, err)
		}

		root := "/repos/" + org + "/" + group
		content := func(line string) string { return base64.StdEncoding.EncodeToString([]byte(line + "\n")) }
		branch := "mate/" + bot
		if status, body := labAs(t, botToken, http.MethodPost, root+"/contents/probe.txt", map[string]any{
			"content": content("probe"), "message": "probe", "branch": "main", "new_branch": branch,
		}); status != http.StatusCreated {
			t.Fatalf("the bot could not push a branch to the group repo: %d %s", status, body)
		}
		status, body := labAs(t, botToken, http.MethodPost, root+"/pulls", map[string]any{
			"head": branch, "base": "main", "title": "probe",
		})
		if status != http.StatusCreated {
			t.Fatalf("the bot could not open a pull request on the group repo: %d %s", status, body)
		}
		var pull struct {
			Number int64 `json:"number"`
		}
		if err := json.Unmarshal([]byte(body), &pull); err != nil || pull.Number == 0 {
			t.Fatalf("the pull request answer %q: %v", body, err)
		}
		// Gitea checks a new request's mergeability in the background and
		// answers 405 "Please try again later" until it has; a refused merge
		// is a 405 in other words, and ends the test.
		for attempt := 1; ; attempt++ {
			err := c.AsToken(botToken).MergePullRequest(ctx, org, group, pull.Number, "")
			if err == nil {
				break
			}
			var apiErr *gitea.APIError
			if attempt < 20 && errors.As(err, &apiErr) && apiErr.Status == http.StatusMethodNotAllowed &&
				strings.Contains(apiErr.Message, "try again later") {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			t.Fatalf("the bot could not merge its pull request into the group repo's main: %v", err)
		}

		for _, refused := range []struct {
			what, path string
			in         map[string]any
		}{
			{"a commit straight onto main", root + "/contents/main.txt",
				map[string]any{"content": content("main"), "message": "probe", "branch": "main"}},
			{"an env/* branch", root + "/branches",
				map[string]any{"new_branch_name": "env/probe", "old_ref_name": "main"}},
			{"a v* tag", root + "/tags",
				map[string]any{"tag_name": "v0.0.1-probe", "target": "main", "message": "probe"}},
		} {
			if status, body := labAs(t, botToken, http.MethodPost, refused.path, refused.in); status < 400 {
				t.Errorf("the bot was allowed %s on the group repo: %d %s", refused.what, status, body)
			} else {
				t.Logf("%s refused, as it must be: %d", refused.what, status)
			}
		}
	})
}

// labAs issues one call to the lab Gitea's API with a token of the test's own
// — a bot's — and returns the status and the body.
func labAs(t *testing.T, token, method, path string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(os.Getenv("GITEA_LAB_URL"), "/")+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out)
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

// labPost issues one POST as the lab admin token and returns the status and
// the body.
func labPost(t *testing.T, path string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(os.Getenv("GITEA_LAB_URL"), "/")+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", "token "+os.Getenv("GITEA_LAB_ADMIN_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out)
}

// labCSRF finds the CSRF token a Gitea page expects: the form's hidden field,
// or the _csrf cookie the page set.
func labCSRF(html string, jar *cookiejar.Jar, base string) string {
	if m := regexp.MustCompile(`name="_csrf"\s+value="([^"]+)"`).FindStringSubmatch(html); len(m) == 2 {
		return m[1]
	}
	if m := regexp.MustCompile(`content="([^"]+)"\s*/?>\s*<!--\s*csrf`).FindStringSubmatch(html); len(m) == 2 {
		return m[1]
	}
	u, err := neturl.Parse(base)
	if err != nil {
		return ""
	}
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "_csrf" {
			return ck.Value
		}
	}
	return ""
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return b
}
