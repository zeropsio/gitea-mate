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
		writeUnavailable(w, "upstream", "Zerops could not be reached")
		return
	}

	rights, err := s.deps.Rights.For(ctx, caller)
	if err != nil {
		s.log.Error("the caller's rights could not be read", "err", err.Error())
		writeUnavailable(w, "upstream", "Zerops could not be reached")
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
		s.answerGiteaFailure(w, "the person's Gitea account could not be made true", login, err)
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
		s.answerGiteaFailure(w, "a person's app token could not be minted", login, err)
		return
	}
	WriteJSON(w, http.StatusOK, personTokenBody{
		Token:     token.Value,
		Login:     login,
		ExpiresIn: int(s.cfg.AppTokenTTL.Seconds()),
	})
}

// answerGiteaFailure tells Gitea saying no from Gitea being away. A 4xx is a
// refusal of what the broker asked — a login source that does not exist, a
// name it will not take — and is answered 424 with Gitea's own words, so the
// app can show them once: answered as "still setting up", it left the app
// retrying against a Gitea whose login source had never been added. Anything
// else is Gitea not answering, answered 503 (writeUnavailable), which the app
// reads as "still setting up" and asks again in a while.
func (s *Server) answerGiteaFailure(w http.ResponseWriter, what, login string, err error) {
	s.log.Error(what, "login", login, "err", err.Error())
	if status := gitea.Status(err); status >= 400 && status < 500 {
		// A 401 or 403 here is not a refusal of what the person asked for: every
		// call this path makes is the broker acting as the site admin, so those
		// two mean Gitea would not take the broker's OWN credential. Relaying
		// Gitea's words for them tells a person their sign-in was rejected —
		// which is how the run of 2026-09-19 presented an account whose Gitea
		// had published an admin token it then refused: the person saw "invalid
		// username, password or token" about credentials that were never theirs.
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			WriteError(w, http.StatusFailedDependency, "gitea_admin_refused",
				"This account's Gitea is not accepting its own administrator credentials. It mints a new pair the next time the Gitea service starts.")
			return
		}
		WriteError(w, http.StatusFailedDependency, "gitea_refused", "Gitea refused: "+giteaWords(err))
		return
	}
	writeUnavailable(w, "gitea", "Gitea could not be reached")
}

// unavailableRetryAfter is how many seconds the app is asked to wait before it
// asks again.
const unavailableRetryAfter = "5"

// writeUnavailable answers a dependency that did not answer: 503 with
// Retry-After, never 502. The platform's edge replaces an upstream 502 with
// its own HTML page and no CORS headers (measured 2026-09-17), so the browser
// saw a network error instead of the broker's words. The CORS headers are the
// ones every answer on this route carries (appCORS).
func writeUnavailable(w http.ResponseWriter, code, message string) {
	w.Header().Set("Retry-After", unavailableRetryAfter)
	WriteError(w, http.StatusServiceUnavailable, code, message)
}

// giteaWords is what Gitea said, without the broker's framing.
func giteaWords(err error) string {
	var apiErr *gitea.APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
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
