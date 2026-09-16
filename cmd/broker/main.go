// Command broker is the Zerops ↔ Gitea broker: it mirrors Zerops permissions
// into a Gitea instance, signs people in to Gitea and hands Mates their Gitea
// access.
//
// Its API is docs/broker-api.md; every name it uses is in docs/vocabulary.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zeropsio/gitea-mate/internal/config"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/oidc"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/server"
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
	giteaClient := gitea.New(gitea.Config{
		BaseURL:       cfg.GiteaURL,
		AdminToken:    cfg.GiteaAdminToken.Reveal(),
		AdminUser:     cfg.GiteaAdminUser,
		AdminPassword: cfg.GiteaAdminPassword.Reveal(),
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
		Cap:            cfg.MirrorCap,
	}
	rights.SetHookSecret(cfg.GiteaWebhookSecret.Reveal())
	loop := mirror.NewLoop(rights, log, cfg.MirrorInterval, mirror.DefaultNudgeDelay)

	provider, err := oidc.New(oidc.Config{
		Issuer:       cfg.BrokerPublicURL,
		ClientSecret: cfg.OIDCClientSecret.Reveal(),
		RedirectURI:  cfg.GiteaPublicURL + "/user/oauth2/zerops/callback",
		MateAppURL:   cfg.MateAppURL,
		Seed:         cfg.OIDCSeed.Reveal(),
		Throwaway:    checker,
		Rights: oidc.RightsFunc(func(ctx context.Context, caller throwaway.Caller) (roles.Rights, error) {
			org, err := mirror.ReadOrg(ctx, zeropsClient, cfg.ZeropsClientID, cfg.ZeropsProjectID)
			if err != nil {
				return roles.Rights{}, err
			}
			computed, found := org.RightsFor(caller.UserID)
			if !found {
				return roles.Rights{}, fmt.Errorf("the org's member list does not carry %s", caller.UserID)
			}
			return computed, nil
		}),
		Log: log,
		// Every token issued asks for a pass a few seconds later, so a person
		// is in the right teams by the time they have looked at the page.
		OnIssue: loop.Nudge,
	})
	if err != nil {
		return err
	}
	log.Info("the signing key is derived", "kid", provider.Key().KeyID())

	srv := server.New(cfg, log, server.Deps{
		Zerops:    zeropsClient,
		Gitea:     giteaClient,
		Throwaway: checker,
		OIDC:      provider,
		// The next brief fills these in: deploys, release approval, recipe
		// deltas.
		Hooks: server.NoopHooks{},
	})
	defer srv.Close()

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loopDone := make(chan struct{})
	go func() { loop.Run(ctx); close(loopDone) }()

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
	return err
}
