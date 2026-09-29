package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// The fixtures are invented: a picture's bytes, three attachments by id, and
// the app tokens of two people the fake Gitea knows.
const (
	pictureUUID = "3f2a9c1e-5b7d-4e8f-9a0b-1c2d3e4f5a6b"
	textUUID    = "4a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	svgUUID     = "5b2c3d4e-5f6a-4b7c-9d8e-0f1a2b3c4d5e"
	absentUUID  = "6c3d4e5f-6a7b-4c8d-8e9f-1a2b3c4d5e6f"
	janApp      = "jan-app-token-fixture"
	janNarrow   = "jan-narrow-token-fixture"
	veraApp     = "vera-app-token-fixture"
)

// pictureBytes stands in for a PNG: its signature and a few bytes more.
var pictureBytes = []byte("\x89PNG\r\n\x1a\n-not-really-a-picture-")

// attachmentRig is the base rig with the attachments of one pull request on
// its Gitea: a picture and a text file and an SVG that only Jan may read, and
// the tokens Jan and Vera hold.
func attachmentRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	for _, login := range []string{"u-jan", "u-vera"} {
		r.gitea.AddUser(gitea.User{Login: login, Active: true})
	}
	appScopes := []string{"read:user", "read:organization", "write:repository", "write:issue"}
	r.gitea.AddToken("u-jan", "mate-app/1", janApp, appScopes...)
	r.gitea.AddToken("u-jan", "narrow", janNarrow, "write:repository", "read:user")
	r.gitea.AddToken("u-vera", "mate-app/2", veraApp, appScopes...)
	r.gitea.AddAttachment(pictureUUID, "image/png", pictureBytes, "u-jan")
	r.gitea.AddAttachment(textUUID, "text/plain; charset=utf-8", []byte("plain words\n"), "u-jan")
	r.gitea.AddAttachment(svgUUID, "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "u-jan")
	return r
}

func (r *rig) attachment(uuid, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/person/attachments/"+uuid, nil)
	req.Header.Set("Origin", "https://app.example")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r.do(req)
}

// GET /person/attachments/{uuid} is how the app reads a picture on a pull
// request. Gitea serves an attachment's bytes on one web route, whose CORS
// preflight it answers with a redirect to sign in (measured on 1.27.2), so a
// browser on another origin cannot read a private picture there with a token.
// The broker asks Gitea as the person — their own token, forwarded — and
// answers every origin; it serves raster pictures and nothing else.
func TestPersonAttachment(t *testing.T) {
	cases := []struct {
		name       string
		uuid       string
		bearer     string
		failGitea  int
		wantStatus int
		wantCode   string
	}{
		{name: "a picture the person may read", uuid: pictureUUID, bearer: janApp, wantStatus: http.StatusOK},
		{name: "not an attachment's id", uuid: "not-a-uuid", bearer: janApp, wantStatus: http.StatusBadRequest, wantCode: "not_an_attachment"},
		{name: "an id Gitea would never write", uuid: strings.ToUpper(pictureUUID), bearer: janApp, wantStatus: http.StatusBadRequest, wantCode: "not_an_attachment"},
		{name: "no token", uuid: pictureUUID, wantStatus: http.StatusUnauthorized, wantCode: "gitea_token_required"},
		{
			// Followed, Gitea's redirect to its sign-in page is a 200 of HTML.
			name: "a token Gitea does not know", uuid: pictureUUID, bearer: "no-such-token",
			wantStatus: http.StatusUnauthorized, wantCode: "gitea_token_refused",
		},
		{name: "a token that may not read issues", uuid: pictureUUID, bearer: janNarrow, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "somebody who may not read it", uuid: pictureUUID, bearer: veraApp, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "no such attachment", uuid: absentUUID, bearer: janApp, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "a text file is not a picture", uuid: textUUID, bearer: janApp, wantStatus: http.StatusUnsupportedMediaType, wantCode: "not_a_picture"},
		{name: "an SVG can carry script, so it is not served", uuid: svgUUID, bearer: janApp, wantStatus: http.StatusUnsupportedMediaType, wantCode: "not_a_picture"},
		{name: "Gitea is away", uuid: pictureUUID, bearer: janApp, failGitea: http.StatusInternalServerError, wantStatus: http.StatusServiceUnavailable, wantCode: "gitea"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := attachmentRig(t)
			if tc.failGitea != 0 {
				r.gitea.Fail["GET /attachments/"+tc.uuid] = tc.failGitea
			}
			rr := r.attachment(tc.uuid, tc.bearer)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body)
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %q: the app must be able to read every answer, a refusal included", got)
			}
			if tc.wantStatus != http.StatusOK {
				var body ErrorBody
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error != tc.wantCode {
					t.Errorf("refusal = %s, want code %q", rr.Body, tc.wantCode)
				}
				if tc.wantStatus == http.StatusServiceUnavailable && rr.Header().Get("Retry-After") == "" {
					t.Error("a Gitea that is away is answered with Retry-After")
				}
				return
			}
			if !bytes.Equal(rr.Body.Bytes(), pictureBytes) {
				t.Errorf("body = %q, want the attachment's bytes unchanged", rr.Body.Bytes())
			}
			for header, want := range map[string]string{
				"Content-Type":           "image/png",
				"Content-Length":         strconv.Itoa(len(pictureBytes)),
				"Cache-Control":          "private, max-age=3600",
				"X-Content-Type-Options": "nosniff",
			} {
				if got := rr.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			if got := rr.Header().Get("Vary"); !strings.Contains(got, "Authorization") {
				t.Errorf("Vary = %q: one browser's cache must never answer one person with another's picture", got)
			}
			if got := rr.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
				t.Errorf("Content-Security-Policy = %q, want a sandbox", got)
			}
		})
	}
}

// A picture larger than a picture has any reason to be is refused before a
// byte of it is sent.
func TestPersonAttachment_TooLarge(t *testing.T) {
	prev := maxPictureBytes
	maxPictureBytes = int64(len(pictureBytes) - 1)
	t.Cleanup(func() { maxPictureBytes = prev })

	rr := attachmentRig(t).attachment(pictureUUID, janApp)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "too_large") {
		t.Fatalf("status = %d, body %s; want 413 too_large", rr.Code, rr.Body)
	}
}

// The browser asks before it sends a token across origins; every origin gets
// its yes, exactly as POST /person/token's preflight does.
func TestPersonAttachmentPreflight(t *testing.T) {
	r := attachmentRig(t)
	req := httptest.NewRequest(http.MethodOptions, "/person/attachments/"+pictureUUID, nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rr := r.do(req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	for header, want := range map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Headers": "Authorization",
		"Access-Control-Allow-Methods": "GET",
	} {
		if got := rr.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to hold %q", header, got, want)
		}
	}
	if rr.Header().Get("Access-Control-Max-Age") == "" {
		t.Error("the preflight's answer is not kept: every picture would ask again")
	}
}

// The person's token reaches Gitea and nothing else — never a log line.
func TestPersonAttachmentNeverLogsTheToken(t *testing.T) {
	var logged bytes.Buffer
	r := attachmentRig(t)
	s := New(r.server.cfg, slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), r.server.deps)
	t.Cleanup(s.Close)
	r.handler = s.Handler()

	for _, bearer := range []string{janApp, veraApp, "no-such-token"} {
		r.attachment(pictureUUID, bearer)
	}
	r.gitea.Fail["GET /attachments/"+pictureUUID] = http.StatusInternalServerError
	r.attachment(pictureUUID, janApp)

	for _, token := range []string{janApp, veraApp, "no-such-token"} {
		if strings.Contains(logged.String(), token) {
			t.Fatalf("a token reached the log:\n%s", logged.String())
		}
	}
	if !strings.Contains(logged.String(), "/person/attachments/") {
		t.Errorf("the requests were not logged at all:\n%s", logged.String())
	}
}
