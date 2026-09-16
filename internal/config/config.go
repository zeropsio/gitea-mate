// Package config reads and validates the broker's environment.
//
// Every variable is listed in docs/vocabulary.md, section "The broker's
// environment". A value is read once, at start; a missing one is named in the
// error and a secret is never part of it. Secrets carry the [Secret] type,
// whose String method redacts, so a stray %v or a logged struct cannot leak
// one.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Secret is a value that must never reach a log, an error or a response.
type Secret string

// String redacts. It is what fmt and log/slog print.
func (s Secret) String() string { return "[redacted]" }

// Reveal returns the value. Every call site is a place to look when auditing
// where a secret travels.
func (s Secret) Reveal() string { return string(s) }

// Config is the broker's whole configuration.
type Config struct {
	ZeropsToken     Secret
	ZeropsAPIURL    string
	ZeropsClientID  string
	ZeropsProjectID string

	GiteaURL        string
	GiteaPublicURL  string
	GiteaAdminToken Secret
	// GiteaAdminUser and GiteaAdminPassword are the site admin's basic-auth
	// credentials. They exist because Gitea's token routes
	// (/users/{login}/tokens) answer 401 "auth required" to an API token,
	// however privileged — measured on 1.27.2 — so minting a Mate bot's
	// credential has no other path. docs/vocabulary.md lists only
	// GITEA_ADMIN_TOKEN; guide 1.3 names both.
	GiteaAdminUser     string
	GiteaAdminPassword Secret
	GiteaWebhookSecret Secret

	OIDCClientSecret Secret
	OIDCSeed         Secret
	BrokerPublicURL  string
	MateAppURL       string

	ListenAddr string

	// MirrorInterval is how often the rights loop runs a pass.
	MirrorInterval time.Duration
	// MirrorCap is the most people, tokens or memberships one pass may
	// disable, remove or delete before it stops and reports instead.
	MirrorCap int
	// RunnerQuietPeriod is how long a group's runner stays up after its last
	// job finished.
	RunnerQuietPeriod time.Duration
}

// GiteaHost is GITEA_PUBLIC_URL without its scheme. A gitea-signin throwaway
// names it (docs/broker-api.md, "Proving a person", step 4).
func (c *Config) GiteaHost() string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c.GiteaPublicURL, "https://"), "http://"), "/")
}

const (
	defaultListenAddr        = ":8080"
	defaultGiteaAdminUser    = "admin"
	defaultMirrorInterval    = 3 * time.Minute
	defaultMirrorCap         = 10
	defaultRunnerQuietPeriod = 15 * time.Minute
)

// Load reads the configuration with getenv, which is os.Getenv in production
// and a map in tests. Every problem is collected, so one start names every
// missing variable rather than one per restart.
func Load(getenv func(string) string) (*Config, error) {
	var missing []string
	var bad []string

	req := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	reqURL := func(name string) string {
		v := req(name)
		if v == "" {
			return ""
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			bad = append(bad, name+" is not an absolute URL")
			return ""
		}
		return strings.TrimSuffix(v, "/")
	}
	optDuration := func(name string, def time.Duration) time.Duration {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad = append(bad, name+" is not a positive duration")
			return def
		}
		return d
	}
	optInt := func(name string, def int) int {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			bad = append(bad, name+" is not a positive integer")
			return def
		}
		return n
	}

	c := &Config{
		ZeropsToken:     Secret(req("ZEROPS_TOKEN")),
		ZeropsAPIURL:    reqURL("ZEROPS_API_URL"),
		ZeropsClientID:  req("ZEROPS_CLIENT_ID"),
		ZeropsProjectID: req("ZEROPS_PROJECT_ID"),

		GiteaURL:           reqURL("GITEA_URL"),
		GiteaPublicURL:     reqURL("GITEA_PUBLIC_URL"),
		GiteaAdminToken:    Secret(req("GITEA_ADMIN_TOKEN")),
		GiteaAdminUser:     strings.TrimSpace(getenv("GITEA_ADMIN_USERNAME")),
		GiteaAdminPassword: Secret(req("GITEA_ADMIN_PASSWORD")),
		GiteaWebhookSecret: Secret(req("GITEA_WEBHOOK_SECRET")),

		OIDCClientSecret: Secret(req("OIDC_CLIENT_SECRET")),
		OIDCSeed:         Secret(req("OIDC_SEED")),
		BrokerPublicURL:  reqURL("BROKER_PUBLIC_URL"),
		MateAppURL:       reqURL("MATE_APP_URL"),

		ListenAddr: strings.TrimSpace(getenv("LISTEN_ADDR")),

		MirrorInterval:    optDuration("MIRROR_INTERVAL", defaultMirrorInterval),
		MirrorCap:         optInt("MIRROR_CAP", defaultMirrorCap),
		RunnerQuietPeriod: optDuration("RUNNER_QUIET_PERIOD", defaultRunnerQuietPeriod),
	}
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.GiteaAdminUser == "" {
		c.GiteaAdminUser = defaultGiteaAdminUser
	}

	var problems []string
	if len(missing) > 0 {
		problems = append(problems, "missing: "+strings.Join(missing, ", "))
	}
	problems = append(problems, bad...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("broker configuration: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// ErrNoConfig is returned by helpers that need a configuration they were not
// given. It exists so a caller can tell "not configured" from "refused".
var ErrNoConfig = errors.New("no configuration")
