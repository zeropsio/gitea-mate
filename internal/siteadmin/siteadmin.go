// Package siteadmin resolves the site admin's Gitea credentials from the
// platform.
//
// The broker's GITEA_ADMIN_TOKEN and GITEA_ADMIN_PASSWORD are references to
// web's variables, which Gitea's first boot publishes (gitea/admin-init.sh).
// On a fresh account the broker can start before that has happened, and a
// reference that has not resolved reaches the container VERBATIM — `${…}`,
// not empty (ledger 2026-09-16) — or not at all. A broker that copied the
// pair at start then held it, unresolved, for its whole life: every pass
// answered 401 until somebody restarted the container (measured 2026-09-17).
//
// So the pair the environment hands over is a hint, not the truth. A value
// that has not [Arrived] is read from web's own variables through the Zerops
// API, with the broker's token on its own project (a sensitive value comes
// back in clear to it — measured 2026-09-17); a pair Gitea refuses is read
// again the same way, since a first boot on a reset volume mints anew. The
// pair is held in memory once it is good, and nothing here ever logs it.
package siteadmin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The service that publishes the pair, and the two variables it publishes
// as its own (gitea/admin-init.sh, provision_admin).
const (
	WebHostname = "web"
	VarToken    = "GITEA_ADMIN_TOKEN"
	VarPassword = "GITEA_ADMIN_PASSWORD"
)

// Arrived reports whether a value the platform was to fill in has: not empty,
// and not the `${…}` reference the import wrote, which reaches a container
// verbatim until the platform resolves it. The same rule admin-init.sh
// applies before it trusts a sibling's value.
func Arrived(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	return !(strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}"))
}

// Config builds a Resolver.
type Config struct {
	// Env is the pair as the environment handed it — GITEA_ADMIN_TOKEN and
	// GITEA_ADMIN_PASSWORD — which may not have arrived.
	Env gitea.AdminCredentials
	// Zerops is the broker's own client, and ClientID and ProjectID name the
	// org and the Gitea project, where web lives.
	Zerops    *zerops.Client
	ClientID  string
	ProjectID string
	Log       *slog.Logger
}

// Resolver is a [gitea.AdminSource]: it answers the pair it holds, and reads
// one from web when it holds none or Gitea refused the one it had.
type Resolver struct {
	zerops    *zerops.Client
	clientID  string
	projectID string
	log       *slog.Logger

	// mu guards the pair and is held across a read of web, so two callers
	// refused the same pair at once cost one read: the second finds the pair
	// already replaced.
	mu   sync.Mutex
	pair gitea.AdminCredentials
	held bool
}

// New builds a resolver. An environment pair that has arrived is held from
// the start; one that has not is read from web at the first ask.
func New(cfg Config) *Resolver {
	r := &Resolver{zerops: cfg.Zerops, clientID: cfg.ClientID, projectID: cfg.ProjectID, log: cfg.Log}
	if r.log == nil {
		r.log = slog.Default()
	}
	if Arrived(cfg.Env.Token) && Arrived(cfg.Env.Password) {
		r.pair, r.held = cfg.Env, true
	}
	return r
}

// Admin implements gitea.AdminSource.
func (r *Resolver) Admin(ctx context.Context) (gitea.AdminCredentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held {
		return r.pair, nil
	}
	return r.read(ctx)
}

// Refused implements gitea.AdminSource: the pair Gitea refused is dropped and
// web is read again. A caller refused a pair this resolver has already
// replaced gets the replacement, and web is not read for it.
func (r *Resolver) Refused(ctx context.Context, used gitea.AdminCredentials) (gitea.AdminCredentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held && r.pair != used {
		return r.pair, nil
	}
	r.held = false
	return r.read(ctx)
}

// read finds web on the Gitea project and takes the pair from its variables.
// Called with the lock held. Every failure names what could not be read and
// never a value: a broker that cannot reach the platform, or a web that has
// not published yet, is a condition the next ask retries.
func (r *Resolver) read(ctx context.Context) (gitea.AdminCredentials, error) {
	services, err := r.zerops.Services(ctx, r.clientID, r.projectID)
	if err != nil {
		return gitea.AdminCredentials{}, fmt.Errorf("the Gitea project's services could not be read: %w", err)
	}
	var web *zerops.Service
	for i := range services {
		if services[i].Name == WebHostname {
			web = &services[i]
			break
		}
	}
	if web == nil {
		return gitea.AdminCredentials{}, fmt.Errorf("the Gitea project %s has no %s service", r.projectID, WebHostname)
	}
	vars, err := r.zerops.UserData(ctx, web.ID)
	if err != nil {
		return gitea.AdminCredentials{}, fmt.Errorf("%s's variables could not be read: %w", WebHostname, err)
	}
	var pair gitea.AdminCredentials
	for _, v := range vars {
		switch v.Key {
		case VarToken:
			pair.Token = v.Content
		case VarPassword:
			pair.Password = v.Content
		}
	}
	if !Arrived(pair.Token) || !Arrived(pair.Password) {
		return gitea.AdminCredentials{}, fmt.Errorf("%s has not published %s and %s yet: Gitea's first boot does that", WebHostname, VarToken, VarPassword)
	}
	r.pair, r.held = pair, true
	r.log.Info("the site admin's credentials were read from the platform", "service", WebHostname)
	return pair, nil
}
