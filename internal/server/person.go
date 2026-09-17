package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/throwaway"
)

// POST /person/token — a person's own Gitea access, for the Mate app.
//
// The app drives Gitea as the person (guide 4.4), and until now it got the
// token to do that from Gitea's own OAuth pages: a login page, the app's
// consent page, Gitea's grant page — four screens for somebody who had already
// signed in to Mate with their Zerops account. This route is the same proof
// the app makes at a Mate's door, and nothing else: a throwaway with no rights,
// minted as the person and deleted after the call, names them; the broker,
// which is Gitea's site admin, makes sure their account exists and mints them a
// token that acts as them. No screen, no click.
//
// The account is bound to the OIDC source at creation, so a person who ever
// opens Gitea's own pages and signs in with Zerops lands on the same account;
// and a pass runs before the route answers, so the account is in its teams by
// the time the app's first read arrives. The token's reach is exactly the
// person's: Gitea enforces the mirrored rights, the same as a token the person
// would have minted themselves.

// PersonProver proves a person from a throwaway — the six-step check of
// docs/broker-api.md.
type PersonProver interface {
	Check(ctx context.Context, bearer string) (throwaway.Caller, error)
}

// RightsReader answers what a proven person may do, read live.
type RightsReader interface {
	For(ctx context.Context, caller throwaway.Caller) (roles.Rights, error)
}

// PassFunc runs one pass of the rights loop now and returns when it is done.
type PassFunc func(ctx context.Context) error

// appTokenScopes is what the app does as the person: read and write the
// group's repositories (branches, contents, tags, Actions runs and reruns),
// read and write pull requests, read the org and the person. Nothing admin.
var appTokenScopes = []string{"read:user", "read:organization", "write:repository", "write:issue"}

// passTimeout bounds the pass a first sign-in waits for. Past it the route
// answers anyway; the next tick or nudge finishes the placement.
const passTimeout = 20 * time.Second

type personTokenBody struct {
	Token string `json:"token"`
	Login string `json:"login"`
	// ExpiresIn is how many seconds the token lives before the rights loop
	// retires it. The app re-mints on the first 401.
	ExpiresIn int `json:"expiresIn"`
}

func (s *Server) handlePersonToken(w http.ResponseWriter, r *http.Request) {
	s.appCORS(w)
	ctx := r.Context()

	caller, err := s.deps.Throwaway.Check(ctx, bearerOf(r))
	var refusal *throwaway.Refusal
	switch {
	case errors.As(err, &refusal):
		WriteErrorReason(w, http.StatusUnauthorized, throwaway.ErrorCode,
			"that token does not prove a person", refusal.Reason)
		return
	case err != nil:
		s.log.Error("the throwaway could not be checked", "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return
	}

	rights, err := s.deps.Rights.For(ctx, caller)
	if err != nil {
		s.log.Error("the caller's rights could not be read", "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return
	}
	if !rights.Active {
		WriteError(w, http.StatusForbidden, "not_a_member",
			"that account is not an active member of this organisation")
		return
	}

	login := roles.Login(caller.UserID)
	created, err := s.ensurePerson(ctx, login, caller)
	if err != nil {
		s.log.Error("the person's Gitea account could not be made true", "login", login, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "gitea", "Gitea could not be reached")
		return
	}
	if created && s.deps.Pass != nil {
		// The account is seconds old and in no team; a pass puts it where the
		// role function says, so the app's first read sees the group.
		passCtx, cancel := context.WithTimeout(ctx, passTimeout)
		if err := s.deps.Pass(passCtx); err != nil {
			s.log.Warn("the pass after a person's first sign-in did not finish", "login", login, "err", err.Error())
		}
		cancel()
	}

	name := mirror.AppTokenPrefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	token, err := s.deps.Gitea.MintToken(ctx, login, name, appTokenScopes)
	if err != nil {
		s.log.Error("a person's app token could not be minted", "login", login, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "gitea", "Gitea could not be reached")
		return
	}
	WriteJSON(w, http.StatusOK, personTokenBody{
		Token:     token.Value,
		Login:     login,
		ExpiresIn: int(s.cfg.AppTokenTTL.Seconds()),
	})
}

// ensurePerson makes the person's Gitea account exist, bound to the OIDC
// source under the id that source knows them by. It reports whether it had to
// create it.
func (s *Server) ensurePerson(ctx context.Context, login string, caller throwaway.Caller) (bool, error) {
	_, err := s.deps.Gitea.GetUser(ctx, login)
	switch {
	case err == nil:
		return false, nil
	case !gitea.IsNotFound(err):
		return false, err
	}
	email := strings.TrimSpace(caller.Member.User.Email)
	if email == "" {
		// Gitea insists on one; a person with none on Zerops gets an address
		// nobody delivers to.
		email = login + "@users.noreply.invalid"
	}
	_, err = s.deps.Gitea.CreateUser(ctx, gitea.NewUser{
		Login:     login,
		Email:     email,
		FullName:  caller.Member.User.FullName,
		SourceID:  s.cfg.GiteaOIDCSourceID,
		LoginName: caller.UserID,
	})
	return err == nil, err
}

func (s *Server) handlePersonTokenPreflight(w http.ResponseWriter, _ *http.Request) {
	s.appCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

// appCORS answers the app wherever it is served from: mate.zerops.io, a
// developer's localhost, the desktop and mobile shells (D22). The origin
// proves nothing here — the call carries a throwaway only the person's own
// Zerops session could have minted, and no cookie is involved — so a browser
// on any origin is told what it could have read with curl.
func (s *Server) appCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
}

func bearerOf(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}
