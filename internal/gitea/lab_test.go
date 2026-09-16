package gitea_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

// A public OAuth2 client the SITE ADMIN registers lets ANOTHER person complete
// the PKCE code flow. That is the whole reason the broker registers the Mate
// app's client: the app is a browser page with no secret, and it must not need
// the person's Gitea session to get itself a client id.
//
// The flow is Gitea's web one — sign in, grant, exchange — so this test drives
// the pages, not the API. It needs a password sign-in form; the lab has one,
// the deployed Gitea deliberately does not (ENABLE_PASSWORD_SIGNIN_FORM is
// false there, and the person arrives through the broker's OIDC source).
//
// No token value is ever logged: statuses and lengths only.
func TestLabPublicOAuth2ClientServesAnotherPerson(t *testing.T) {
	c, _ := labClient(t)
	ctx := context.Background()
	base := strings.TrimSuffix(os.Getenv("GITEA_LAB_URL"), "/")

	stamp := fmt.Sprint(time.Now().UnixNano() % 1e9)
	person := "probe-person" + stamp
	password := "Probe-" + stamp + "-" + fmt.Sprint(time.Now().UnixNano())
	redirect := "http://127.0.0.1:5173/gitea/callback"

	app, err := c.CreateOAuth2App(ctx, gitea.NewOAuth2App{
		Name:               "Zerops Mate probe " + stamp,
		ConfidentialClient: false,
		RedirectURIs:       []string{redirect},
	})
	if err != nil {
		t.Fatalf("CreateOAuth2App: %v", err)
	}
	t.Cleanup(func() {
		labDelete(t, "/user/applications/oauth2/"+strconv.FormatInt(app.ID, 10))
		labDelete(t, "/admin/users/"+person+"?purge=true")
	})
	if app.ConfidentialClient {
		t.Fatalf("the application is confidential: %+v", app)
	}

	// The person is made with a password the test knows; the broker's own
	// CreateUser generates one it never returns, which is right for a bot and
	// useless for a sign-in.
	if status, body := labPost(t, "/admin/users", map[string]any{
		"username": person, "email": person + "@lab.invalid",
		"password": password, "must_change_password": false,
	}); status != http.StatusCreated {
		t.Fatalf("creating the person: %d %s", status, body)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	browser := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		// Every redirect is read, never followed: the authorization code
		// arrives in a Location the test must see.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	page := func(method, url string, form neturl.Values) (int, string, string) {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, string(raw), resp.Header.Get("Location")
	}

	status, html, _ := page(http.MethodGet, base+"/user/login", nil)
	t.Logf("GET /user/login -> %d", status)
	status, _, loc := page(http.MethodPost, base+"/user/login", neturl.Values{
		"_csrf": {labCSRF(html, jar, base)}, "user_name": {person}, "password": {password},
	})
	t.Logf("POST /user/login -> %d %s", status, loc)
	if status != http.StatusFound && status != http.StatusSeeOther {
		t.Fatalf("the person could not sign in: %d", status)
	}

	// PKCE: the verifier never leaves the test, the challenge is its SHA-256.
	verifier := strings.TrimRight(base64.RawURLEncoding.EncodeToString(randomBytes(t, 32)), "=")
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authorize := base + "/login/oauth/authorize?" + neturl.Values{
		"client_id": {app.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"state": {"probe-state"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}.Encode()
	status, html, loc = page(http.MethodGet, authorize, nil)
	t.Logf("GET /login/oauth/authorize -> %d %s", status, loc)
	if status == http.StatusOK {
		status, _, loc = page(http.MethodPost, base+"/login/oauth/grant", neturl.Values{
			"_csrf": {labCSRF(html, jar, base)}, "client_id": {app.ClientID},
			"redirect_uri": {redirect}, "state": {"probe-state"}, "granted": {"true"},
		})
		t.Logf("POST /login/oauth/grant -> %d", status)
	}
	if loc == "" {
		t.Fatalf("no redirect carried a code (status %d)", status)
	}
	parsed, err := neturl.Parse(loc)
	if err != nil {
		t.Fatalf("the callback URL: %v", err)
	}
	code := parsed.Query().Get("code")
	t.Logf("callback %s://%s%s carries a code of %d characters, state %q",
		parsed.Scheme, parsed.Host, parsed.Path, len(code), parsed.Query().Get("state"))
	if code == "" {
		t.Fatal("the callback carries no code")
	}

	// The exchange sends no client secret at all — that is the claim.
	status, body, _ := page(http.MethodPost, base+"/login/oauth/access_token", neturl.Values{
		"grant_type": {"authorization_code"}, "client_id": {app.ClientID},
		"code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier},
	})
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal([]byte(body), &token)
	t.Logf("POST /login/oauth/access_token -> %d, token_type %q, expires_in %d, access token of %d characters, error %q %q",
		status, token.TokenType, token.ExpiresIn, len(token.AccessToken), token.Error, token.Description)
	if status != http.StatusOK || token.AccessToken == "" {
		t.Fatalf("the public client could not exchange the code: %d %s", status, token.Error)
	}

	// The token is the PERSON's, not the admin's.
	who, err := c.AsToken(token.AccessToken).WhoAmI(ctx)
	if err != nil {
		t.Fatalf("GET /user with the issued token: %v", err)
	}
	t.Logf("GET /api/v1/user with that token -> %s", who.Login)
	if who.Login != person {
		t.Errorf("the token belongs to %q, want %q", who.Login, person)
	}
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
