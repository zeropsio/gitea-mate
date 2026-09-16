package giteamate_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gitea/wait-for-url.sh is what stands between Gitea's `admin auth add-oauth`
// and a broker that is not listening yet. Measured on 2026-09-16: adding the
// `zerops` source while the discovery URL answers 502 writes the login source
// row but leaves Gitea without its provider — "Failed to create OpenID Connect
// Provider … Non-success code for Discovery URL: 502" — so the sign-in page
// offers no Zerops link, and nothing but a restart of Gitea ever fixes it.
func TestWaitForURL(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on the path")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not on the path")
	}

	cases := []struct {
		name     string
		failFor  int32 // how many requests answer 502 before one answers 200
		deadline string
		wantOK   bool
	}{
		{name: "an answer straight away", failFor: 0, deadline: "10", wantOK: true},
		{name: "a broker that is still starting", failFor: 3, deadline: "10", wantOK: true},
		{name: "a broker that never comes up", failFor: 1000, deadline: "2", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if seen.Add(1) <= tc.failFor {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issuer":"x"}`))
			}))
			defer srv.Close()

			cmd := exec.Command("bash", "gitea/wait-for-url.sh", srv.URL, tc.deadline, "1")
			out, err := cmd.CombinedOutput()
			if ok := err == nil; ok != tc.wantOK {
				t.Fatalf("exit ok = %v, want %v (output %q)", ok, tc.wantOK, out)
			}
			if tc.wantOK && seen.Load() < tc.failFor+1 {
				t.Errorf("gave up after %d requests", seen.Load())
			}
		})
	}
}

// TestAdminInitWaitsForTheBroker pins the order: nothing adds the login source
// before the discovery URL has answered.
func TestAdminInitWaitsForTheBroker(t *testing.T) {
	raw, err := os.ReadFile("gitea/admin-init.sh")
	if err != nil {
		t.Fatalf("reading admin-init.sh: %v", err)
	}
	script := string(raw)
	wait := strings.Index(script, "wait-for-url.sh")
	add := strings.Index(script, "auth add-oauth")
	if wait < 0 {
		t.Fatal("admin-init.sh does not wait for the broker's discovery URL")
	}
	if add < 0 {
		t.Fatal("admin-init.sh no longer adds the login source")
	}
	if wait > add {
		t.Error("admin-init.sh adds the login source before it waits for the broker")
	}
}

// TestGiteaRuntimeShipsTheWait: the wait runs on the Gitea container, so both
// the script and curl have to be there — the runtime's prepare installs only
// what it names.
func TestGiteaRuntimeShipsTheWait(t *testing.T) {
	raw, err := os.ReadFile("zerops.yaml")
	if err != nil {
		t.Fatalf("reading zerops.yaml: %v", err)
	}
	gitea := string(raw)
	if i := strings.Index(gitea, "- setup: broker"); i > 0 {
		gitea = gitea[:i]
	}
	if !strings.Contains(gitea, "- gitea/wait-for-url.sh") {
		t.Error("the gitea setup does not deploy gitea/wait-for-url.sh")
	}
	run := gitea[strings.Index(gitea, "    run:"):]
	if !strings.Contains(run, "curl") {
		t.Error("the gitea runtime does not install curl, which the wait needs")
	}
}

func TestWaitForURLIsExecutable(t *testing.T) {
	info, err := os.Stat("gitea/wait-for-url.sh")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode %v is not executable", info.Mode().Perm())
	}
	_ = time.Now
}
