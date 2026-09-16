package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		ZeropsToken:        "zt",
		ZeropsAPIURL:       "https://api.example",
		ZeropsClientID:     "org-1",
		ZeropsProjectID:    "prj-gitea",
		GiteaURL:           "http://web:3000",
		GiteaPublicURL:     "https://git.example",
		GiteaAdminToken:    "gt",
		GiteaAdminUser:     "admin",
		GiteaAdminPassword: "gp",
		GiteaWebhookSecret: "ws",
		OIDCClientSecret:   "cs",
		OIDCSeed:           "seed",
		BrokerPublicURL:    "https://broker.example",
		MateAppURL:         "https://app.example",
		ListenAddr:         ":8080",
	}
}

func TestHealthz(t *testing.T) {
	s := New(testConfig(), slog.New(slog.DiscardHandler), Deps{})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	s := New(testConfig(), slog.New(slog.DiscardHandler), Deps{})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/nothing-here", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestWriteError(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteError(rr, http.StatusForbidden, "not_owner", "you do not own this Mate")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error != "not_owner" || body.Message != "you do not own this Mate" || body.Reason != "" {
		t.Errorf("body = %+v", body)
	}
	if strings.Contains(rr.Body.String(), `"reason"`) {
		t.Errorf("an empty reason is written out: %s", rr.Body.String())
	}
}

func TestWriteErrorReason(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteErrorReason(rr, http.StatusUnauthorized, "throwaway_invalid", "that token is not a sign-in throwaway", "wrong_name")

	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error != "throwaway_invalid" || body.Reason != "wrong_name" {
		t.Errorf("body = %+v", body)
	}
}

func TestRequestLogCarriesNoBodyOrHeader(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusTeapot, "nope", "no")
	})
	h := logRequests(log, mux)

	req := httptest.NewRequest(http.MethodPost, "/echo?rid=RIDVALUE", strings.NewReader(`{"token":"SECRETBODY"}`))
	req.Header.Set("Authorization", "Bearer SECRETHEADER")
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	for _, secret := range []string{"SECRETBODY", "SECRETHEADER", "Authorization"} {
		if strings.Contains(line, secret) {
			t.Errorf("the log line carries %q: %s", secret, line)
		}
	}
	for _, want := range []string{`"method":"POST"`, `"path":"/echo"`, `"status":418`} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line lacks %s: %s", want, line)
		}
	}
}
