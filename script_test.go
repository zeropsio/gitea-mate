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

// TestAdminInitOIDCSecret drives add_oidc_source with a stubbed Gitea binary.
// The case that matters is the middle one: `${broker_OIDC_CLIENT_SECRET}`
// reaches this container verbatim while the platform has not resolved the
// sibling reference yet. It is 28 characters, so the emptiness guard lets it
// through, and `add-oauth --secret` writes a login source Gitea can never
// authenticate with — every sign-in afterwards ends
// `Failed OAuth callback: (internal) oauth2: "invalid_client"`, and the
// "already exists" guard means no later boot repairs it (measured live on a
// real Mate, 2026-09-16).
func TestAdminInitOIDCSecret(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on the path")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}

	cases := []struct {
		name     string
		secret   string
		existing bool
		wantCmd  string
		wantNone bool
	}{
		{name: "a resolved secret and no source", secret: "s3cr3t", wantCmd: "add-oauth"},
		{name: "the reference has not resolved", secret: "${broker_OIDC_CLIENT_SECRET}", wantNone: true},
		{name: "an empty secret", secret: "", wantNone: true},
		{name: "a source from an earlier boot is repaired", secret: "s3cr3t", existing: true, wantCmd: "update-oauth"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issuer":"x"}`))
			}))
			defer srv.Close()

			list := "ID   Name     Type   Enabled\n"
			if tc.existing {
				list += "7    zerops   OAuth2   true\n"
			}
			stub := "#!/usr/bin/env bash\n" +
				"printf '%s\\n' \"$*\" >> " + dir + "/calls\n" +
				"if [ \"$1\" = admin ] && [ \"$2\" = auth ] && [ \"$3\" = list ]; then\n" +
				"  cat <<'EOF'\n" + list + "EOF\n" +
				"fi\nexit 0\n"
			bin := dir + "/gitea-stub"
			if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
				t.Fatalf("write stub: %v", err)
			}

			cmd := exec.Command("bash", "-c", "ADMIN_INIT_SOURCE_ONLY=1 . ./gitea/admin-init.sh && add_oidc_source")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"GITEA_BIN="+bin, "CONF="+dir+"/app.ini",
				"BROKER_PUBLIC_URL="+srv.URL, "OIDC_CLIENT_SECRET="+tc.secret)
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("add_oidc_source: %v\n%s", runErr, out)
			}
			calls, _ := os.ReadFile(dir + "/calls")
			got := string(calls)
			switch {
			case tc.wantNone:
				if strings.Contains(got, "add-oauth") || strings.Contains(got, "update-oauth") {
					t.Errorf("the source must be left for a later boot; calls were:\n%s\noutput:\n%s", got, out)
				}
			default:
				if !strings.Contains(got, tc.wantCmd) {
					t.Errorf("want a %s call, calls were:\n%s\noutput:\n%s", tc.wantCmd, got, out)
				}
				if !strings.Contains(got, tc.secret) {
					t.Errorf("the call must carry the resolved secret; calls were:\n%s", got)
				}
			}
		})
	}
}

// start.sh exits non-zero while Gitea has no `zerops` login source, so the
// platform re-runs the start command — admin-init.sh and then start.sh —
// instead of serving a Gitea nobody can sign in to. Measured on the owner's
// run of 2026-09-17: admin-init.sh left the source "for a later boot" at
// 11:20:38 because the broker's secret had not resolved, and no boot came
// until a restart by hand at 11:38; every sign-in until then was refused with
// "login source does not exist [id: 1]".
func TestStartRefusesToServeWithoutTheZeropsSource(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on the path")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for _, tc := range []struct {
		name  string
		list  string // what `gitea admin auth list` prints; empty = the command fails
		serve bool
	}{
		{"the source is there", "ID   Name     Type   Enabled\n1    zerops   OAuth2   true\n", true},
		{"no source yet", "ID   Name     Type   Enabled\n", false},
		{"the sources cannot be listed", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := "#!/usr/bin/env bash\n" +
				"if [ \"$1\" = admin ] && [ \"$2\" = auth ] && [ \"$3\" = list ]; then\n"
			if tc.list == "" {
				stub += "  exit 1\n"
			} else {
				stub += "  printf '%s' \"$LIST\"\n"
			}
			stub += "fi\nexit 0\n"
			bin := dir + "/gitea-stub"
			if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
				t.Fatalf("write stub: %v", err)
			}
			cmd := exec.Command("bash", "-c", "START_SOURCE_ONLY=1 . ./gitea/start.sh && require_zerops_source")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GITEA_BIN="+bin, "CONF="+dir+"/app.ini", "LIST="+tc.list)
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.serve {
				t.Fatalf("serve = %v, want %v\n%s", runErr == nil, tc.serve, out)
			}
			if !tc.serve && !strings.Contains(string(out), "not there yet, restarting") {
				t.Errorf("a refused boot must say why; output:\n%s", out)
			}
		})
	}
}
