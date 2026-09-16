// Package oidc is the OIDC provider Gitea signs people in through
// (docs/broker-api.md, "OIDC provider", and guide 3.6).
//
// One client, one redirect URI, one key — derived from OIDC_SEED so a restart
// signs the same way. Everything else lives in memory for minutes: a restart
// forgets requests, codes and access tokens, and the person signs in again.
//
// The step that makes it safe is the consent: the broker never sees a person's
// Zerops token. The Mate app mints a throwaway named for this Gitea, posts it
// to /oidc/complete, and deletes it seconds later.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/throwaway"
)

// ClientID is the one client: Gitea's `zerops` login source.
const ClientID = "gitea"

// Claims are what an id_token and /oidc/userinfo carry.
type Claims struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"email,omitempty"`
	Name              string   `json:"name,omitempty"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
}

// Rights answers what a person may do, read live from Zerops at the moment
// they consent. The server wires it to the role function over the org's member
// list, projects and registry.
type Rights interface {
	For(ctx context.Context, caller throwaway.Caller) (roles.Rights, error)
}

// RightsFunc adapts a function to [Rights].
type RightsFunc func(ctx context.Context, caller throwaway.Caller) (roles.Rights, error)

// For calls f.
func (f RightsFunc) For(ctx context.Context, caller throwaway.Caller) (roles.Rights, error) {
	return f(ctx, caller)
}

// Config is what a provider needs.
type Config struct {
	// Issuer is BROKER_PUBLIC_URL.
	Issuer string
	// ClientSecret is OIDC_CLIENT_SECRET, the secret Gitea's source holds.
	ClientSecret string
	// RedirectURI is the one allowed: {GITEA_PUBLIC_URL}/user/oauth2/zerops/callback.
	RedirectURI string
	// MateAppURL is where the consent page lives, and the only origin
	// /oidc/complete answers CORS for.
	MateAppURL string
	// Seed is OIDC_SEED.
	Seed string

	Throwaway *throwaway.Checker
	Rights    Rights
	Log       *slog.Logger

	// Now is injectable; nil means time.Now.
	Now func() time.Time
	// OnIssue runs after a token is issued, so the rights loop can make a pass
	// a few seconds later and have the person in their teams by the time they
	// see the page.
	OnIssue func()
}

// Provider serves the OIDC routes.
type Provider struct {
	cfg   Config
	key   *Key
	store *store
}

// New builds a provider and derives its key.
func New(cfg Config) (*Provider, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	key, err := DeriveKey(cfg.Seed)
	if err != nil {
		return nil, err
	}
	return &Provider{cfg: cfg, key: key, store: newStore(cfg.Now)}, nil
}

// Key is the signing key, for whoever needs its kid.
func (p *Provider) Key() *Key { return p.key }

// Routes registers every OIDC route on mux.
func (p *Provider) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("GET /oidc/jwks", p.handleJWKS)
	mux.HandleFunc("GET /oidc/authorize", p.handleAuthorize)
	mux.HandleFunc("POST /oidc/complete", p.handleComplete)
	mux.HandleFunc("OPTIONS /oidc/complete", p.handleCompletePreflight)
	mux.HandleFunc("POST /oidc/token", p.handleToken)
	mux.HandleFunc("GET /oidc/userinfo", p.handleUserinfo)
}

// ---------------------------------------------------------------------------
// Discovery and keys
// ---------------------------------------------------------------------------

func (p *Provider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.cfg.Issuer,
		"authorization_endpoint":                p.cfg.Issuer + "/oidc/authorize",
		"token_endpoint":                        p.cfg.Issuer + "/oidc/token",
		"userinfo_endpoint":                     p.cfg.Issuer + "/oidc/userinfo",
		"jwks_uri":                              p.cfg.Issuer + "/oidc/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"scopes_supported":                      []string{"openid", "email", "profile", "groups"},
		"claims_supported":                      []string{"sub", "email", "name", "preferred_username", "groups"},
		"grant_types_supported":                 []string{"authorization_code"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	})
}

func (p *Provider) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.key.JWKS())
}

// ---------------------------------------------------------------------------
// /oidc/authorize
// ---------------------------------------------------------------------------

func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID {
		writeError(w, http.StatusBadRequest, "invalid_client", "this issuer serves one client")
		return
	}
	if q.Get("response_type") != "code" {
		writeError(w, http.StatusBadRequest, "unsupported_response_type", "only the code flow is served")
		return
	}
	if q.Get("redirect_uri") != p.cfg.RedirectURI {
		// Never redirect to an unregistered URI, not even to report the error.
		writeError(w, http.StatusBadRequest, "invalid_request", "that redirect_uri is not registered")
		return
	}

	rid, err := randomID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "the request could not be stored")
		return
	}
	p.store.putRequest(rid, request{
		RedirectURI: q.Get("redirect_uri"),
		State:       q.Get("state"),
		Nonce:       q.Get("nonce"),
		Scope:       q.Get("scope"),
		Expires:     p.cfg.Now().Add(RequestTTL),
	})

	consent := p.cfg.MateAppURL + "/gitea-signin?" + url.Values{
		"rid":    {rid},
		"broker": {p.cfg.Issuer},
	}.Encode()
	http.Redirect(w, r, consent, http.StatusFound)
}

// ---------------------------------------------------------------------------
// /oidc/complete
// ---------------------------------------------------------------------------

func (p *Provider) handleCompletePreflight(w http.ResponseWriter, r *http.Request) {
	p.cors(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// cors allows the Mate app's origin and nothing else. A runner in the same
// project can reach this port, so the origin is checked literally.
func (p *Provider) cors(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != p.cfg.MateAppURL {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", p.cfg.MateAppURL)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Vary", "Origin")
}

func (p *Provider) handleComplete(w http.ResponseWriter, r *http.Request) {
	p.cors(w, r)

	var body struct {
		RID string `json:"rid"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.RID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "the body is {\"rid\": \"…\"}")
		return
	}

	caller, err := p.cfg.Throwaway.Check(r.Context(), bearer(r))
	var refusal *throwaway.Refusal
	switch {
	case errors.As(err, &refusal):
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   throwaway.ErrorCode,
			"message": "that token does not prove a person",
			"reason":  refusal.Reason,
		})
		return
	case err != nil:
		p.cfg.Log.Error("the throwaway could not be checked", "err", err.Error())
		writeError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return
	}

	req, ok := p.store.takeRequest(body.RID)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "that sign-in has expired; start again from Gitea")
		return
	}

	rights, err := p.cfg.Rights.For(r.Context(), caller)
	if err != nil {
		p.cfg.Log.Error("the caller's rights could not be read", "err", err.Error())
		writeError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return
	}
	if !rights.Active {
		writeError(w, http.StatusForbidden, "not_a_member", "that account is not an active member of this organisation")
		return
	}

	claims := Claims{
		Subject:           caller.UserID,
		Email:             caller.Member.User.Email,
		Name:              caller.Member.User.FullName,
		PreferredUsername: roles.Login(caller.UserID),
		Groups:            rights.Claims,
	}

	value, err := randomID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "the code could not be issued")
		return
	}
	p.store.putCode(value, code{Claims: claims, Nonce: req.Nonce, Expires: p.cfg.Now().Add(CodeTTL)})

	redirect := req.RedirectURI + "?" + url.Values{"code": {value}, "state": {req.State}}.Encode()
	writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

// ---------------------------------------------------------------------------
// /oidc/token
// ---------------------------------------------------------------------------

func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the body is a form")
		return
	}
	if !p.authenticateClient(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="broker"`)
		writeError(w, http.StatusUnauthorized, "invalid_client", "the client could not be authenticated")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is served")
		return
	}
	if uri := r.PostForm.Get("redirect_uri"); uri != "" && uri != p.cfg.RedirectURI {
		writeError(w, http.StatusBadRequest, "invalid_grant", "that redirect_uri is not the one the code was issued for")
		return
	}

	c, ok := p.store.takeCode(r.PostForm.Get("code"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_grant", "that code is unknown, spent or expired")
		return
	}

	now := p.cfg.Now()
	payload := map[string]any{
		"iss":                p.cfg.Issuer,
		"aud":                ClientID,
		"sub":                c.Claims.Subject,
		"iat":                now.Unix(),
		"exp":                now.Add(CodeTTL).Unix(),
		"email":              c.Claims.Email,
		"name":               c.Claims.Name,
		"preferred_username": c.Claims.PreferredUsername,
		"groups":             c.Claims.Groups,
	}
	if c.Nonce != "" {
		payload["nonce"] = c.Nonce
	}
	idToken, err := p.key.Sign(payload)
	if err != nil {
		p.cfg.Log.Error("the id_token could not be signed", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "server_error", "the id_token could not be signed")
		return
	}

	accessToken, err := randomID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "the access token could not be issued")
		return
	}
	p.store.putAccess(accessToken, access{Claims: c.Claims, Expires: now.Add(AccessTokenTTL)})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(AccessTokenTTL.Seconds()),
		"id_token":     idToken,
		"scope":        "openid email profile groups",
	})

	// The teams are written by the rights loop, which runs a pass a few seconds
	// from now — so the person is in the right teams by the time they see the
	// page.
	if p.cfg.OnIssue != nil {
		p.cfg.OnIssue()
	}
}

// authenticateClient takes client_secret_basic or client_secret_post.
func (p *Provider) authenticateClient(r *http.Request) bool {
	if id, secret, ok := r.BasicAuth(); ok {
		return id == ClientID && constantEquals(secret, p.cfg.ClientSecret)
	}
	return r.PostForm.Get("client_id") == ClientID &&
		constantEquals(r.PostForm.Get("client_secret"), p.cfg.ClientSecret)
}

// ---------------------------------------------------------------------------
// /oidc/userinfo
// ---------------------------------------------------------------------------

func (p *Provider) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	a, ok := p.store.readAccess(bearer(r))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "that access token is unknown or expired")
		return
	}
	writeJSON(w, http.StatusOK, a.Claims)
}

// ---------------------------------------------------------------------------

func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"server_error","message":"the answer could not be encoded"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// constantEquals compares without leaking the length-prefix of a secret through
// timing.
func constantEquals(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
