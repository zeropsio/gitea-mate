// Package config reads and validates the broker's environment.
//
// Every variable is listed in docs/vocabulary.md, section "The broker's
// environment", with one correction the platform forced on 2026-09-16: an
// import refuses any custom variable whose name begins with `ZEROPS_`
// (`400 userDataZeropsPrefixForbidden`, case-insensitive), so the four
// variables that name Zerops are `MATE_ZEROPS_TOKEN`, `MATE_ZEROPS_API_URL`,
// `MATE_ZEROPS_CLIENT_ID` and `MATE_ZEROPS_PROJECT_ID`. A value is read once, at start; a missing one is named in the
// error and a secret is never part of it. Secrets carry the [Secret] type,
// whose String method redacts, so a stray %v or a logged struct cannot leak
// one.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
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

	GiteaURL       string
	GiteaPublicURL string
	// GiteaAdminToken and GiteaAdminPassword are the site admin's pair as the
	// environment handed it — a hint, not the truth. Both are references to
	// web's variables, which Gitea's first boot publishes, and the broker can
	// start before that: the value is then empty or the `${…}` reference
	// verbatim. Neither is required here; internal/siteadmin tells a value
	// from a reference and reads the pair from the platform when it must.
	//
	// The password exists because Gitea's token routes
	// (/users/{login}/tokens) answer 401 "auth required" to an API token,
	// however privileged — measured on 1.27.2 — so minting a Mate bot's
	// credential has no other path. docs/vocabulary.md lists only
	// GITEA_ADMIN_TOKEN; guide 1.3 names both.
	GiteaAdminToken    Secret
	GiteaAdminUser     string
	GiteaAdminPassword Secret
	GiteaWebhookSecret Secret

	OIDCClientSecret Secret
	OIDCSeed         Secret
	BrokerPublicURL  string
	MateAppURL       string
	// MateAppOrigins is every origin the Mate app runs from — the web shell,
	// the desktop and mobile shells, a dev server — matched literally. The
	// broker answers the app's own calls (POST /person/token) with CORS for
	// each and nothing else. MateAppURL is always one of them.
	MateAppOrigins []string

	ListenAddr string

	// MirrorInterval is how often the rights loop runs a pass.
	MirrorInterval time.Duration
	// MirrorCap is the most people, tokens or memberships one pass may
	// disable, remove or delete before it stops and reports instead.
	MirrorCap int
	// GiteaOIDCSourceID is the id of Gitea's `zerops` login source, which the
	// recipe adds once at first boot: the first and only source, so 1. A
	// person the broker creates is bound to it, so their sign-in to Gitea's
	// own pages lands on the same account. Gitea 1.27 has no API that lists
	// sources, hence a variable rather than a lookup.
	GiteaOIDCSourceID int64
	// AppTokenTTL is how long a person's app token (POST /person/token) lives
	// before the rights loop retires it.
	AppTokenTTL time.Duration
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
	defaultGiteaOIDCSourceID = 1
	defaultAppTokenTTL       = 12 * time.Hour
)

// Load reads the configuration with getenv, which is os.Getenv in production
// and a map in tests. Every problem is collected, so one start names every
// missing variable rather than one per restart. The site admin's pair is the
// one thing a start does not insist on: it is resolved from the platform.
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

	// originList reads a comma-separated list of origins. An entry that is not
	// an absolute origin is named by the variable, never quoted.
	originList := func(name string) []string {
		var out []string
		for _, raw := range strings.Split(getenv(name), ",") {
			entry := strings.TrimSuffix(strings.TrimSpace(raw), "/")
			if entry == "" {
				continue
			}
			u, err := url.Parse(entry)
			if err != nil || u.Scheme == "" || u.Host == "" {
				bad = append(bad, name+" carries an entry that is not an absolute origin")
				continue
			}
			if !slices.Contains(out, entry) {
				out = append(out, entry)
			}
		}
		return out
	}

	c := &Config{
		ZeropsToken:     Secret(req("MATE_ZEROPS_TOKEN")),
		ZeropsAPIURL:    reqURL("MATE_ZEROPS_API_URL"),
		ZeropsClientID:  req("MATE_ZEROPS_CLIENT_ID"),
		ZeropsProjectID: req("MATE_ZEROPS_PROJECT_ID"),

		GiteaURL:           reqURL("GITEA_URL"),
		GiteaPublicURL:     reqURL("GITEA_PUBLIC_URL"),
		GiteaAdminToken:    Secret(strings.TrimSpace(getenv("GITEA_ADMIN_TOKEN"))),
		GiteaAdminUser:     strings.TrimSpace(getenv("GITEA_ADMIN_USERNAME")),
		GiteaAdminPassword: Secret(strings.TrimSpace(getenv("GITEA_ADMIN_PASSWORD"))),
		GiteaWebhookSecret: Secret(req("GITEA_WEBHOOK_SECRET")),

		OIDCClientSecret: Secret(req("OIDC_CLIENT_SECRET")),
		OIDCSeed:         Secret(req("OIDC_SEED")),
		BrokerPublicURL:  reqURL("BROKER_PUBLIC_URL"),
		MateAppURL:       reqURL("MATE_APP_URL"),
		MateAppOrigins:   originList("MATE_APP_ORIGINS"),

		ListenAddr: strings.TrimSpace(getenv("LISTEN_ADDR")),

		MirrorInterval:    optDuration("MIRROR_INTERVAL", defaultMirrorInterval),
		MirrorCap:         optInt("MIRROR_CAP", defaultMirrorCap),
		RunnerQuietPeriod: optDuration("RUNNER_QUIET_PERIOD", defaultRunnerQuietPeriod),
		GiteaOIDCSourceID: int64(optInt("GITEA_OIDC_SOURCE_ID", defaultGiteaOIDCSourceID)),
		AppTokenTTL:       optDuration("APP_TOKEN_TTL", defaultAppTokenTTL),
	}
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.GiteaAdminUser == "" {
		c.GiteaAdminUser = defaultGiteaAdminUser
	}
	// The app's own URL is an origin of the app. A list that forgot it would
	// register a client the app cannot use from the page it signs in on.
	if c.MateAppURL != "" && !slices.Contains(c.MateAppOrigins, c.MateAppURL) {
		c.MateAppOrigins = append(c.MateAppOrigins, c.MateAppURL)
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
