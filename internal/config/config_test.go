package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func full() map[string]string {
	return map[string]string{
		"ZEROPS_TOKEN":         "zt",
		"ZEROPS_API_URL":       "https://api.app-prg1.zerops.io",
		"ZEROPS_CLIENT_ID":     "org-1",
		"ZEROPS_PROJECT_ID":    "prj-1",
		"GITEA_URL":            "http://web:3000",
		"GITEA_PUBLIC_URL":     "https://web-1234-3000.prg1.zerops.app",
		"GITEA_ADMIN_TOKEN":    "gt",
		"GITEA_ADMIN_PASSWORD": "gp",
		"GITEA_WEBHOOK_SECRET": "ws",
		"OIDC_CLIENT_SECRET":   "cs",
		"OIDC_SEED":            "seed",
		"BROKER_PUBLIC_URL":    "https://broker-1234-8080.prg1.zerops.app",
		"MATE_APP_URL":         "https://app.example",
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr []string // substrings the error must carry
		check   func(t *testing.T, c *Config)
	}{
		{
			name: "a complete environment loads with the documented defaults",
			check: func(t *testing.T, c *Config) {
				if c.ListenAddr != ":8080" {
					t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
				}
				if c.MirrorInterval != 3*time.Minute {
					t.Errorf("MirrorInterval = %v, want 3m", c.MirrorInterval)
				}
				if c.MirrorCap != 10 {
					t.Errorf("MirrorCap = %d, want 10", c.MirrorCap)
				}
				if c.RunnerQuietPeriod != 15*time.Minute {
					t.Errorf("RunnerQuietPeriod = %v, want 15m", c.RunnerQuietPeriod)
				}
				if c.GiteaAdminUser != "admin" {
					t.Errorf("GiteaAdminUser = %q, want admin", c.GiteaAdminUser)
				}
			},
		},
		{
			name:   "the site admin's name can be overridden",
			mutate: func(m map[string]string) { m["GITEA_ADMIN_USERNAME"] = "root" },
			check: func(t *testing.T, c *Config) {
				if c.GiteaAdminUser != "root" {
					t.Errorf("GiteaAdminUser = %q", c.GiteaAdminUser)
				}
			},
		},
		{
			name:    "the site admin's password is required: the token routes refuse an API token",
			mutate:  func(m map[string]string) { delete(m, "GITEA_ADMIN_PASSWORD") },
			wantErr: []string{"GITEA_ADMIN_PASSWORD"},
		},
		{
			name:    "one missing variable is named",
			mutate:  func(m map[string]string) { delete(m, "OIDC_SEED") },
			wantErr: []string{"missing", "OIDC_SEED"},
		},
		{
			name: "every missing variable is named at once",
			mutate: func(m map[string]string) {
				delete(m, "ZEROPS_TOKEN")
				delete(m, "GITEA_ADMIN_TOKEN")
				delete(m, "MATE_APP_URL")
			},
			wantErr: []string{"ZEROPS_TOKEN", "GITEA_ADMIN_TOKEN", "MATE_APP_URL"},
		},
		{
			name:    "a blank variable counts as missing",
			mutate:  func(m map[string]string) { m["ZEROPS_CLIENT_ID"] = "   " },
			wantErr: []string{"ZEROPS_CLIENT_ID"},
		},
		{
			name:    "a URL variable must be absolute",
			mutate:  func(m map[string]string) { m["GITEA_URL"] = "web:3000" },
			wantErr: []string{"GITEA_URL is not an absolute URL"},
		},
		{
			name:   "a trailing slash is trimmed off every URL",
			mutate: func(m map[string]string) { m["BROKER_PUBLIC_URL"] = "https://broker.example/" },
			check: func(t *testing.T, c *Config) {
				if c.BrokerPublicURL != "https://broker.example" {
					t.Errorf("BrokerPublicURL = %q", c.BrokerPublicURL)
				}
			},
		},
		{
			name: "the tunables can be overridden",
			mutate: func(m map[string]string) {
				m["MIRROR_INTERVAL"] = "45s"
				m["MIRROR_CAP"] = "3"
				m["RUNNER_QUIET_PERIOD"] = "2m"
			},
			check: func(t *testing.T, c *Config) {
				if c.MirrorInterval != 45*time.Second || c.MirrorCap != 3 || c.RunnerQuietPeriod != 2*time.Minute {
					t.Errorf("tunables = %v %d %v", c.MirrorInterval, c.MirrorCap, c.RunnerQuietPeriod)
				}
			},
		},
		{
			name:    "a nonsense duration is refused, not silently defaulted",
			mutate:  func(m map[string]string) { m["MIRROR_INTERVAL"] = "soon" },
			wantErr: []string{"MIRROR_INTERVAL is not a positive duration"},
		},
		{
			name:    "a non-positive cap is refused",
			mutate:  func(m map[string]string) { m["MIRROR_CAP"] = "0" },
			wantErr: []string{"MIRROR_CAP is not a positive integer"},
		},
		{
			name:   "LISTEN_ADDR is honoured when set",
			mutate: func(m map[string]string) { m["LISTEN_ADDR"] = ":9000" },
			check: func(t *testing.T, c *Config) {
				if c.ListenAddr != ":9000" {
					t.Errorf("ListenAddr = %q", c.ListenAddr)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := full()
			if tc.mutate != nil {
				tc.mutate(m)
			}
			c, err := Load(env(m))
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Load succeeded, want an error naming %v", tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

func TestLoadErrorNeverCarriesAValue(t *testing.T) {
	m := full()
	m["ZEROPS_TOKEN"] = ""
	m["GITEA_URL"] = "not a url but secret-looking: hunter2"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error quotes a value: %q", err)
	}
}

func TestSecretRedacts(t *testing.T) {
	s := Secret("hunter2")
	if got := fmt.Sprintf("%v/%s/%d", s, s, len(s.Reveal())); got != "[redacted]/[redacted]/7" {
		t.Errorf("formatted secret = %q", got)
	}
}

func TestGiteaHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://web-1234-3000.prg1.zerops.app", "web-1234-3000.prg1.zerops.app"},
		{"http://127.0.0.1:3301", "127.0.0.1:3301"},
		{"https://git.example.com/", "git.example.com"},
	}
	for _, tc := range cases {
		c := &Config{GiteaPublicURL: tc.in}
		if got := c.GiteaHost(); got != tc.want {
			t.Errorf("GiteaHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
