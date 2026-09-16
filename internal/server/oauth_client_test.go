package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/config"
)

func appConfig() *config.Config {
	c := testConfig()
	c.MateAppOrigins = []string{"https://app.example", "http://localhost:5173"}
	return c
}

func registered() Deps {
	return Deps{OAuthClient: func() (OAuthClient, bool) {
		return OAuthClient{
			ClientID:     "client-1",
			RedirectURIs: []string{"http://localhost:5173/gitea/callback", "https://app.example/gitea/callback"},
		}, true
	}}
}

// The app asks for its client id before anybody has signed in, so the route
// takes no credential: a public client's id is not a secret, and the redirect
// URIs are what protect it.
func TestOAuthClientNeedsNoCredential(t *testing.T) {
	s := New(appConfig(), slog.New(slog.DiscardHandler), registered())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gitea/oauth-client", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body)
	}
	var body struct {
		ClientID     string   `json:"clientId"`
		RedirectURIs []string `json:"redirectUris"`
		AuthorizeURL string   `json:"authorizeUrl"`
		TokenURL     string   `json:"tokenUrl"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.ClientID != "client-1" {
		t.Errorf("clientId = %q", body.ClientID)
	}
	want := []string{"http://localhost:5173/gitea/callback", "https://app.example/gitea/callback"}
	if !slices.Equal(body.RedirectURIs, want) {
		t.Errorf("redirectUris = %v, want %v", body.RedirectURIs, want)
	}
	if body.AuthorizeURL != "https://git.example/login/oauth/authorize" {
		t.Errorf("authorizeUrl = %q", body.AuthorizeURL)
	}
	if body.TokenURL != "https://git.example/login/oauth/access_token" {
		t.Errorf("tokenUrl = %q", body.TokenURL)
	}
}

// Until a pass has registered the client there is nothing to answer, and the
// app must wait rather than start a flow with a client id it invented.
func TestOAuthClientBeforeTheFirstPass(t *testing.T) {
	s := New(appConfig(), slog.New(slog.DiscardHandler), Deps{
		OAuthClient: func() (OAuthClient, bool) { return OAuthClient{}, false },
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gitea/oauth-client", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error != "not_registered_yet" {
		t.Errorf("error = %q", body.Error)
	}
}

// The route is the app's, and the app is a browser page. Every origin it runs
// from is allowed by name — a runner in this project can reach this port, so
// nothing else is.
func TestOAuthClientCORS(t *testing.T) {
	s := New(appConfig(), slog.New(slog.DiscardHandler), registered())
	cases := []struct {
		name, origin, wantAllow string
		method                  string
		wantStatus              int
	}{
		{name: "the web shell", origin: "https://app.example", wantAllow: "https://app.example", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "a local shell", origin: "http://localhost:5173", wantAllow: "http://localhost:5173", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "its preflight", origin: "http://localhost:5173", wantAllow: "http://localhost:5173", method: http.MethodOptions, wantStatus: http.StatusNoContent},
		{name: "somebody else", origin: "https://evil.example", wantAllow: "", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "a near miss", origin: "http://127.0.0.1:5173", wantAllow: "", method: http.MethodGet, wantStatus: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/gitea/oauth-client", nil)
			req.Header.Set("Origin", tc.origin)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if rr.Header().Get("Vary") != "Origin" {
				t.Errorf("Vary = %q, want Origin", rr.Header().Get("Vary"))
			}
		})
	}
}

// A broker with no source for the client does not serve the route at all.
func TestOAuthClientUnwired(t *testing.T) {
	s := New(appConfig(), slog.New(slog.DiscardHandler), Deps{})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gitea/oauth-client", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
