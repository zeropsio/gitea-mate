// Command broker is the Zerops ↔ Gitea broker: it mirrors Zerops permissions
// into a Gitea instance, signs people in to Gitea and delivers every Mate its
// Gitea access.
//
// Its API is docs/broker-api.md; every name it uses is in docs/vocabulary.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	giteamate "github.com/zeropsio/gitea-mate"
	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/oidc"
	"github.com/zeropsio/gitea-mate/internal/pipeline"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/server"
	"github.com/zeropsio/gitea-mate/internal/siteadmin"
	"github.com/zeropsio/gitea-mate/internal/throwaway"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("broker stopped", "err", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	zeropsClient := zerops.New(cfg.ZeropsAPIURL, cfg.ZeropsToken.Reveal(), nil)
	// The site admin's pair is what the environment says only once it has
	// arrived; until then, and whenever Gitea refuses it, it is read from
	// web's variables. A start with the pair still unresolved serves, and the
	// first pass resolves it.
	admin := siteadmin.New(siteadmin.Config{
		Env:       gitea.AdminCredentials{Token: cfg.GiteaAdminToken.Reveal(), Password: cfg.GiteaAdminPassword.Reveal()},
		Zerops:    zeropsClient,
		ClientID:  cfg.ZeropsClientID,
		ProjectID: cfg.ZeropsProjectID,
		Log:       log,
	})
	giteaClient := gitea.New(gitea.Config{
		BaseURL:   cfg.GiteaURL,
		Admin:     admin,
		AdminUser: cfg.GiteaAdminUser,
	})

	checker := &throwaway.Checker{
		Broker: zeropsClient,
		AsCaller: func(bearer string) *zerops.Client {
			return zerops.New(cfg.ZeropsAPIURL, bearer, nil)
		},
		ClientID:  cfg.ZeropsClientID,
		GiteaHost: cfg.GiteaHost(),
	}

	rights := &mirror.Mirror{
		Zerops:         zeropsClient,
		Gitea:          giteaClient,
		Log:            log,
		ClientID:       cfg.ZeropsClientID,
		GiteaProjectID: cfg.ZeropsProjectID,
		AdminLogin:     cfg.GiteaAdminUser,
		HookURL:        cfg.BrokerPublicURL + "/hooks/gitea",
		// Every registered Mate's container is given these two, and a token.
		GiteaPublicURL:  cfg.GiteaPublicURL,
		BrokerPublicURL: cfg.BrokerPublicURL,
		Cap:             cfg.MirrorCap,
		AppTokenTTL:     cfg.AppTokenTTL,
	}
	rights.SetHookSecret(cfg.GiteaWebhookSecret.Reveal())
	loop := mirror.NewLoop(rights, log, cfg.MirrorInterval, mirror.DefaultNudgeDelay)

	// What a proven person may do, read live: the OIDC consent and the app's
	// own sign-in (POST /person/token) ask the same question.
	rightsFor := oidc.RightsFunc(func(ctx context.Context, caller throwaway.Caller) (roles.Rights, error) {
		org, err := mirror.ReadOrgWith(ctx, zeropsClient, cfg.ZeropsClientID, cfg.ZeropsProjectID, caller.Members)
		if err != nil {
			return roles.Rights{}, err
		}
		computed, found := org.RightsFor(caller.UserID)
		if !found {
			return roles.Rights{}, fmt.Errorf("the org's member list does not carry %s", caller.UserID)
		}
		return computed, nil
	})

	provider, err := oidc.New(oidc.Config{
		Issuer:       cfg.BrokerPublicURL,
		ClientSecret: cfg.OIDCClientSecret.Reveal(),
		RedirectURI:  cfg.GiteaPublicURL + "/user/oauth2/zerops/callback",
		MateAppURL:   cfg.MateAppURL,
		Seed:         cfg.OIDCSeed.Reveal(),
		Throwaway:    checker,
		Rights:       rightsFor,
		Log:          log,
		// Every token issued asks for a pass a few seconds later, so a person
		// is in the right teams by the time they have looked at the page.
		OnIssue: loop.Nudge,
	})
	if err != nil {
		return err
	}
	log.Info("the signing key is derived", "kid", provider.Key().KeyID())

	// The broker's own context: the loops run on it, and so does every queued
	// deploy — a deploy outlives the webhook or the request that asked for it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Since D27 the broker deploys nothing itself: a queued job is a workflow
	// it starts, whose own job runs `zcli push` on a key the broker grants.
	records := deploy.NewRecords(0)
	dispatcher := &deploy.Dispatcher{
		Zerops:   zeropsClient,
		Gitea:    giteaClient,
		Log:      log,
		ClientID: cfg.ZeropsClientID,
	}
	queue := deploy.NewQueue(ctx, dispatcher.Run, log)
	pipe := &pipeline.Pipeline{
		Zerops:         zeropsClient,
		Gitea:          giteaClient,
		Log:            log,
		ClientID:       cfg.ZeropsClientID,
		GiteaProjectID: cfg.ZeropsProjectID,
		// A Mate's recipe pull request is merged by the rights loop's next pass;
		// the hook that announces it asks for that pass now (D23).
		Nudge: loop.Nudge,
		Resolver: &deploy.Resolver{
			Gitea: giteaClient,
			Log:   log,
			Merger: &deploy.Merger{
				Gitea: giteaClient,
				Log:   log,
				// The admin token travels in the clone URL and nowhere else:
				// it is built for each clone and never stored.
				CloneURL: func(ctx context.Context, owner, repo string) (string, error) {
					creds, err := admin.Admin(ctx)
					if err != nil {
						return "", fmt.Errorf("the site admin's credentials: %w", err)
					}
					return cloneURL(cfg.GiteaURL, cfg.GiteaAdminUser, creds.Token, owner, repo), nil
				},
			},
		},
		Queue:        queue,
		Records:      records,
		RunnerImport: giteamate.RunnerImport,
		Base:         ctx,
	}
	deployLoop := &pipeline.Loop{Pipeline: pipe, Log: log}

	srv := server.New(cfg, log, server.Deps{
		Zerops:  zeropsClient,
		Gitea:   giteaClient,
		OIDC:    provider,
		Hooks:   pipe,
		Deploys: pipe,
		Runners: pipe,
		// A person's own Gitea access: proved the way the OIDC consent is,
		// placed by a pass before the answer.
		Throwaway: checker,
		Rights:    rightsFor,
		Pass: func(ctx context.Context) error {
			_, err := loop.RunNow(ctx)
			return err
		},
	})
	defer srv.Close()

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	loopDone := make(chan struct{})
	go func() { loop.Run(ctx); close(loopDone) }()

	// The deploy pass is a goroutine of its own: a deploy that takes a minute
	// must not hold up a permission change, and the reverse.
	deployDone := make(chan struct{})
	go func() { deployLoop.Run(ctx); close(deployDone) }()

	errs := make(chan error, 1)
	go func() {
		log.Info("broker listening", "addr", cfg.ListenAddr, "issuer", cfg.BrokerPublicURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	log.Info("broker shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = httpServer.Shutdown(shutdownCtx)
	<-loopDone
	<-deployDone
	// Let whatever is mid-deploy finish: an app version half uploaded is worse
	// than a container that took a few seconds longer to go.
	queue.Wait()
	return err
}

// cloneURL builds the URL a throwaway working copy is cloned from. The admin
// token is in it, so it is built at the moment of the clone and never stored,
// logged or returned.
func cloneURL(base, user, token, owner, repo string) string {
	rest, ok := strings.CutPrefix(base, "http://")
	scheme := "http://"
	if !ok {
		rest, ok = strings.CutPrefix(base, "https://")
		scheme = "https://"
	}
	if !ok {
		rest, scheme = base, "http://"
	}
	return scheme + url.UserPassword(user, token).String() + "@" + strings.TrimSuffix(rest, "/") + "/" + owner + "/" + repo
}
