package oidc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/oidc"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/throwaway"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

const (
	org         = "org-1"
	giteaHost   = "git.example"
	issuer      = "https://broker.example"
	redirectURI = "https://git.example/user/oauth2/zerops/callback"
	appURL      = "https://app.example"
	secret      = "the-client-secret"
	seed        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

type rig struct {
	fake     *zeropstest.Fake
	provider *oidc.Provider
	handler  http.Handler
	issued   int
	now      time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := zeropstest.New(t, org)
	f.Now = now
	f.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: org})
	f.AddIdentity("throwaway", zeropstest.Identity{UserInfoID: "tok-throw", TokenID: "tok-throw", ClientID: org})
	f.AddToken(zerops.Token{
		ID: "tok-throw", Name: "gitea-signin:" + giteaHost + ":n0nce",
		RoleCode: "NO_ACCESS", CreatedByUser: "u-jan", Created: now.Add(-30 * time.Second),
	})
	f.AddMember(zerops.Member{
		ID: "cu-jan", UserID: "u-jan", Status: "ACTIVE", RoleCode: "READ_ONLY",
		User: zerops.UserLight{ID: "u-jan", Email: "jan@example", FullName: "Jan Novák"},
	})

	r := &rig{fake: f, now: now}
	provider, err := oidc.New(oidc.Config{
		Issuer: issuer, ClientSecret: secret, RedirectURI: redirectURI,
		MateAppURL: appURL, Seed: seed,
		Throwaway: &throwaway.Checker{
			Broker: f.Client("broker"), AsCaller: f.Client, ClientID: org, GiteaHost: giteaHost,
		},
		Rights: oidc.RightsFunc(func(ctx context.Context, caller throwaway.Caller) (roles.Rights, error) {
			return roles.Compute(
				roles.Person{ID: caller.UserID, OrgRole: roles.Role(caller.Member.RoleCode), Status: caller.Member.Status},
				map[string]roles.Role{"p-fen": roles.Owner},
				roles.Registry{Groups: []roles.Group{{ID: "g-acme", Slug: "acme", Projects: []roles.Project{
					{ID: "p-fen", Kind: roles.KindMate},
					{ID: "p-prod", Kind: roles.KindProduction},
				}}}},
			), nil
		}),
		Log:     slog.New(slog.DiscardHandler),
		Now:     func() time.Time { return r.now },
		OnIssue: func() { r.issued++ },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	provider.Routes(mux)
	r.provider, r.handler = provider, mux
	return r
}

func (r *rig) do(req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	r.handler.ServeHTTP(rr, req)
	return rr
}

// authorize walks the first leg and returns the rid the consent page is handed.
func (r *rig) authorize(t *testing.T, state, nonce string) string {
	t.Helper()
	q := url.Values{
		"client_id": {"gitea"}, "response_type": {"code"},
		"redirect_uri": {redirectURI}, "state": {state}, "nonce": {nonce},
		"scope": {"openid email profile groups"},
	}
	rr := r.do(httptest.NewRequest(http.MethodGet, "/oidc/authorize?"+q.Encode(), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("/oidc/authorize = %d, want 302", rr.Code)
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != appURL+"/gitea-signin" {
		t.Fatalf("the consent page is %s", got)
	}
	if loc.Query().Get("broker") != issuer {
		t.Errorf("broker = %q", loc.Query().Get("broker"))
	}
	rid := loc.Query().Get("rid")
	if rid == "" {
		t.Fatal("no rid")
	}
	return rid
}

func (r *rig) complete(t *testing.T, rid, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oidc/complete", strings.NewReader(`{"rid":"`+rid+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", appURL)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r.do(req)
}

// The whole flow, end to end, with the id_token verified against the JWKS the
// provider serves.
func TestSignInEndToEnd(t *testing.T) {
	r := newRig(t)
	rid := r.authorize(t, "the-state", "the-nonce")

	rr := r.complete(t, rid, "throwaway")
	if rr.Code != http.StatusOK {
		t.Fatalf("/oidc/complete = %d: %s", rr.Code, rr.Body)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != appURL {
		t.Errorf("CORS origin = %q", got)
	}
	var completed struct {
		Redirect string `json:"redirect"`
	}
	mustJSON(t, rr, &completed)

	loc, err := url.Parse(completed.Redirect)
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if strings.TrimSuffix(loc.Scheme+"://"+loc.Host+loc.Path, "/") != redirectURI {
		t.Fatalf("the redirect is %s, want %s", loc, redirectURI)
	}
	if loc.Query().Get("state") != "the-state" {
		t.Errorf("state = %q", loc.Query().Get("state"))
	}
	authCode := loc.Query().Get("code")
	if authCode == "" {
		t.Fatal("no code")
	}

	token := r.exchange(t, authCode, http.StatusOK)
	if token.TokenType != "Bearer" || token.ExpiresIn != 300 {
		t.Errorf("token = %+v", token)
	}
	if r.issued != 1 {
		t.Errorf("OnIssue ran %d times, want 1", r.issued)
	}

	claims := verifyAgainstJWKS(t, r, token.IDToken)
	for field, want := range map[string]any{
		"iss":                issuer,
		"aud":                "gitea",
		"sub":                "u-jan",
		"nonce":              "the-nonce",
		"email":              "jan@example",
		"name":               "Jan Novák",
		"preferred_username": "u-ujan",
	} {
		if claims[field] != want {
			t.Errorf("%s = %v, want %v", field, claims[field], want)
		}
	}
	// Jan is READ_ONLY in the org and OWNER of the Mate: reads and writes the
	// group, does not release.
	groups := toStrings(claims["groups"])
	if strings.Join(groups, ",") != "g:acme:read,g:acme:write" {
		t.Errorf("groups = %v", groups)
	}
	if iat, exp := claims["iat"].(float64), claims["exp"].(float64); exp <= iat {
		t.Errorf("iat %v, exp %v", iat, exp)
	}

	// /oidc/userinfo answers the same claims.
	req := httptest.NewRequest(http.MethodGet, "/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	rr = r.do(req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/oidc/userinfo = %d: %s", rr.Code, rr.Body)
	}
	var info oidc.Claims
	mustJSON(t, rr, &info)
	if info.Subject != "u-jan" || info.PreferredUsername != "u-ujan" || strings.Join(info.Groups, ",") != "g:acme:read,g:acme:write" {
		t.Errorf("userinfo = %+v", info)
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IDToken     string `json:"id_token"`
}

func (r *rig) exchange(t *testing.T, authCode string, wantStatus int) tokenResponse {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "redirect_uri": {redirectURI}}
	req := httptest.NewRequest(http.MethodPost, "/oidc/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("gitea", secret)
	rr := r.do(req)
	if rr.Code != wantStatus {
		t.Fatalf("/oidc/token = %d, want %d: %s", rr.Code, wantStatus, rr.Body)
	}
	var out tokenResponse
	if wantStatus == http.StatusOK {
		mustJSON(t, rr, &out)
	}
	return out
}

func TestClientSecretPostIsAccepted(t *testing.T) {
	r := newRig(t)
	rid := r.authorize(t, "s", "n")
	authCode := codeFrom(t, r.complete(t, rid, "throwaway"))

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {authCode},
		"client_id": {"gitea"}, "client_secret": {secret},
	}
	req := httptest.NewRequest(http.MethodPost, "/oidc/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rr := r.do(req); rr.Code != http.StatusOK {
		t.Fatalf("client_secret_post = %d: %s", rr.Code, rr.Body)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name       string
		run        func(t *testing.T, r *rig) *httptest.ResponseRecorder
		wantStatus int
	}{
		{
			name:       "a code cannot be spent twice",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				rid := r.authorize(t, "s", "n")
				authCode := codeFrom(t, r.complete(t, rid, "throwaway"))
				r.exchange(t, authCode, http.StatusOK)
				return r.postToken(t, url.Values{
					"grant_type": {"authorization_code"}, "code": {authCode},
				}, true)
			},
		},
		{
			name:       "an unknown code",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				return r.postToken(t, url.Values{"grant_type": {"authorization_code"}, "code": {"nope"}}, true)
			},
		},
		{
			name:       "the wrong client secret",
			wantStatus: http.StatusUnauthorized,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				rid := r.authorize(t, "s", "n")
				authCode := codeFrom(t, r.complete(t, rid, "throwaway"))
				form := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}}
				req := httptest.NewRequest(http.MethodPost, "/oidc/token", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.SetBasicAuth("gitea", "not-the-secret")
				return r.do(req)
			},
		},
		{
			name:       "no client authentication at all",
			wantStatus: http.StatusUnauthorized,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				return r.postToken(t, url.Values{"grant_type": {"authorization_code"}, "code": {"x"}}, false)
			},
		},
		{
			name:       "a grant type nobody serves",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				return r.postToken(t, url.Values{"grant_type": {"client_credentials"}}, true)
			},
		},
		{
			name:       "a redirect_uri on the exchange that is not the registered one",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				rid := r.authorize(t, "s", "n")
				authCode := codeFrom(t, r.complete(t, rid, "throwaway"))
				return r.postToken(t, url.Values{
					"grant_type": {"authorization_code"}, "code": {authCode},
					"redirect_uri": {"https://evil.example/callback"},
				}, true)
			},
		},
		{
			name:       "an unknown client_id at /oidc/authorize",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				q := url.Values{"client_id": {"someone-else"}, "response_type": {"code"}, "redirect_uri": {redirectURI}}
				return r.do(httptest.NewRequest(http.MethodGet, "/oidc/authorize?"+q.Encode(), nil))
			},
		},
		{
			name:       "an unregistered redirect_uri at /oidc/authorize",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				q := url.Values{"client_id": {"gitea"}, "response_type": {"code"}, "redirect_uri": {"https://evil.example/cb"}}
				return r.do(httptest.NewRequest(http.MethodGet, "/oidc/authorize?"+q.Encode(), nil))
			},
		},
		{
			name:       "a response_type nobody serves",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				q := url.Values{"client_id": {"gitea"}, "response_type": {"token"}, "redirect_uri": {redirectURI}}
				return r.do(httptest.NewRequest(http.MethodGet, "/oidc/authorize?"+q.Encode(), nil))
			},
		},
		{
			name:       "an rid nobody issued",
			wantStatus: http.StatusBadRequest,
			run:        func(t *testing.T, r *rig) *httptest.ResponseRecorder { return r.complete(t, "made-up", "throwaway") },
		},
		{
			name:       "an rid that expired",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				rid := r.authorize(t, "s", "n")
				// The clock moves past the request's ten minutes; the throwaway
				// itself is re-minted, so what expires is the sign-in and
				// nothing else.
				r.now = r.now.Add(oidc.RequestTTL + time.Minute)
				r.fake.Now = r.now
				r.fake.AddToken(zerops.Token{
					ID: "tok-throw", Name: "gitea-signin:" + giteaHost + ":n0nce",
					RoleCode: "NO_ACCESS", CreatedByUser: "u-jan", Created: r.now.Add(-30 * time.Second),
				})
				return r.complete(t, rid, "throwaway")
			},
		},
		{
			name:       "an rid consumed twice",
			wantStatus: http.StatusBadRequest,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				rid := r.authorize(t, "s", "n")
				if rr := r.complete(t, rid, "throwaway"); rr.Code != http.StatusOK {
					t.Fatalf("the first consent = %d", rr.Code)
				}
				return r.complete(t, rid, "throwaway")
			},
		},
		{
			name:       "a consent with no throwaway",
			wantStatus: http.StatusUnauthorized,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				return r.complete(t, r.authorize(t, "s", "n"), "")
			},
		},
		{
			name:       "an access token nobody issued",
			wantStatus: http.StatusUnauthorized,
			run: func(t *testing.T, r *rig) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/oidc/userinfo", nil)
				req.Header.Set("Authorization", "Bearer made-up")
				return r.do(req)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			rr := tc.run(t, r)
			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body)
			}
		})
	}
}

func (r *rig) postToken(t *testing.T, form url.Values, authenticate bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oidc/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authenticate {
		req.SetBasicAuth("gitea", secret)
	}
	return r.do(req)
}

// A refused throwaway comes back with the reason the contract names, so the
// consent page can say what went wrong.
func TestRefusedThrowawayCarriesItsReason(t *testing.T) {
	r := newRig(t)
	r.fake.AddIdentity("stranger", zeropstest.Identity{UserInfoID: "tok-x", TokenID: "tok-x", ClientID: "org-2"})
	rr := r.complete(t, r.authorize(t, "s", "n"), "stranger")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
	var body map[string]string
	mustJSON(t, rr, &body)
	if body["error"] != "throwaway_invalid" || body["reason"] != throwaway.ReasonWrongOrg {
		t.Errorf("body = %v", body)
	}
}

// An access token lives five minutes, and the store forgets it after.
func TestAccessTokenExpires(t *testing.T) {
	r := newRig(t)
	rid := r.authorize(t, "s", "n")
	token := r.exchange(t, codeFrom(t, r.complete(t, rid, "throwaway")), http.StatusOK)

	r.now = r.now.Add(oidc.AccessTokenTTL + time.Second)
	req := httptest.NewRequest(http.MethodGet, "/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	if rr := r.do(req); rr.Code != http.StatusUnauthorized {
		t.Errorf("an expired access token = %d, want 401", rr.Code)
	}
}

// CORS answers the Mate app's origin and nobody else's — a runner in the same
// project can reach this port.
func TestCORSIsTheMateAppOnly(t *testing.T) {
	r := newRig(t)
	for _, origin := range []string{"https://evil.example", "http://app.example", ""} {
		req := httptest.NewRequest(http.MethodOptions, "/oidc/complete", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := r.do(req)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q got Access-Control-Allow-Origin %q", origin, got)
		}
	}

	req := httptest.NewRequest(http.MethodOptions, "/oidc/complete", nil)
	req.Header.Set("Origin", appURL)
	rr := r.do(req)
	if rr.Header().Get("Access-Control-Allow-Origin") != appURL {
		t.Errorf("the Mate app's own origin was refused: %v", rr.Header())
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("Authorization is not an allowed header: %q", rr.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestDiscovery(t *testing.T) {
	r := newRig(t)
	rr := r.do(httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var doc map[string]any
	mustJSON(t, rr, &doc)
	for field, want := range map[string]string{
		"issuer":                 issuer,
		"authorization_endpoint": issuer + "/oidc/authorize",
		"token_endpoint":         issuer + "/oidc/token",
		"userinfo_endpoint":      issuer + "/oidc/userinfo",
		"jwks_uri":               issuer + "/oidc/jwks",
	} {
		if doc[field] != want {
			t.Errorf("%s = %v, want %v", field, doc[field], want)
		}
	}
	if got := toStrings(doc["id_token_signing_alg_values_supported"]); strings.Join(got, ",") != "ES256" {
		t.Errorf("algorithms = %v", got)
	}
	if got := toStrings(doc["scopes_supported"]); strings.Join(got, " ") != "openid email profile groups" {
		t.Errorf("scopes = %v", got)
	}
	if got := toStrings(doc["claims_supported"]); strings.Join(got, " ") != "sub email name preferred_username groups" {
		t.Errorf("claims = %v", got)
	}
}

// The key is derived, not stored: the same seed always gives the same key, and
// a different seed a different one.
func TestKeyIsDeterministic(t *testing.T) {
	a, err := oidc.DeriveKey(seed)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	b, err := oidc.DeriveKey(seed)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if a.KeyID() != b.KeyID() {
		t.Errorf("two derivations of one seed differ: %s %s", a.KeyID(), b.KeyID())
	}
	c, err := oidc.DeriveKey(seed + "x")
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if a.KeyID() == c.KeyID() {
		t.Error("two seeds gave one key")
	}
	if len(a.KeyID()) != 8 {
		t.Errorf("kid = %q, want eight hex characters", a.KeyID())
	}
	if _, err := oidc.DeriveKey(""); err == nil {
		t.Error("an empty seed made a key")
	}

	// The kid is the public point's digest, never the seed's: a JWKS must not
	// publish a prefix of the private scalar.
	seedDigest := sha256.Sum256([]byte(seed))
	if strings.HasPrefix(hexOf(seedDigest[:]), a.KeyID()) {
		t.Error("the kid is a prefix of the seed's digest, which is the private scalar")
	}
}

// verifyAgainstJWKS checks the signature with the public key the provider
// itself publishes, and returns the claims.
func verifyAgainstJWKS(t *testing.T, r *rig, idToken string) map[string]any {
	t.Helper()
	rr := r.do(httptest.NewRequest(http.MethodGet, "/oidc/jwks", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/oidc/jwks = %d", rr.Code)
	}
	var jwks struct {
		Keys []struct {
			Kty, Crv, Alg, Kid, X, Y string
		} `json:"keys"`
	}
	mustJSON(t, rr, &jwks)
	if len(jwks.Keys) != 1 {
		t.Fatalf("jwks carries %d keys", len(jwks.Keys))
	}
	k := jwks.Keys[0]
	if k.Kty != "EC" || k.Crv != "P-256" || k.Alg != "ES256" {
		t.Fatalf("jwk = %+v", k)
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		t.Fatalf("the id_token has %d parts", len(parts))
	}
	var header struct{ Alg, Typ, Kid string }
	if err := json.Unmarshal(decode(t, parts[0]), &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	if header.Alg != "ES256" || header.Kid != k.Kid {
		t.Errorf("header = %+v, jwks kid %s", header, k.Kid)
	}

	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(decode(t, k.X)),
		Y:     new(big.Int).SetBytes(decode(t, k.Y)),
	}
	sig := decode(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("the signature is %d bytes, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the id_token does not verify against the published JWKS")
	}

	var claims map[string]any
	if err := json.Unmarshal(decode(t, parts[1]), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	return claims
}

func codeFrom(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("/oidc/complete = %d: %s", rr.Code, rr.Body)
	}
	var body struct {
		Redirect string `json:"redirect"`
	}
	mustJSON(t, rr, &body)
	u, err := url.Parse(body.Redirect)
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	return u.Query().Get("code")
}

func mustJSON(t *testing.T, rr *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), into); err != nil {
		t.Fatalf("body %q: %v", rr.Body.String(), err)
	}
}

func decode(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64url %q: %v", s, err)
	}
	return raw
}

func toStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

func hexOf(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(raw)*2)
	for _, b := range raw {
		out = append(out, digits[b>>4], digits[b&0xf])
	}
	return string(out)
}
