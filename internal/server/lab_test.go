package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
)

// TestLabPersonAttachment runs GET /person/attachments/{uuid} in front of a
// real Gitea. Like internal/gitea's TestLab it is skipped unless GITEA_LAB_URL,
// GITEA_LAB_ADMIN_TOKEN, GITEA_LAB_ADMIN_USER and GITEA_LAB_ADMIN_PASSWORD are
// set, and the credentials reach it only through the environment.
//
// It makes its own fixtures — a private repository with a pull request, a bot
// with a Mate's scopes that attaches a picture, a text file and an SVG to it, a
// person who may read the repository and one who may not — and deletes them.
func TestLabPersonAttachment(t *testing.T) {
	base, adminToken := os.Getenv("GITEA_LAB_URL"), os.Getenv("GITEA_LAB_ADMIN_TOKEN")
	adminUser, adminPassword := os.Getenv("GITEA_LAB_ADMIN_USER"), os.Getenv("GITEA_LAB_ADMIN_PASSWORD")
	if base == "" || adminToken == "" || adminUser == "" || adminPassword == "" {
		t.Skip("set GITEA_LAB_URL, GITEA_LAB_ADMIN_TOKEN, GITEA_LAB_ADMIN_USER and GITEA_LAB_ADMIN_PASSWORD to run the lab test")
	}
	ctx := context.Background()
	admin := gitea.New(gitea.Config{
		BaseURL: base, AdminToken: adminToken, AdminUser: adminUser, AdminPassword: adminPassword,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	})
	lab := labCaller{t: t, base: strings.TrimSuffix(base, "/")}

	stamp := fmt.Sprint(time.Now().UnixNano() % 1e9)
	org, repo := "probe-attach"+stamp, "svc"
	bot, reader, stranger := "mate-p"+stamp, "u-reader"+stamp, "u-stranger"+stamp
	t.Cleanup(func() {
		lab.call(adminToken, http.MethodDelete, "/api/v1/repos/"+org+"/"+repo, nil, "")
		lab.call(adminToken, http.MethodDelete, "/api/v1/orgs/"+org, nil, "")
		for _, login := range []string{bot, reader, stranger} {
			lab.call(adminToken, http.MethodDelete, "/api/v1/admin/users/"+login+"?purge=true", nil, "")
		}
	})

	for _, login := range []string{bot, reader, stranger} {
		if _, err := admin.CreateUser(ctx, gitea.NewUser{
			Login: login, Email: login + "@lab.invalid", FullName: login,
		}); err != nil {
			t.Fatalf("CreateUser(%s): %v", login, err)
		}
	}
	lab.must(adminToken, http.MethodPost, "/api/v1/orgs", map[string]any{"username": org, "visibility": "private"})
	lab.must(adminToken, http.MethodPost, "/api/v1/orgs/"+org+"/repos",
		map[string]any{"name": repo, "private": true, "auto_init": true, "default_branch": "main"})
	lab.must(adminToken, http.MethodPut, "/api/v1/repos/"+org+"/"+repo+"/collaborators/"+bot, map[string]any{"permission": "write"})
	lab.must(adminToken, http.MethodPut, "/api/v1/repos/"+org+"/"+repo+"/collaborators/"+reader, map[string]any{"permission": "read"})

	mint := func(login, name string, scopes ...string) string {
		minted, err := admin.MintToken(ctx, login, name, scopes)
		if err != nil {
			t.Fatalf("MintToken(%s): %v", login, err)
		}
		return minted.Value
	}
	botToken := mint(bot, mirror.TokenName(bot, 2), mirror.BotScopes...)
	earlierBotToken := mint(bot, mirror.TokenName(bot, 1), "write:repository", "read:user")
	readerToken := mint(reader, mirror.AppTokenPrefix+stamp, appTokenScopes...)
	strangerToken := mint(stranger, mirror.AppTokenPrefix+stamp, appTokenScopes...)

	// The bot's change: a branch with a commit, and its pull request.
	lab.must(botToken, http.MethodPost, "/api/v1/repos/"+org+"/"+repo+"/contents/app.txt", map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte("hello\n")), "message": "add app",
		"branch": "main", "new_branch": "mate/" + bot,
	})
	var pull struct {
		Number int `json:"number"`
	}
	lab.decode(lab.must(botToken, http.MethodPost, "/api/v1/repos/"+org+"/"+repo+"/pulls",
		map[string]any{"head": "mate/" + bot, "base": "main", "title": "Build a todo app"}), &pull)

	picture := labPNG()
	assets := "/api/v1/repos/" + org + "/" + repo + "/issues/" + fmt.Sprint(pull.Number) + "/assets"
	if status, body := lab.upload(earlierBotToken, assets, "shot-1.png", "image/png", picture); status != http.StatusForbidden {
		t.Errorf("an earlier generation (write:repository,read:user) attached a picture: %d %s", status, body)
	}
	uuidOf := func(name, contentType string, content []byte) string {
		status, body := lab.upload(botToken, assets, name, contentType, content)
		if status != http.StatusCreated {
			t.Fatalf("the bot could not attach %s: %d %s", name, status, body)
		}
		var att struct {
			UUID string `json:"uuid"`
		}
		lab.decode(body, &att)
		return att.UUID
	}
	pictureUUID := uuidOf("shot-1.png", "image/png", picture)
	textUUID := uuidOf("note.txt", "text/plain", []byte("plain words\n"))
	svgUUID := uuidOf("pic.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))

	// The broker in front of that Gitea, as it runs: its router, its logger.
	var logged bytes.Buffer
	s := New(&config.Config{GiteaURL: base}, slog.New(slog.NewTextHandler(&logged, nil)), Deps{Gitea: admin})
	t.Cleanup(s.Close)
	broker := httptest.NewServer(s.Handler())
	t.Cleanup(broker.Close)

	preflight, err := http.NewRequestWithContext(ctx, http.MethodOptions, broker.URL+"/person/attachments/"+pictureUUID, nil)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Header.Set("Origin", "http://localhost:5173")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization")
	resp, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	t.Logf("preflight: %d allow-origin=%q allow-headers=%q", resp.StatusCode,
		resp.Header.Get("Access-Control-Allow-Origin"), resp.Header.Get("Access-Control-Allow-Headers"))
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("the preflight is not answered for every origin")
	}

	get := func(uuid, token string) (int, http.Header, []byte) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, broker.URL+"/person/attachments/"+uuid, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://localhost:5173")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, body
	}

	status, header, body := get(pictureUUID, readerToken)
	t.Logf("the reader's picture: %d %s %s bytes, identical=%v", status, header.Get("Content-Type"), header.Get("Content-Length"), bytes.Equal(body, picture))
	if status != http.StatusOK || !bytes.Equal(body, picture) || header.Get("Content-Type") != "image/png" {
		t.Errorf("the reader's picture = %d %q, want 200, image/png and the bytes attached", status, header.Get("Content-Type"))
	}
	for _, tc := range []struct {
		name, uuid, token string
		want              int
	}{
		{"a token Gitea does not know", pictureUUID, "not-a-token-" + stamp, http.StatusUnauthorized},
		{"a person who may not read the repository", pictureUUID, strangerToken, http.StatusNotFound},
		{"an earlier generation of the bot's token", pictureUUID, earlierBotToken, http.StatusForbidden},
		{"a text file", textUUID, readerToken, http.StatusUnsupportedMediaType},
		{"an SVG", svgUUID, readerToken, http.StatusUnsupportedMediaType},
	} {
		status, header, body := get(tc.uuid, tc.token)
		t.Logf("%s: %d %s", tc.name, status, strings.TrimSpace(string(body)))
		if status != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, status, tc.want)
		}
		if header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: the refusal cannot be read by the app", tc.name)
		}
	}
	for _, token := range []string{readerToken, strangerToken, earlierBotToken, botToken} {
		if strings.Contains(logged.String(), token) {
			t.Fatalf("a token reached the broker's log")
		}
	}
}

// labCaller makes the lab's raw calls: the fixtures need routes the broker's
// client has no reason to know.
type labCaller struct {
	t    *testing.T
	base string
}

func (l labCaller) call(token, method, path string, in any, contentType string) (int, []byte) {
	l.t.Helper()
	var reader io.Reader
	if in != nil {
		if raw, ok := in.([]byte); ok {
			reader = bytes.NewReader(raw)
		} else {
			encoded, err := json.Marshal(in)
			if err != nil {
				l.t.Fatal(err)
			}
			reader, contentType = bytes.NewReader(encoded), "application/json"
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, l.base+path, reader)
	if err != nil {
		l.t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (l labCaller) must(token, method, path string, in any) []byte {
	l.t.Helper()
	status, body := l.call(token, method, path, in, "")
	if status >= 300 {
		l.t.Fatalf("%s %s = %d %s", method, path, status, body)
	}
	return body
}

func (l labCaller) upload(token, path, name, contentType string, content []byte) (int, []byte) {
	l.t.Helper()
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreatePart(map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="attachment"; filename=%q`, name)},
		"Content-Type":        {contentType},
	})
	if err != nil {
		l.t.Fatal(err)
	}
	_, _ = part.Write(content)
	_ = writer.Close()
	return l.call(token, http.MethodPost, path+"?name="+name, form.Bytes(), writer.FormDataContentType())
}

func (l labCaller) decode(body []byte, out any) {
	l.t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		l.t.Fatalf("decode %s: %v", body, err)
	}
}

// labPNG is a real, decodable 8×6 PNG: Gitea detects an attachment's type from
// its bytes.
func labPNG() []byte {
	raw, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAgAAAAGCAIAAABxZ0isAAAAEUlEQVR4nGM4YWODFTEMpAQAr9U8AVonTcEAAAAASUVORK5CYII=")
	return raw
}
